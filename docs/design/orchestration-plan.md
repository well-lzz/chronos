# 任务编排优化：设计计划

> **状态（2026-10-30 核对）**：本方案已实现：租约协调、独立验证、断点恢复、会话接力与安静周期终止见提交 `c7f7122`。 本文保留设计取舍记录，实现细节以代码为准。

> 设计基线文档（纯设计，不含实现）。承接 `docs/mission-modes.md` 的「任务=搜索」与现有 `scheduler` / `swarm` / `poller` / `termination` / `blackboard` 内核；与 `docs/design/subagent-delegation.md`（纵向上下文外包）正交互补——本文件谈的是**横向编排**：多执行单元如何分工、协调、验证、终止、续跑。

---

## 0. 前提：优化必须守住 Chronos 的极简身份

Chronos 刻意做减法：`0 Skill / 0 RAG / 0 MCP / 无任务角色分解 / 无预设攻击流程 / 机械调度器（无 LLM）`。业界不少「标准」编排做法对 Chronos 恰恰是**反模式**。所以每条优化先过一关：**不破坏极简身份**。

**红线（明确不做）**

- **不加 routing / 专门化 worker**（侦察-agent、Web-agent 这类分类路由）——直接违反 `无角色分解`，是 Chronos 刻意拒绝的。保持「对等中性 executor」。
- **不把机械调度器变成 LLM 调度器**——scheduler 的价值是确定性、可预测；编排的「智能」留在 executor / 子 Agent 层，不上移到调度器。
- **不默认开竞速**——swarm 是**协作不竞速**，这是与 Claude 树状委派的核心差异点；下面的协调机制要**强化协作**，不能退回竞速。

---

## 1. 调研依据（优质编排思路浓缩）

**Anthropic 五模式**（workflow 比全自主 agent 更可控）

- prompt chaining（固定串行，每步吃上一步输出）
- routing（分类 → 专门处理）
- parallelization：**sectioning**（拆独立子任务）/ **voting**（同任务多跑取共识）
- orchestrator-workers（中枢**运行时**决定子任务，派发、合并）
- evaluator-optimizer（生成者 + 独立评判者按标准反馈回修）

**Cognition 两条铁律**（反对盲目多智能体并行）

1. **共享上下文**。
2. **动作隐含决策，冲突的决策产生坏结果**。→ 读任务可并行；**写任务危险**，并行写要么共享上下文合并，要么别并行。偏好单线程线性；上下文过长用专门压缩模型。

**LangChain 综合**

- 读/写二分：研究类并行安全，改状态类要串行。
- 适合多智能体：广度优先、多独立方向、信息超单窗口；不适合：所有 agent 需同一上下文、强相互依赖。
- **durable execution**：从失败点 resume，而非重来；全链路 tracing；小样本（~20）LLM-as-judge 评测。

---

## 2. 对照 Chronos 现状（gap 分析）

| 优质思路 | Chronos 现状 | gap / 机会 |
|---|---|---|
| sectioning vs voting | swarm = 同一 mission 跑 N 个 executor，多样性只来自**模型不同** → 实为 **voting 式** | executor 大面积**重叠搜索**，无按子目标/面的分工覆盖 |
| Cognition②：冲突动作 | N 个 executor 对**同一有状态靶场**并行用会改状态的工具（POST/tcp/run_python），**零协调** | **并行写冲突**：一个改了目标状态，另一个假设即崩——最大编排隐患 |
| evaluator-optimizer | `verify_finding` 是**发现者自验**（确认偏置） | 缺**独立评判**，误报率高 |
| durable execution | 黑板落盘持久，但**执行态**（executor 窗口、调度窗口、deferred 队列）崩溃即丢 | 重启是**重来**不是**续跑** |
| orchestrator 近上限摘要+干净接手 | 有 compaction（有损），无「换干净 executor 接手」 | 长 engagement 单 executor 窗口饱和只能压缩，不能**接力** |
| 覆盖/收益递减终止 | `ShouldTerminate` 默认**永不**，只有 StallPolicy 的 pivot/terminate | engagement 缺**理性停止**，只能烧墙钟 |
| routing | 调度器按分值→难度填窗口，无分类路由 | **设计红线，非 gap**（见 §0） |

---

## 3. 优化提案（按优先级）

### P0 — 并行写的冲突协调（补 Cognition②，最高价值）

**问题**：swarm 多 executor 对同一有状态靶场并行改状态，无协调；一个 executor 的 mutating 动作会让兄弟的假设失效，产生 Cognition 所说的「冲突决策 → 坏结果」。

**思路（黑板认领 / 租约，纯结构性）**

- 给黑板加**认领 / 租约**原语：executor 认领一个 sub-goal / 一块「面」（host:port、端点族），带 **TTL**；
- 兄弟见到某面已被认领 → 不重复、不在同一面上做冲突的 mutating 动作，转去未认领的面；
- 租约在 resolve / 过期 / executor 退出时释放；
- 认领信息进 `bb.Summary()` 重注入，让兄弟实时看见「谁在做什么」。

**收益**：既消除并行写冲突，又把 voting 式**重叠搜索**推向 sectioning 式**覆盖分工**，每 token 覆盖更多面。

**与哲学一致**：认领是**结构性协调**，不是角色分工——executor 仍是对等中性个体，只是「别撞车」。强化协作，不引入竞速。

### P1 — 独立验证（补 evaluator-optimizer）

**问题**：`verify_finding` 由发现者自己做，有确认偏置，误报进报告。

**思路**

- candidate 产生的「验证子目标」**优先派给另一个 executor / 另一个模型**（swarm 本就多模型），形成「生成者 ≠ 评判者」；
- 验证仍走现有 candidate → verified/refuted/inconclusive 生命周期，**只改「谁来验」，不改语义**；
- 评判者引用**真实流量** `evidence_refs` 做判决，与现有证据模型一致。

**收益**：显著降误报；天然利用 swarm 的多模型多样性做交叉验证。

**与哲学一致**：复用既有工具与生命周期，无新角色；只是把验证动作**换个执行者**。

### P1 — mission / scheduler 级 durable resume

**问题**：崩溃即重来；只有黑板幸存，执行态全丢。

**思路**

- 把 `docs/design/subagent-delegation.md` §7 的 `CHRONOS_STATE_DIR` 持久化**上提**到：
  - 各 executor 的**消息窗口**（含 compaction 后状态）；
  - **调度器状态**：并发窗口、deferred 队列 + 冷却/次数、各 swarm 进度游标；
- 重启**从断点续**，而非重跑；
- 与子 Agent 持久化**同一套机制**，一致推进。

**收益**：长 engagement / 长 benchmark 的稳健性；省掉崩溃重来的预算浪费。

### P2 — 饱和接力（orchestrator 换人）

**问题**：长 engagement 单 executor 窗口饱和时，只能有损压缩，推理质量下降。

**思路**

- 当某 executor 窗口**持续逼近上限且压缩收益递减**，起一个**干净 executor**，用黑板摘要做 **handoff** 接手该 executor 的未决 sub-goal；
- 旧 executor 退出、释放其租约（见 P0）。

**定位**：事前预防的编排动作，与 compaction（事后有损）互补，与子 Agent 委派（纵向外包）正交——这是**横向换人**。

### P2 — 覆盖 / 收益递减终止

**问题**：`ShouldTerminate` 默认永不，engagement 只能烧墙钟。

**思路**：给 engagement 落一个真实谓词（mission-modes.md §7.3 本就设想过）——**所有 open sub-goal 已解 + 连续 N cycle 无新 Fact/Finding + 在预算内** → 理性停止。candidate 仍有未决验证子目标时不算完。

**收益**：engagement 停得理性而非耗尽预算；与 P0 的认领 / P1 的独立验证组合后，「覆盖度」有了可度量的依据。

### P3 — 编排决策可观测

**问题**：调度器 / swarm 的决策（defer、pivot、kill、认领/释放租约、handoff）当前多为日志或静默。

**思路**：和子 Agent 委派树一致，做成**结构化事件**，在控制台可见；供操作员理解「现在谁在做什么、为什么」，也为调参（并发、租约 TTL、终止阈值）提供数据。

---

## 4. 落地顺序建议

1. **黑板认领 / 租约（P0）** + 把认领纳入 `Summary()` 重注入——编排协调的地基，直接消除最大隐患。
   - 依赖 `docs/design/subagent-delegation.md` §6 的**黑板修订号**（认领/释放即修订事件），建议与之合并实现。
2. **独立验证（P1）**——在认领机制上，把验证子目标派给非发现者。
3. **durable resume（P1）**——与子 Agent 持久化同批推进。
4. **编排可观测（P3）**——与上面功能同批上，别后补。
5. **饱和接力（P2）** / **覆盖终止（P2）**——稳健性/长程能力加分项，验证前几项稳定后再做。

---

## 5. 与其它设计文件的关系

- **`docs/design/subagent-delegation.md`**：纵向——一个 executor 把支线深挖外包进隔离子窗口，只回传结论。
- **本文件**：横向——多个对等 executor 如何分工（认领）、协调（不冲突写）、交叉验证、续跑、理性终止。
- **黑板**：两条轴共同的协调媒介；**黑板修订号**是两边都要的前置基建，应一次建成。
- **mission-modes.md**：任务=搜索的抽象来源；本文件的覆盖终止即其 §7.3 的落地。
