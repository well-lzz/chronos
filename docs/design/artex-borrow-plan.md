# 借鉴 ARTEX：对照 Chronos 的设计方案

> **状态（2026-10-30 核对）**：本方案已实现：调用计量 `internal/llmrec`、备用链与熔断 `internal/llm/pool.go`、工具闸门 `internal/guard`、cold-digest、通知与归档见提交 `7e452b5`；`go.mod` 仍只有 `go-openai` 一个直接依赖。 本文保留设计取舍记录，实现细节以代码为准。

> 设计基线文档（纯设计，不含实现）。来源：通读 ARTEX（`well-lzz/ARTEX`，v0.3.15）的核心 harness 包 `guard` / `intercept` / `llmrec` / `llmpool` / `agent`(cold-digest 压缩) 后，对照 Chronos 现状得出。与 `docs/design/subagent-delegation.md`、`docs/design/orchestration-plan.md` 衔接。

---

## 0. 前提与两条红线

ARTEX 是 Chronos 的「重装版」同类项目：同为 Go 攻防 harness，但走 batteries-included 路线（Skill / MCP / RAG / Main·Plan·Worker 多 agent 分层 / Postgres / mitmproxy / 完整后台）。借鉴必须守住 Chronos 的立身之本。

**红线一：借设计，不借依赖。** ARTEX 的能力靠重依赖换来——`norma`(它自研的 LLM SDK)、Postgres(pgx)、go-mitmproxy、jwt、dnsx。Chronos 的核心卖点是 **单静态二进制 + 1 个依赖(go-openai)**。所有借鉴都要用「go-openai + 标准库（顶多再加一个 CGO-free 的 `modernc.org/sqlite`）」**重新实现**，绝不 import ARTEX 的包。

**红线二：借机制，不借「反极简」特性。** 明确不借：
- **Skills / MCP / enrich(资产图 RAG)** —— 违反 Chronos `0 Skill / 0 MCP / 0 RAG`。
- **Main / Plan / Worker 固定角色分工** —— 违反 `无任务角色分解`。要「外包子任务」用我们已设计的 `delegate`（模型临场发起、同一中性契约），不要 ARTEX 的固定角色。
- **Postgres 作为硬依赖** —— 如需持久化，用内嵌 SQLite，不引外部 DB 服务。

---

## 1. 四个重点借鉴（高价值、哲学兼容）

### A. LLM 调用录制 + 用量计量（借 `llmrec`）

**ARTEX 怎么做**：一个包住 `llm.Provider` 的 `Recorder` 装饰器，拦截每次 Stream/Complete：
- **两档分离**：轻量 **用量计量**（token/延迟/状态）**永远写**，即使调用中断/失败；**重量 trace**（完整 request/response body + 原始 wire 报文）由开关 `llm_record` 控制，默认可关。
- **归因**：从 session id（`exp<N>-<role>`）解析出 task / worker 泳道，token 花在谁身上一目了然；连「兜底审批判定」这种旁路调用也用 `WithWorker` 显式打标。
- **usage 折叠**：Anthropic 的 input/output token 分散在 `message_start` / `message_delta` 两个事件里，用 `Add` 折叠而非覆盖（否则会把开头记的 input 清零）。

**Chronos 现状**：只有一个全进程共享的 `*Tokens` 计数（`atomic.AddInt64`），**无 per-call、无 per-executor、无 request/response 留存**。排查「agent 为什么这么做 / 钱花哪了」几乎没有数据。

**最小落地（Chronos 版）**：
- 在 `llm` 包加一个 **Provider 装饰器**（Chronos 用 go-openai，直接包 `CreateChatCompletion`）。
- **轻量档**：每次调用后写一条 `{mission, executor(spec), model, input_tok, output_tok, latency_ms, status}`——先落内存环形缓冲 + 可选落 `CHRONOS_STATE_DIR/llm_usage.jsonl`。替换现在那个只会加总的 `*Tokens`。
- **重量档**：开关控制，开时把 request messages + response 落 `llm_records/`（JSONL 或每调用一文件），供控制台「LLM 记录」页回放。
- **归因**：Chronos 的 executor 已有 `spec` 标识，直接当 worker 泳道；mission 名当 task。

**优先级 ★★★**：零哲学冲突，是后续一切调参（压缩、编排、预算）的数据地基。也是我们在压缩/编排文档里反复要补的「可观测性」。

---

### B. LLM 池 failover + 熔断（借 `llmpool`）

**ARTEX 怎么做**：`Pool` 是个 `llm.Provider`，按有序链逐个尝试：
- **失败即转移的安全窗**：只在「尚未 yield 任何事件」时才切换——一旦有 text/tool_use 吐给上层，再换模型会重复输出、污染历史，所以中途失败原样上抛交给 resume。幸好要处理的失败（402 无额度 / 401 坏 key / 429 / 5xx）都在请求建立期暴露，落在安全窗内。
- **熔断器**（`Registry`，全进程共享）：**硬失败**（401/402/403/404，确定性）**首次即熔断**；**软失败**（429/5xx/网络）连续 N 次（默认 3）才熔断。冷却**阶梯**：1min → 5min → 30min。熔断状态**落盘**，重启后冷却窗仍在。
- **智能排序 `order()`**：跳过冷却中的、跳过上下文窗口装不下本请求的；同优先级组内**轮转**（`rr`）做负载分摊；全被过滤掉时至少探一下链头，宁可多发一次也不停摆。
- **`shouldFailover`**：ctx 取消/超时**不转移**（那是用户停任务，别烧备用 key）；400**不转移**（到处都一样坏）。

**Chronos 现状**：只有 DeepSeek key 池的逐请求轮询（鉴权/限流/5xx 切 key），**无跨 provider 链式 failover、无熔断器、无冷却阶梯、无窗口感知排序**。

**最小落地（Chronos 版）**：
- 把现有「key 轮询」升级为 **profile 链**：每个 profile = {base_url, model, key, priority, window_tokens}（复用控制台已有的模型设置，多填几条即可）。
- 实现 **shouldFailover / 熔断阶梯 / 窗口感知跳过 / 安全窗(未 yield 才切)** —— 这几段逻辑是纯标准库，可几乎照搬 ARTEX 的判定（HTTP 状态码分类、1/5/30min 阶梯、硬/软失败区分）。
- 熔断状态落 `CHRONOS_STATE_DIR`，重启不丢。
- 与 **B 的 llmrec 配合**：失败的那次尝试也要记在它自己的 profile 名下。

**优先级 ★★★**：长程/评测稳健性直接受益；纯工程、零哲学冲突。注意「安全窗只在未 yield 时切」这条与 Chronos 的 resume 设计要对齐。

---

### C. PreToolUse 工具闸门 + 人工审批（借 `guard` + `intercept`）

**ARTEX 怎么做**：每个工具调用先过 **PreToolUse hook**：
- 规则从 DB 载入、内存缓存、按优先级求值，三种动作：**allow**（放行并跳过低优先级）/ **deny**（拦截并回模型一条消息）/ **ask**（拦截 + 建 pending 记录 + 等人工批准，超时/拒绝则 block）。
- 无规则命中时可选 **LLM 兜底判定**（judge），其 token 单独计量（worker='judge'）。
- **block 消息的措辞很关键**：拦截消息明确写成「【平台管控·非目标防御】此操作被禁止」——否则渗透 agent 会把 403/denied 当成目标 WAF 去绕(改 payload、重编码、重试)，既徒劳又错误。审计行保留原始 reason，只有回模型的 tool_result 带这层措辞。
- **PostToolUse 归因（G5）**：把结果分类 blocked/error/ok，让规划者换策略而不是在 WAF 前放弃。
- **审计日志**：每次 gated 调用落一条（环形 2000）。

**Chronos 现状**：只有 **scope-lock** 一种硬规则（出网 host/CIDR 判定）。**没有通用的「工具执行前拦截/审批/审计」抽象**，也没有「把平台拦截和目标防御区分开」的措辞处理。

**最小落地（Chronos 版）**：
- 在 solver 的工具分发处（`dispatch.go`）加一个 **PreToolUse 钩子点**：scope-lock 保留为最高优先级的内置 deny 规则，之上叠加**可配置拦截规则**（allow/deny/ask，先从配置文件/控制台读，不必上 DB）。
- **ask → 操作员审批**：复用控制台已有的 operator 令牌通道；pending 记录先放内存 + 可选落盘；engagement 的「安全启动默认暂停」已经是雏形，可统一到这套。
- **直接照搬那条措辞经验**：Chronos 回模型的拦截消息也要写成「平台管控·非目标防御·此操作被禁止」，避免 agent 把它当目标防御去绕——这是**零成本、立刻见效**的一条。
- **审计环形缓冲**：每次 gated 调用落一条，控制台可见。

**优先级 ★★**：把 scope-lock 升级成通用工具闸门，正是我们在编排文档 P-级里标过要补的。「ask 人工审批」对 engagement（真实授权评估）尤其有价值。

---

### D. 冷摘要式上下文压缩（借 `agent` 的 cold-digest）—— 直击 Chronos 已知短板

**ARTEX 怎么做**（这是 ARTEX 最精巧的部分）：它不把对话当线性 transcript 压，而是把探索建成 **Fact/Intent 图**（节点 + 血缘边 From→To），在图上做压缩：
- **hot/cold 判定**：节点连续 R 轮（默认 6）无活动才算结构性 cold（按**活跃度**，不是按位置）。
- **连通分组**：把 cold 节点按血缘连通性（或共同父）分组，每组 ≥K(=2) 才折叠。
- **LLM 压成 digest**：每组用 LLM 压成一段「这一片探明了什么」的 digest 节点，**digest 保留 member_ids 可还原**，带 generation(重摘代次) 和 signature(变更检测)。
- **minor / major 两档**：minor 只折叠「尚未覆盖」的新 cold 集（增量）；major 回源重压全部、按 signature 复用未变的 digest（跳过 LLM）、淘汰过期 digest。
- **工程护栏**：每 task 一把锁、运行间 ≥60s 冷却、后台跑不堵规划热路径、**commit 时 liveness recheck**（压缩期间复活的节点从 covers 集里剔除，digest 绝不覆盖 hot 节点）。

**Chronos 现状（对照我们之前的发现）**：
- transcript 压缩（`compaction.go`）：按**字符预算**裁，保护 system[0] + 最近 3 cycle + tool 配对——**有损、按位置**。
- 黑板重注入（`Summary()`）：`tail(findOrd,25)` **按插入顺序取最近 25 条**，不是按价值——早期的、高危的 Finding 会**静默离场**。

ARTEX 的做法恰好补上 Chronos 这两处的根本缺陷:**从"按位置/按最近"升级到"按活跃度 + 按结构 + 价值驱动"的保真压缩**。

**最小落地（Chronos 版，分两档，别一步到位）**：

- **档一（低成本，先做）**：不建图，仅把 `Summary()` 的 recency-cap 换成**价值驱动配额**——复用已有的 `FindingsSorted()`：所有 verified / critical / high **必保**，candidate/low 给配额，溢出**压成一行计数**而非整条丢。这条我们在 `orchestration-plan` / 压缩笔记里已提过，是性价比最高的一步。
- **档二（借 cold-digest 精髓）**：给黑板的 Finding/Fact 之间补**血缘关系**（哪个 fact 催生哪个 finding、哪个 sub-goal 探出哪些），然后：
  - 对**冷掉的**（长时间未被引用/更新的）Finding/Fact 分组，用 LLM 压成一段 digest，**保留 ID 可还原**，注入时用 digest 顶替原始多条；
  - 借 ARTEX 的**工程护栏**：后台跑、冷却、liveness recheck（压缩期间被重新引用的不折叠）。
  - digest 与档一的「必保高危」正交：高危永远独立保真，只有低价值的冷节点才被折叠。

**优先级**：档一 ★★★（直接修静默丢高危的 bug）；档二 ★★（更大改动，需要先给黑板加血缘边，可在 delegate/认领落地后再做）。

---

## 2. 次要借鉴（有价值，非紧急）

| ARTEX 能力 | 借鉴点 | Chronos 最小落地 |
|---|---|---|
| **多渠道通知** `notify` | 无状态渠道适配器，凭据外部传入 | 长 engagement 完成/高危发现时推送；先做一个 webhook/邮件渠道即可 |
| **可移植任务归档** `traffic/archive.go` | 内容寻址去重 + streamable tar 打包一次任务的流量 | 报告导出时附一份可复现的流量包；复用现有 JSONL + 内容哈希 |
| **多视图后台** `web` | dashboard / task 详情 tab / approvals / llm-records 的信息架构 | 接「前端优化」那条：Chronos 单页控制台可拆出 Findings / Traffic / LLM记录 / 审批 四个视图 |
| **持久会话 + resume** `db` | 执行态落盘、按 session 反向分页、task 级快照游标 | 用 SQLite 落 executor 窗口 + 调度器状态，对齐我们 `subagent-delegation` §7 / `orchestration-plan` 的 durable resume |

---

## 3. 明确不借（守红线）

| ARTEX 的东西 | 为什么不借 |
|---|---|
| `skills/`（playwright-cli、api-recon、scopesentry） | Chronos `0 Skill`——预制 skill 就是它拒绝的「预设流程」 |
| `mcphttp` + stdio MCP + skill-gated 解锁 | Chronos `0 MCP` |
| `enrich`（dnsx + HTTP 探测回填资产图） | 引擎侧 RAG/知识，违反「方法论留给模型」 |
| Main / Plan / Worker 固定角色 | 违反 `无任务角色分解`；要外包用 `delegate`（中性契约） |
| Postgres / norma / go-mitmproxy 等重依赖 | 违反「单二进制 + 1 依赖」；一律用 go-openai + 标准库(+SQLite) 重实现 |

---

## 4. 落地优先级与衔接

| 次序 | 借鉴项 | 理由 / 衔接 |
|---|---|---|
| 1 | **A. llmrec 录制+计量** | 零冲突、是一切调参的数据地基 |
| 2 | **D 档一. Summary 价值配额** | 直接修「静默丢高危」的确定性 bug，改面最小 |
| 3 | **B. llmpool failover+熔断** | 稳健性；注意「未 yield 才切」与 resume 对齐 |
| 4 | **C. 工具闸门 + 审批** | scope-lock 升级为通用闸门；先落「平台管控措辞」这条零成本经验 |
| 5 | **次要：持久 resume / 后台多视图 / 通知** | 与两份已有设计文档的 durable resume、前端优化合并推进 |
| 6 | **D 档二. cold-digest 保真压缩** | 最大收益但需先给黑板加血缘边，放最后 |

**一句话**：ARTEX 当「组件货架」——从中挑 **tracing、failover、工具闸门/审批、冷摘要压缩** 四件中性 harness 原语，用 Chronos 的极简方式（go-openai + 标准库 +可选 SQLite）重实现；**skill/MCP/RAG/固定角色/重依赖挡在门外**。其中 D(压缩) 和我们之前自查出的 Chronos 短板是同一个问题的两种答案，ARTEX 的「按活跃度+结构+价值」思路值得完整吸收。
