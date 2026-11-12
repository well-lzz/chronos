# 上下文压缩：成本可控的长程执行

> 本文是 executor 上下文压缩策略的设计说明与实现基线。代码与本文冲突时，以 `internal/solver/compaction.go` 的实际实现为准（或先改本文再改码）。

**一句话**：一个 executor 在多轮重试里维护**同一条不断变长的对话**，每次调 API 都整条重发。不加约束时成本随轮数**平方级**增长。我们用「黑板沉淀 + 配对安全的窗口压缩」把每次发送的上下文**封顶在 ~3.5 万 token**，把累计成本从 `O(K²)` 压到 `O(K)`，且**不丢解题信息、不破坏 tool_call 配对**。

---

## 1. 问题：没有压缩时成本是平方级

每个 executor 的会话 `s.messages` 跨所有重试 cycle **只增不减**，每次 `CreateChatCompletion` 都把**全部历史**重新发送：

- 第 `k` 轮发送 ≈ `k` 份历史 → `K` 轮累计 ≈ `K²/2` 份。
- 实测（run 36593，**无压缩**镜像）：单个 session 累计 token 冲到 **3000–4800 万**，平均 **~85k token/次调用**——这就是平方尾巴。

目标不是"少发"，而是把**每次发送的上下文封顶**，于是累计 `O(K²) → O(K × 预算)`，变成线性。

## 2. 核心思想：把「持久知识」和「原始对话」分开

> 这是能安全丢历史的**前提**，也是本策略真正的壁垒。

- **持久知识**在共享黑板 `internal/blackboard`（`Blackboard`）上，是**结构化**的：`open sub-goals` + `findings`（id / 状态 / 严重度 / 证据数 / target）+ `facts`。
- 每一轮解题，`solver.go` 把 `bb.Summary()` **重新注入到本轮 prompt 顶部**：

  ```
  prompt = <blackboard 摘要> + "\n\n" + "Continue solving."
  ```

因此，**老的原始 tool 往返（一次次 HTTP 请求/响应大段正文）是冗余的**：它们的结论早已沉淀进黑板摘要、下一轮还会原样重注入。**丢掉它们不丢可操作的信号。**

> 对比：没有"结构化知识层 + 每轮重注入"的 harness，丢上下文就是**真丢信息**——只能要么全留（贵），要么乱截（丢发现 / API 报错）。我们因为有黑板，才敢**激进压缩且无损**。

## 3. 实现机制

**触发时机**：每次 `CreateChatCompletion` **之前**，`(*Solver).compactMessages()` 跑一次（持 `messageMu`，原地替换 `s.messages`）。核心逻辑在 `compactWindow(msgs, maxKeepTurns, budget)`。

**阈值**：`totalChars(msgs) <= budget` 直接返回、零开销——**绝大多数轮走这条快路径**。只有超预算才真正压缩。超预算后分两套机制：

### 3.1 主策略：在「干净的 user 边界」整轮丢弃旧 cycle

- 保留 **system 提示** + **最近 `contextMaxKeepTurns` 个 cycle**；从 `keep = 3→2→1` 依次尝试 `out = system + msgs[cut:]`，第一次落进预算就返回。
- **「干净边界」是正确性关键**：`cleanUserBoundaries()` 维护一个**未闭合 tool_call 计数器**（assistant 的 `tool_calls` `+N`，`tool` 结果 `-1`），**只有计数器归零处的 user 消息**才是合法切点。
  - 原因：OpenAI / DeepSeek 要求 `assistant.tool_calls` 必须紧跟其对应的 `tool` 结果。**在工具循环中途乱切会切断配对 → API 直接 400**。而操作员的 `Guide` / `SetScope` / `Pivot` 消息可能恰好落在工具循环中间，所以"是不是 user 消息"这一个条件不够。

### 3.2 兜底：`clipOld`（没有 ≥2 个干净边界，或单个超大 cycle 自己就爆预算）

- **按消息从大到小**反复裁剪正文，**只保护 system**，直到低于预算或无可裁。
- 只裁**文本字段**（`Content`，必要时裁 tool_call 的 `Arguments`），**保留 role / ToolCallID / tool_call 的 id**——所以配对永不断。
- 按 rune 切（`clipRunes`，头尾各留一半 + 省略标记），不切坏 UTF-8；单条地板 `perMsgClip`。
- 返回「**是否真的改过**」，杜绝"声称压缩了其实没压"（历史 bug，已修）。

### 3.3 安全带：`pairingValid`

返回前再校验一遍「每个 tool 结果都有前置 tool_call、每个 tool_call 都有应答」。**万一结构不对，就放弃本轮压缩、原样返回**——宁可这次不省，也绝不发出会 400 的序列。

## 4. 与 prompt 缓存的关系（诚实说明）

DeepSeek 对**稳定前缀**有缓存（命中约 1 折价）。两者关系：

- 压缩**会在触发那一刻让前缀变一次 → 一次 cache miss**；但压缩不频繁，之后更小的前缀重新缓存、更便宜。
- **光靠缓存不够**：不压缩时上下文仍在涨，缓存读的是**越来越大的前缀**，1 折也随规模线性增长；长上下文还拖**延迟**、稀释**模型注意力**（解题质量下降）。
- **压缩把前缀封顶** → 缓存读成本封顶 + 延迟低 + 模型聚焦。

净效果：**压缩 + 缓存 = 最省**；只有缓存、上下文照涨 = 成本与延迟仍随轮数增长。

## 5. 为什么这是差异化优点

同类 harness 常见两种做法都更差：

1. **不压缩**：上下文无界增长，成本平方级、延迟高、长上下文掉质量。
2. **粗暴截断**：易切断 tool_call/tool_result 配对（API 报错），或截掉关键发现（信息丢失）。

本策略同时拿到四点：

- **成本有界**：平方 → 线性。
- **正确性有保证**：只在干净边界切 + `pairingValid` 兜底，绝不 400。
- **信息无损**：黑板把结论结构化、每轮重注入，丢的只是冗余原文。
- **优雅降级**：单个超大 cycle 也能靠 `clipOld` 收进预算。

> 核心一句话：**是「黑板沉淀 + 配对安全的窗口压缩」这对组合，让我们敢把上下文压到 ~3.5 万 token 还不丢解题能力。**

## 6. 参数（`internal/solver/compaction.go`）

| 常量 | 值 | 含义 |
|---|---|---|
| `contextBudgetChars` | `140_000` | 触发阈值（≈ 4 字符/token ≈ **3.5 万 token**）。设在模型窗口之下，让重发历史保持便宜。 |
| `contextMaxKeepTurns` | `3` | 压缩时保留的最近 cycle 边界数。 |
| `perMsgClip` | `6_000` | 兜底裁剪时，单条消息正文的地板（rune）。 |

## 7. 代码地图

| 文件 : 符号 | 作用 |
|---|---|
| `internal/solver/solver.go` : `compactMessages()` | 每轮调 `CreateChatCompletion` 前的入口，持锁原地替换 `s.messages`。 |
| `internal/solver/solver.go` : `RunUntilDoneOrGaveUp()` | 构造本轮 prompt 时把 `bb.Summary()` 注入顶部；随后调 `compactMessages()`。 |
| `internal/solver/compaction.go` : `compactWindow()` | 主逻辑：阈值判断 → 丢 cycle → 兜底裁剪 → 配对校验。 |
| `internal/solver/compaction.go` : `cleanUserBoundaries()` | 计算「未闭合 tool_call 归零」的合法 user 切点。 |
| `internal/solver/compaction.go` : `clipOld()` / `clipMessage()` / `clipRunes()` | 按最大消息优先、rune 安全地裁剪正文，保留结构字段。 |
| `internal/solver/compaction.go` : `pairingValid()` | 配对不变量校验（返回前兜底）。 |
| `internal/blackboard/blackboard.go` : `Summary()` | 每轮重注入的结构化摘要（sub-goals / findings / facts）。 |
| `internal/solver/compaction_test.go` | 回归：丢旧 cycle 保配对、小会话不动、忽略工具循环中途注入、单个/最近超大 cycle 都断言压缩后 `≤ budget`。 |

## 8. 实测与成本算例

- **实测基线**（run 36593，无压缩）：平均 ~85k token/次调用；单 session 累计 3000–4800 万 token。
- **压缩后（封顶模型）**：每次发送 ≤ `contextBudgetChars`（~3.5 万 token）。
- **算例**：一道 stuck 题跑 50 cycle、每 cycle 新增约 `S` token。
  - 无压缩累计 ≈ `S × 50²/2 = 1250·S`。
  - 有压缩累计 ≈ `50 × 35k ≈ 175 万` token（与 `K` 线性）。
  - 轮数越多，差距越大（平方 vs 线性）。

## 9. 不变量与边界情况

- **system 永不丢**：`msgs[0]` 必须是 system，否则放弃压缩。
- **配对永不断**：只在干净边界切 + 只裁文本字段 + `pairingValid` 兜底。
- **无损性依赖黑板**：压缩的前提是"durable 结论已在黑板并每轮重注入"；若未来有不经黑板沉淀的关键信息，需先补进黑板再压缩。
- **诚实的 `changed`**：`clipOld` 返回是否真的改过，上层据此如实报告，不谎报压缩。
- **最坏情况**：若 system + 最近一个 cycle 的不可裁结构仍超预算，`clipOld` 尽力而为、可能略超——但这是结构下限，不是逻辑错误。
