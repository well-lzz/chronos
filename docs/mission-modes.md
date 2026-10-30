# Mission 模型：Fact / Goal / Step / Finding

> 本文是"从 CTF 专用 → 通用攻防/代审"重构的**设计基线**。开工前先 review；代码与本文冲突时以本文为准（或先改本文）。承接 [`rewrite-plan.md`](rewrite-plan.md) §0「任务=搜索」。

> **当前决策**：Capture/Assessment 两种模式已合并为一种行为，不再有 `mission_mode` 开关。executor 共享黑板、协作搜索；即使配置多个模型，也没有胜者选择或同伴取消。`submit_flag` 是目标带 flag 时记录已验证 Finding 的可选能力；平台确认任务完成后，由调度器结束该任务。终止规则见当前 Go 实现的 `internal/termination` 和调度器。

## 0. 核心抽象

一切任务都是**搜索过程**。用四个概念描述：

- **Fact** — 已确认的事实，即**当前世界状态**（发现的主机/端口/服务版本/端点/凭据/已获访问…）。
- **Goal** — 搜索的**终止条件**；可动态增删 **Sub-Goal**（拿到某立足点 → 新增"在该主机提权"的子目标）。
- **Step** — 下一步做什么：**从已有 Fact 产生新 Fact** 的因果转移（= 一次/多次工具调用 + 解读）。
- **Finding** — 搜索途中记录的**产物**；可动态定义，漏洞类任务里 Finding 就是**安全漏洞**。

统一了两类任务：

| | 产物 vs Goal | 终止 |
|---|---|---|
| **CTF** | 产物 = Goal = flag（flag 既是 Finding 又满足 Goal） | 平台确认后由调度器结束任务 |
| **渗透 / 攻防 / 代审** | Goal 弥散（评估范围）；产物 = 途中的 Finding，持续累积 | 预算 / 覆盖完成 |

## 1. 映射到现有内核

### 1.1 Blackboard（黑板）—— 显式共享状态
一个 per-mission 的结构化存储：**Facts + Findings + Goals/Sub-Goals**。它替代"只靠 LLM 上下文"的隐式记忆——长程 engagement 的事实量**装不下上下文窗口**，必须显式、可查询、跨 Step 与跨 executor 共享。黑板也天然统一了现有的 `message_bus` / sibling-findings（兄弟共享 = 读写同一黑板）。**黑板落盘持久化**（§7.5），崩溃/重启不丢发现。

### 1.2 Step 循环（取代自由 ReAct 循环）
每一步：
1. **读黑板**：当前 Facts + 未决 Goal/Sub-Goal（注入 prompt）。
2. **模型决定并执行下一个 Step**（自由决策，无预设流程）：http/tcp/run_python 等工具调用。
3. **模型记录产物**：用中性工具把新 Fact / Finding / Sub-Goal 写回黑板。
4. **判终止**：Goal 谓词是否满足。

本质仍是"模型自由驱动搜索",只是把**状态/目标/发现显式化**。

### 1.3 新增中性工具（模型自己维护黑板）
`add_fact(statement, evidence)` · `add_finding(finding)` · `add_subgoal(desc)` · `resolve_subgoal(id, outcome)` · 可选 `list_facts()/list_findings()`。
——这些是**认知结构**工具，不含任何"怎么打"的方法论。现有 http_request/tcp_send/run_python 不变。

### 1.4 终止谓词
- **任务内部**：根据未决 Sub-Goal、覆盖进展和预算判断执行器是否结束；未验证候选的验证 Sub-Goal 仍需处理。
- **平台任务**：平台确认已解决后，调度器结束整个任务；不按哪个模型先得到 flag 选赢家。

### 1.5 Executor 池：协作而非竞速
"solver" 重新定义为 **executor（执行单元）**——不绑定某道题，而是整个发现过程的执行力。多个 executor **共享同一 mission 黑板**，各自拉取下一个 Step/Sub-Goal、执行、写回。各 executor 协作覆盖、汇聚发现，互相不取消。

## 2. 统一 Mission 行为

所有任务使用同一套 LLM、黑板、中性工具、scope 约束、执行轨迹和报告。多个 executor 共享任务状态并协作。平台提供 oracle 时，`submit_flag` 可以确认 Finding；平台确认任务完成后，调度器统一结束任务。

## 3. scope = 强制授权边界（非可选）
Assessment 模式必须以一份**显式授权范围**为输入（engagement 配置：目标/资产清单 + 交战规则 RoE + 目标描述）。agent 只在范围内行动——现有结构性 target-lock 升级为"engagement scope",**硬约束**。这既是真实渗透的法律/职业底线，也让工具始终处于"仅授权范围"的正道上。

## 4. Finding schema（可动态定义，默认=安全漏洞）
```
Finding {
  id, title, target,
  category,            # 如 web/injection, auth, binary, cloud…（描述性，非预设流程）
  severity,            # info/low/medium/high/critical
  status,              # candidate(unverified) → verified | refuted  （见 §7.1 生命周期）
  evidence,            # 复现证据 / 轨迹引用（trace step、请求响应片段）
  reproduction,        # 复现步骤
  remediation,         # 修复建议（可选）
  found_by, ts, verified_ts
}
```
- **生命周期**：`add_finding` 可写入 `candidate`；验证是一次/多次 Step（尝试复现/确认）→ `verified` 或 `refuted`。未验证候选会生成一个"验证该候选"的 Sub-Goal。
- flag 是 Finding 的特例（category=flag，status 由平台 oracle 直接定）。

## 5. 报告（你已选：本地 JSON + MD/HTML）
黑板的 Findings → `report.json`（结构化）+ `report.md`（可读）+ 可选 `report.html`。含 run 元数据、每条 Finding、覆盖/耗时/token/成本汇总。Capture 模式的报告 = 解出的题 + flag + 证据。

## 6. 与 Less-Is-More 的关系（张力说明）
加入黑板 + Goal/Finding 管理，比"自由 ReAct + 极简工具"**是多了结构**。但它加的是**领域中立的认知结构**（如何表示状态、目标、发现），**不是**攻击方法论、预设流程或 benchmark 拟合——模型每一步仍自由决策"怎么搜"。对弥散目标的长程搜索,显式状态是**必需**（上下文装不下整场 engagement 的事实）。Playwright MCP 只提供通用浏览器探索能力，不增加预设攻击流程；0 Skill/RAG、无角色多 agent 的原则保持。

## 7. 决策（已定稿）

1. **Finding 门槛 → 记录候选 + 主动验证**。`add_finding` 允许写入 `confidence=unverified` 的**候选**；候选本身是搜索的产物。agent 需要**主动验证**候选：验证是一次/多次 Step（尝试复现/确认），结果把 Finding 提升为 `verified` 或标记 `refuted` 丢弃。报告区分已验证 / 未验证。
2. **solver = 执行力，不绑定"解题" → executor 模型**。solver 不再定义为"解某道题"，而是"整个发现过程中的**执行单元**"：它在一个 **mission 的共享黑板**上拉取下一个 Step/Sub-Goal、执行、写回 Fact/Finding。多个 executor 共享同一黑板、协作覆盖，互相不取消。
   - CTF(Capture)：每道题 = 一个**单目标 mission**（隔离靶场、自带 flag-goal），调度器并发跑若干个。
   - 渗透(Assessment)：整场 engagement = 一个**多目标 mission**（一片 scope、一块贯通的黑板，发现全局累积、支持横向）。
3. **终止/覆盖 → 涌现式覆盖（建议方案，待你确认）**：不用硬编码清单。覆盖度由**模型自己动态提的 Sub-Goal** 体现（未决/已解 Sub-Goal = 活的 checklist）。"某目标算完" = 所有未决 Sub-Goal 已解决 + 连续 N 步无新 Fact/Finding + 在预算内。未来的**规则层(#4)可注入覆盖型 Sub-Goal** 强化完整性，作为可选插件，不进内核。
4. **Sub-Goal 来源 → 模型自己提**；预留**规则层**扩展点（后续由你加入规则注入/排序 Sub-Goal）。
5. **黑板持久化 → 做**。Facts/Findings/Goals 落盘（survive 重启/崩溃），Assessment 长跑不丢发现。*注意：这是对 `rewrite-plan.md` "删 checkpoint" 的局部修订——只持久化黑板这份高价值状态，不恢复旧的 solver checkpoint/resume 全套。*

## 8. 对现有代码的影响（迁移面）
- `HeadlessSolver` → **Executor**：自由循环 → **Step 循环 + 黑板注入 + 新工具**；不再绑定单题，而是在 mission 黑板上执行。`submit_flag` 降为 Capture 模式"确认终止目标 Finding"的特例。
- 新增：`blackboard.py`（Fact/Finding/Goal 存储 + 持久化）、`findings` schema、`report.py`（渲染 JSON+MD/HTML）、`mission` 配置与 Capture/Assessment 两种 policy。
- `platforms.py` → 泛化为 **mission source**：TSecBench 平台是其一；本地 engagement 配置（scope/RoE/目标）是其二。
- 机械调度器基本不变（跨 mission/目标并发 + 预算 + 滚动重试）。
- Sub-Goal **规则层**：预留扩展点（模型自提为默认，后续规则注入）。
- 代码审计 = Assessment 的一个**目标类型**（工具面换为读码/静态查询，产物同为 Finding）——后置。

## 9. 落地顺序（review 通过后）
1. ✅ 黑板 + Fact/Finding/Sub-Goal 工具 + Step 循环；**先在 Capture 模式跑通，保证 CTF 不回归**。（`backend/blackboard.py`；solver 5 个中性工具 + 黑板注入；swarm 每题一块共享黑板；flag 记为 verified Finding）
2. ✅ Assessment policy（无 oracle 终止、非竞速、Finding 累积）。**后续按用户决策进一步简化**：两模式合并为一种行为（删 `mission_mode`）；移除内置递减收益终止，改为 `backend/termination.py:should_terminate` 用户钩子（默认永不提前停，`MAX_BUMPS` + 墙钟兜底）；swarm 一律协作不竞速；`submit_flag` 降为可选能力。
3. ✅ engagement 配置 work-source + scope 强制。（`backend/scope.py` 统一 host+CIDR 匹配，三个工具同一判定；`backend/engagement.py` 配置 + **空 scope 的 assessment 直接拒绝运行**；`--engagement` CLI；assessment 专用中性提示词）
4. ✅ report 渲染（JSON + MD/HTML）。（`backend/report.py`；engagement 结束写 `<state_dir>/reports/<name>/report.{json,md,html}`）
5. ⏳ 代码审计目标类型（后续迭代）—— Assessment 的一个目标类型，工具面换为读码/静态查询，产物同为 Finding。

> 落地说明：步骤 1–4 均为**增量、Capture 不回归**（`mission_mode` 默认 `capture`，CTF 路径逐字不变；CIDR 仅在 engagement 出现）。校验手段：`ruff` + `compileall` + Scope/黑板/report 单元往返 + run_python 子进程 scope 守卫实测（CIDR 内放行、CIDR 外与域名越界拦截）+ engagement 空 scope 三类拒绝。环境缺 `pydantic_ai`/`genai_prices`，故未做整图 import 运行。
