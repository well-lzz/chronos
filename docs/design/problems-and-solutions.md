# Chronos Harness 优化:问题与解决方案汇总

> **状态（2026-10-30 核对）**：表中 ✅/📝/🔎 是 2026-10-27 时的状态。截至 2026-10-30，表内全部问题均已在 `main` 实现，见 [设计方案目录](README.md)。 本文保留设计取舍记录，实现细节以代码为准。

> 本文汇总 2026-10 一轮 harness 审计与演进中遇到的问题、根因与解决方案,作为各设计文档的总索引。
> **状态图例**:✅ 已实现(10-26 工作区,多数尚未提交) · 📝 设计定稿待实现 · 🔎 开放/待定

---

## 0. 方法与边界

全程遵守 Chronos 极简身份:`0 Skill / 0 RAG / 0 MCP / 无角色分解 / 无剧本`、单静态二进制、唯一直接依赖 `go-openai`。所有优化**借设计不借依赖、借机制不借反极简特性**。审计对象是 harness 工程(调度/上下文/编排/可观测/证据),不碰攻击逻辑。

---

## A. 上下文管理

| # | 问题 | 根因 | 方案 | 状态 |
|---|---|---|---|---|
| A1 | 关键信息被静默去除 | `blackboard.Summary()` 对 findings/facts 取「最近 25 条」(按插入序 `tail`),**早期、尤其高危 Finding 悄悄离开模型上下文**;与 severity-first 的呈现语义自相矛盾 | 重注入改**价值驱动配额**:verified + critical/high 必保;低价值给配额;溢出压成一行计数;复用 `FindingsSorted` | ✅ |
| A2 | 压缩是「按位置」而非「按价值」 | transcript 压缩 + recency cap 叠加,旧 cycle 原文与早期发现一起丢 | **cold-digest 保真压缩**:按活跃度(连续 6 轮未动)+ 血缘连通分组,LLM 压成可还原 digest(保留成员 ID/签名/代次);verified 及 critical/high 不折叠;提交前 liveness recheck | ✅ |
| A3 | 压缩阈值与模型脱钩 | `contextBudgetChars` 写死(≈35k token),不随模型窗口变 | 按模型 `window_tokens` 动态推导预算;transcript 预算与重注入预算解耦;用真实 `PromptTokens` 做反馈式触发 | 🔎 部分(models-file 已带 window,llmpool 按窗口跳过;压缩阈值动态化未确认) |
| A4 | 系统提示里的时间预算冻结 | `~N min left` 只在 `start()` 烘一次,系统提示永不重建 | 时间预算移到每轮重注入的 user 消息,按当前 deadline 重算 | 🔎 |
| A5 | 压缩无可观测性 | `compactMessages` 静默发生,无日志/事件 | 事件流新增 `compact_boundary`;控制台可见 | ✅ |
| — | **确立原则** | — | 必须永远记得的(scope/constraints/必保 finding)只能走「免压缩 index 0」或「每轮重注入」,绝不能只活在会被压掉的 transcript 里 | ✅ |

---

## B. 多 executor 协作

| # | 问题 | 根因 | 方案 | 状态 |
|---|---|---|---|---|
| B1 | 「实时共享」根本没发生(用户实测无感) | **默认单 `--model` = 单 executor,没有兄弟**可共享 | 去多模型竞态、鼓励多 executor;`--models-file` 每-executor 自定义模型;`--model` 优先于控制台配置 | ✅ |
| B2 | 即便多 executor,共享也是 cycle 粒度 | 黑板只在 cycle 入口经 `Summary()` 注入一次,cycle 内看不见兄弟新写 | cycle 内增量注入(黑板修订号 + 版本差分) | 📝 |
| B3 | 协作全程不可观测 | 控制台按 executor 分流,无「吸收兄弟发现」标记 | 结构化事件流(session_init/turn_start/turn_end/工具调用·结果/本轮结果)+ 控制台过滤视图 | ✅ |
| B4 | 并行写冲突(Cognition②) | N executor 对同一有状态靶场并行改状态,零协调 | 黑板**租约** `claim_surface`/`release_surface`:认领目标面,兄弟不重复/不冲突写;推向 sectioning 式覆盖分工 | ✅ |
| B5 | 多模型竞速语义不清 | 胜者 flag 聚合 + 同伴取消,与「协作不竞速」矛盾 | 删除胜者聚合;某 executor 找到 flag 不取消同伴;平台判真由调度器统一结束;重复规格只启动一次 | ✅ |

---

## C. 单 executor 能力边界

| # | 问题 | 根因 | 方案 | 状态 |
|---|---|---|---|---|
| C1 | 单 executor 上下文封顶 | 一条增长对话受窗口限制 | 黑板重注入 + 压缩 + **子 Agent 委派**(把支线深挖外包进隔离子窗口,只回传 finding ID) | A 列已实现;委派 📝 |
| C2 | 单线推理、视野窄 | 一个 executor 一次一条思路 | swarm 多 executor 覆盖 + StallPolicy pivot | ✅ |
| C3 | 模型能力天花板 | 单模型固定能力 | 模型无关 + 多模型 swarm + `llmpool` failover | ✅ |
| — | **刻意不补的** | — | 固定中性工具面(不接 skill/MCP)、无跨任务记忆——这是立身之本,非缺陷;`run_python` 给图灵完备空间 | 设计原则 |

---

## D. 任务编排

| # | 问题 | 根因 | 方案 | 状态 |
|---|---|---|---|---|
| D1 | 崩溃即重来 | 黑板落盘但执行态(executor 窗口/调度窗口/deferred 队列)丢 | **durable resume**:调度器 + executor 运行态落盘,重启从断点续 | ✅ |
| D2 | 发现者自验的确认偏置 | `verify_finding` 由发现者自己做 | **独立验证**:候选分派给另一 executor/模型验证,要求实际观察 + 流量证据(生成者≠评判者) | ✅ |
| D3 | engagement 无理性终止 | `ShouldTerminate` 默认永不,只能烧墙钟 | 覆盖完成 + 安静周期参与终止判断 | ✅ |
| D4 | 长会话窗口饱和只能有损压缩 | 单 executor 窗口到顶 | 饱和接力:起干净 executor 用黑板 handoff 接手 | 📝 |
| D5 | 操作员对话脱离工具上下文 | 另起「纯聊天」无工具模型回合 | 操作员消息进 executor **原会话队列**,下一轮结合工具历史处理 | ✅ |
| — | **编排红线** | — | 不加 routing/专门化 worker、不把调度器变 LLM、不退回竞速 | 设计原则 |

---

## E. 工程基建(借鉴 ARTEX,极简重实现)

| # | 问题 | 方案 | 状态 |
|---|---|---|---|
| E1 | 只有全局 token 计数,无 per-call/per-executor、无请求响应留存 | `llmrec`:逐次调用计量(token/延迟/状态)落 `llm_usage.jsonl` + 内存环形;`LLM_RECORD=1` 存完整逻辑正文到 `llm_records/`(不记 Key) | ✅ |
| E2 | 只有 key 轮询,无跨 provider failover/熔断 | `llm/pool.go`:备用链 + 硬失败首次熔断/软失败 3 次熔断 + 1/5/30min 冷却落 `llm_breakers.json` + 窗口感知跳过 + 未 yield 才切的安全窗 | ✅ |
| E3 | 只有 scope-lock,无通用工具闸门/审批 | `guard` PreToolUse:Scope 硬锁在前 + `CHRONOS_TOOL_RULES_FILE` 的 allow/deny/ask + 人工审批 + 「平台管控·非目标防御」标注 + 审计落 `tool_audit.jsonl` | ✅ |
| E4 | 无通知、无可移植归档 | `CHRONOS_WEBHOOK_URL` 高危/完成推送(去重落 `notifications.json`);engagement 每运行 `archive.tar.gz`(manifest SHA-256 去重) | ✅ |

> 全部用 `go-openai` + 标准库实现,`go.mod` 仍只有 `go-openai` 一个直接依赖,未引入 ARTEX 包 / Postgres / Skill / MCP / RAG / 固定角色。详见 [`artex-borrow-plan.md`](artex-borrow-plan.md)。

---

## F. 图设计(差异化亮点)

| # | 问题 | 根因 | 方案 | 状态 |
|---|---|---|---|---|
| F1 | 继续借 Cairn 的 fact-intent 探索图 = 无设计亮点 | Cairn 已把「状态空间搜索 + 探索图」跑到极致;Chronos 当前黑板本质同形 | **换轴**:从「探索图(往哪搜)」改为「**可废止证据图(凭什么信)**」 | 📝 |
| F2 | 判定靠模型写 label,有确认偏置、不可审计 | verdict 是存储字段,非算出的属性 | verdict 改为**图的纯函数**:proof 接地才可 confirmed、可被 proof 反驳翻盘(Dung 论证);报告即这张可审计的图 | 📝 |

> 这是 Chronos 唯一别人学不走的差异化:把「证据=真实流量」从数据特性升级成判决机制。详见 [`evidence-graph.md`](evidence-graph-design.md)。

---

## 状态总览

- **✅ 已实现(10-26 工作区)**:A1 A2 A5、B1 B3 B4 B5、C2 C3、D1 D2 D3 D5、E1 E2 E3 E4。
- **📝 设计定稿待实现**:子 Agent 委派(C1 纵向)、cycle 内增量注入(B2)、饱和接力(D4)、证据图(F1 F2)。
- **🔎 开放/待定**:压缩阈值动态化(A3)、时间预算每轮重算(A4)。

**共用前置基建**:黑板**单调递增修订号**——委派算「子 Agent 新建了哪些 ID」、编排「cycle 内增量注入」、以及证据图的边变更失效都依赖它,建议一次建成。

---

## 仍需实测的边界(诚实标注)

- 真实模型接口/平台/授权目标的**端到端运行尚未验证**;离线预检只证配置可解析。
- `LLM_RECORD` 存的是**逻辑**请求/响应,尚未含原始网络 wire 报文。
- 切换范围的「新执行实例」仍在**同一 Chronos 进程内**,非独立 OS 子进程。
- 本轮多数改动在工作区(`chronos-repo/`),除协作恢复主线 `c7f7122` 外尚未提交。
- `run_python` socket 护栏是「防意外漂移」非沙箱。

---

## 设计文档索引

- [`mission-modes.md`](../mission-modes.md) —— 「任务=搜索」设计基线
- [`subagent-delegation.md`](subagent-delegation.md) —— 子 Agent 委派(纵向上下文外包)
- [`orchestration-plan.md`](orchestration-plan.md) —— 任务编排优化(横向分工协调)
- [`artex-borrow-plan.md`](artex-borrow-plan.md) —— 借鉴 ARTEX 的 harness 工程
- [`evidence-graph.md`](evidence-graph-design.md) —— 可废止证据图(图设计差异化亮点)

**Obsidian 记录**:`2026-10-25.md`(上下文/协作问题)、`2026-10-26.canvas`(实现落地 14 节点)。
