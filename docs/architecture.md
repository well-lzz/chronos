# Chronos 架构分析

> 本文是对 chronos 这个自主安全评测 Agent 的整体工程架构说明，基于对 `cmd/` + `internal/` 全部源码（~7150 行，14 个包）的逐文件通读。代码与本文冲突时以代码为准（或先改本文再改码）。

---

## 0. 一句话

**一个无沙箱、黑板驱动的多执行器搜索引擎**：外层是一个不含 LLM 的"机械壳"（调度、轮询、容器生命周期），内层是一个平台无关的"评测核心"（模型 + 中性工具面 + 共享黑板）。两层之间只通过 `Platform` 接口和 `mission.Meta` 这两个窄契约耦合，使得同一个核心既能打 TSecBench 评测，也能跑真实授权评估。

---

## 1. 两层架构：机械壳 vs 评测核心

整个系统刻意分成两半，边界是 `Platform` 接口：

```
┌─────────────────────── 机械壳 (Mechanical Shell, 无 LLM) ───────────────────────┐
│  main → scheduler → poller → platform(TSec adapter)                             │
│  职责：拉题、并发窗口、容器起停、确认解出、deferred 回灌、全局预算                 │
└────────────────────────────────┬────────────────────────────────────────────────┘
                                 │  platform.Platform 接口 + mission.Meta
┌────────────────────────────────┴──────── 评测核心 (Assessment Core, 平台无关) ───┐
│  swarm → solver → {blackboard, traffic, tools, scope, llm, nettools}            │
│  职责：对单个 mission 跑协作式多执行器搜索，记录 Facts/Findings，提交 flag        │
└──────────────────────────────────────────────────────────────────────────────────┘
```

**关键设计意图**（`platform.go` 注释原话）：TSecBench 只是**一个** adapter，不是硬绑定。要接另一个评测平台或工作源，只需实现 `Platform` 接口并在 `platform.New` 注册；scheduler / swarm / console 都与平台无关。真实评估走 `engagement` 路径，根本不用 platform。

这条边界是整个代码库最重要的架构决策：**核心不知道"CTF"是什么**，它只知道"在一个授权 scope 内搜索、把发现记到黑板、如果目标带 flag 就提交"。

---

## 2. 包依赖分层

从底向上（箭头 = 依赖方向），无环：

```
L0 基础类型   mission, scope, config, llmconfig
L1 能力层     nettools(纯IO)  traffic(证据日志)  blackboard(知识层)  llm(模型解析+Key池)
L2 执行层     solver (一个执行器：cycle 循环 + 工具 dispatch + 压缩 + scope 守卫)
L3 协作层     swarm  (N 个 solver 并行跑一个 mission，共享黑板，提交去重/熔断)
L4 适配层     platform(接口+TSec)  poller(轮询+事件)
L5 协调层     scheduler (机械调度)  |  engrun/engagement (真实评估)
L6 入口+观测  cmd/chronos/main     |  web/console(SSE控制台)  report(产物渲染)
L-辅助        prompts(系统提示)  termination(停止策略)
```

- `nettools` **不含** scope 检查（纯 IO + 截断）；scope 在 `solver` 里、调用 nettools **之前**强制。这把"授权边界"和"网络 IO"分开，边界只有一个地方。
- `blackboard` 不依赖任何上层，是纯粹的状态容器 —— 这让它能被 solver 和 swarm 共享而不产生循环依赖。

---

## 3. 一次运行的完整生命周期

```
main.runScheduler
  → platform.NewTSec(baseURL, token)
  → scheduler.New(pf, cfg, specs)        maxConc 默认 3
  → scheduler.Run(ctx)
      ├─ 可选 context.WithDeadline(TotalBudgetSeconds)   // 全局预算
      ├─ poller.Start(runCtx)                            // 抖动轮询平台
      └─ for {                                            // 1s tick 主循环
           select: runCtx.Done | poll.Events | cmds | tick
           ├─ poll.Terminal()?  → "平台任务已结束" 干净停机
           ├─ reapAndRefill()   → 回收 done 的 run，未解出的转 deferred
           ├─ 每 60s: reconcileContainers + autoSpawnUnsolved + handleDeadlock
           └─ 全解完且无在途 → 退出
         }

autoSpawnUnsolved → 按 priority 排序填满空槽 → spawnOne(name)
  spawnOne (goroutine, 不阻塞主循环):
    → pf.PullChallenge          // 起容器，拿 container_addr
    → context.WithTimeout(perChallengeLimit)   // 单题时限 1800s / killchain 2700s
    → blackboard.LoadOrNew + traffic.LoadOrNew // 断点续跑
    → swarm{Meta, Scope, Platform, BB, TL}.Run(sctx)

swarm.Run → 每个 model spec 起一个 goroutine runExecutor:
    solver.New(...)
    for {                                      // cycle 循环
      res = solver.RunUntilDoneOrGaveUp(ctx)   // 一个 cycle = 一整段对话
      stall.Observe(progress?) → Pivot / Terminate(benchmark)
      StatusFlagFound → return flag (记录，不杀兄弟)
    }

solver.RunUntilDoneOrGaveUp:                   // 一个 cycle
    prompt = blackboard.Summary() + "Continue solving."   // 每轮重注入黑板
    for {                                                  // tool-call 循环
      compactMessages()                                    // 发送前压缩上下文
      resp = client.CreateChatCompletion(messages, tools)
      for tc in resp.ToolCalls: dispatch(tc) → append tool result
      maxToolCallsPerCycle = 40 → 结束本 cycle
    }
```

**解出如何终止**：不是 solver 自己判定。`submit_flag` 经平台确认后，**poller** 在下一次轮询看到该题 solved → 发 `ChallengeSolved` 事件 → scheduler `killSwarm(solved=true)` → `cancel()` 掉这题的 context → 所有执行器一起停。这是"单一事实源"设计：平台确认才算数，执行器之间不互相抢杀。

---

## 4. 并发模型（三级嵌套 + 取消传播）

```
scheduler (1 个主循环 goroutine)
  └─ 并发窗口 maxConc=3 个 run，每个 run 一个 context (WithTimeout 单题时限)
       └─ swarm.Run: 每个 model spec 一个 runExecutor goroutine（当前 1 个 spec）
            └─ solver cycle 循环（串行），每 cycle 内 tool-call 串行
  └─ spawnOne 的 PullChallenge 各自开 goroutine（起容器慢，不阻塞主循环，用 s.pulls WaitGroup 收口）
```

取消传播（context 树）：
```
ctx(main)
 └─ runCtx = WithDeadline(TotalBudgetSeconds)     // 全局预算，scheduler 持有
     └─ sctx = WithTimeout(perChallengeLimit)     // 单题时限，每个 run 持有
         └─ swarm 把 sctx 传给每个 solver          // cycle 循环每轮检查 ctx.Err()
```
- **向下取消**：全局预算到 → runCtx cancel → 所有 run 一起停；单题 cancel（解出/超时/operator kill）→ 只停该题。
- **共享状态同步**：黑板用 `sync.Mutex`，traffic 用自己的锁，swarm 的提交状态用独立 `subMu`，token 计数用 `atomic`。executor 之间**协作不竞争**：共享黑板、不互相 cancel。

> 这解释了 37332 排查里看到的现象：平台结束任务那一刻，runCtx 被取消，3 个并发 solver 同时停在"工具已发、结果未回"——这是取消树自上而下传播的正常表现。

---

## 5. 核心抽象详解

### 5.1 `Platform` 接口（可插拔工作源）
`platform.go` 定义了 10 个方法的窄接口：拉题 `FetchAllChallenges`、轮询 `FetchPollState`、起容器 `PullChallenge`、提交 `SubmitFlag`、释放 `ReleaseChallenge`、回收僵尸容器 `CloseStaleContainers` 等。`Challenge` / `SubmitResult` 是归一化视图。TSec adapter（`tsec.go`，797 行，是最大的单包）把平台的 JSON/JWT 细节全部吸收在这一层。

**可插拔性验证**：scheduler、swarm、solver 全程只见 `platform.Platform` 和 `mission.Meta`，没有任何 `tsec` 字样。

### 5.2 `scope.Scope`（唯一的硬授权边界）
`scope.go`：不可变的 host 集合 + CIDR 列表。
- `Allows(host)` 是**唯一**的放行判定；空 scope **放行一切皆否**（fail-loud）。
- 无 DNS 解析（避免 rebinding 绕过），host 精确小写匹配，IP 走 `netip.Prefix.Contains`。
- solver 的每个出网工具（`http_request` / `tcp_send` / `run_python` 的 socket）在调 `nettools` 前先过 `hostAllowed`。**这是整个系统的安全基石**：边界只有一处，无旁路。

### 5.3 `Blackboard`（可持久化的知识层——真正的壁垒）
`blackboard.go`，671 行。三类结构化状态：
- **Facts**：确认的世界状态（服务/版本/端点/凭据）。
- **Findings**：搜索产物，有 `candidate → verified | refuted | inconclusive` 生命周期，用真实 traffic id 做证据（`EvidenceRef{ref, role}`）。
- **SubGoals**：模型自己的覆盖清单（open/resolved）。

两个关键方法：
- `Summary()`：把上面三类压成一段紧凑文本，**每个 cycle 重新注入 prompt 顶部**。
- `ProgressMarker()`：`len(facts)+len(findings)+verified数`，是 stall 策略判断"有没有进展"的唯一信号。

每次 mutation 都 `persist()` 到 JSON（`LoadOrNew` 支持断点续跑）。

> **这是上下文压缩能无损的前提**（见 `docs/context-compaction.md`）：durable 结论都在黑板、每轮重注入，所以丢掉老的原始 tool 往返不丢信息。黑板 + 压缩这对组合是这个 harness 省成本的根。

### 5.4 Solver cycle + 上下文压缩
`solver.go`：一个执行器维护**一条不断变长的对话** `s.messages`。每次 `CreateChatCompletion` 前跑 `compactMessages()`（`compaction.go`），把发送上下文封顶在 ~3.5 万 token，累计成本从 `O(K²)` 压到 `O(K)`。cycle 内 tool-call 串行，`maxToolCallsPerCycle=40` 封顶。详见专门的压缩设计文档。

### 5.5 `termination` / stall 策略
- `termination.go`：`ShouldTerminate` 默认永不提前停（留给安全边界），是个用户可改的策略钩子。
- `stall.go`：进展驱动的 `pivot → terminate`。`Observe(madeProgress)`：连续无进展 → `Pivot`（换思路）；pivot 都用尽仍无进展且是 benchmark 模式 → `Terminate`（放掉这题的槽，让别的题跑）。**只看进展、不看墙钟**，所以"慢但在推进"的解题永远不会被误杀。

### 5.6 `poller` + `Terminal`
`poller.go`：抖动间隔（5–15s）轮询平台，`crypto/rand` 抖动。产出 `NewChallenge` / `ChallengeSolved` 两类事件。`terminal` 标志在平台返回 `ErrTaskNotRunnable`（body 含 "already finished" / "task_not_found"）时置位 → scheduler 据此干净停机。这是 #13 加的"平台终态检测"，防止对一个已结束的任务空转。

### 5.7 `llm` Key 池轮转（成本/限流）
`llm.go`：`keyPoolDoer` 是个 `http.RoundTripper` 包装，在多把 DeepSeek Key 之间轮转：
- `next atomic.Uint64` 轮询游标，**随机起点**（`rand.Intn`）避免所有首请求压到同一把 Key。
- 遇到可重试状态码（限流等）自动换下一把 Key 重放（`req.GetBody` 重建 body）。
- `ProfileKeys` / `poolKeys`：把 profile 自带 Key 和环境 DeepSeek 池（`DEEPSEEK_API_KEY` / `_2` / `_KEYS`）去重合并，仅在 DeepSeek 端点时合并。

---

## 6. 状态与持久化

| 数据 | 位置 | 格式 | 作用 |
|---|---|---|---|
| 黑板 | `StateDir/<mission>.blackboard.json` | JSON，每次 mutation 落盘 | Facts/Findings/SubGoals，断点续跑 |
| 流量证据 | `StateDir/<mission>.traffic.jsonl` | JSONL 追加 | 每次 http/tcp 往返，findings 引用其 id |
| 模型配置 | `StateDir` llmconfig | JSON | operator 在控制台改的 profile |

**无数据库、无外部依赖**：纯文件，重启后 `LoadOrNew` 恢复。mission 之间状态隔离（按 mission 名 slug）。

---

## 7. 成本控制（这套 harness 的差异化优点）

三条叠加：
1. **上下文压缩**：每次发送封顶 ~3.5万 token，`O(K²)→O(K)`（专门文档详述）。
2. **黑板重注入**：丢老上下文无损，让激进压缩成为可能。
3. **Key 池轮转 + prompt 缓存**：多 Key 分摊限流；稳定前缀命中 DeepSeek 缓存（cache_read ≈ 1 折）。

> 实测（run 36593 无压缩镜像）单 session 累计冲到 3000–4800 万 token；压缩后每次发送被封顶。

---

## 8. 安全与公平性边界（刻意写进架构）

- **Scope 是硬出网边界**：唯一放行点 `scope.Allows`，无 DNS、无旁路、空 scope 全否。
- **系统提示无攻击方法论**（`prompts.go` 原话："No attack methodology, no playbook, no termination rule"）：只给中性任务契约（搜索、记黑板、带 flag 就提交）。
- **无跨轮记忆/无内嵌答案**：黑板只在**单个 mission 内**共享，mission 之间文件隔离；提示词不针对具体赛题。
- **提交熔断**（`swarm.trySubmit`）：同一 flag 去重；递增冷却（0/30s/2m/5m/10m）；连续 4 次错误 → `submitLocked` 锁死，阻止盲猜刷错。
- **Flag 确认走平台**：只有平台 confirmed 才记为 verified finding 并终止该题。

---

## 9. 可观测性

`web/` 嵌入式控制台（纯 stdlib，SSE 推送，无 websocket 依赖）：实时看每个执行器的 tool_call/tool_result 流、黑板 findings（按 confirmed/suspected/failed 的业务语汇，不是 CTF 的 solved/unsolved）、operator 可 pause/resume/kill/bump/chat。`console.Source` 是 web 与"跑 mission 的东西"（scheduler 多题 / engrun 单评估）之间的契约，两者呈现同一套发现视图。

---

## 10. 配置面（env 旋钮，`config.go`）

| 变量 | 默认 | 含义 |
|---|---|---|
| `MAX_CONCURRENT` | 3 | 并发窗口（平台实例上限） |
| `CHALLENGE_TIME_LIMIT_SECONDS` | 1800 | 单题时限 |
| `KILLCHAIN_TIME_LIMIT_SECONDS` | 2700 | 多阶段题时限 |
| `TOTAL_BENCHMARK_BUDGET_SECONDS` | 0=关 | 全局墙钟预算（镜像里设 28200） |
| `MIN_REMAINING_TO_SPAWN_SECONDS` | — | 剩余预算不足则不再起新题 |
| `MAX_CHALLENGE_ATTEMPTS` / `DEFER_RETRY_COOLDOWN_SECONDS` | — | deferred 回灌的次数上限与冷却 |
| `DEEPSEEK_API_KEY` / `_2` / `_KEYS` | — | Key 池（轮转） |

---

## 11. 扩展点（怎么加东西）

- **加一个平台**：实现 `platform.Platform` 10 个方法，在 `platform.New` 注册。scheduler/swarm 不动。
- **加一个模型 provider**：`llm.Resolve` 里加一个 `provider/model` 分支（OpenAI 兼容即可）。
- **加一个工具**：`tools.go` 加定义 + `dispatch.go` 加分发 case；出网类工具记得先过 `hostAllowed`。
- **改停止策略**：`termination.ShouldTerminate`（覆盖率/收益递减/finding 数阈值）或 `stall.StallConfig`。

---

## 12. 架构优点与权衡（诚实评估）

**优点**
- 两层解耦干净，核心平台无关，可插拔性真实（不是口号）。
- 黑板 + 压缩这对组合是独有的成本优势，且无损。
- 安全边界单点化（scope）、公平性写进架构（无 playbook/无跨轮记忆）。
- 无外部依赖（纯文件状态 + stdlib SSE），部署简单、可断点续跑。
- 取消树清晰，解出判定单一事实源（平台确认），executor 协作不互杀。

**权衡 / 潜在风险点**
- **无进程级看门狗**：运行主循环没有自杀/panic-recover 兜底。进程级的异常（OOM、未捕获 panic）会直接带走整个 run——这类中断只能靠平台侧感知，Agent 自身无日志痕迹（与 37332 排查结论一致）。
- **资源翻搅无上限**：压缩触发频繁 + 多执行器并发 `run_python` 时，session/子进程/内存可能尖峰。当前没有并发 session 数或内存的守护，极端情况下可能被沙箱 OOM-kill（37332 的开放疑点）。
- **模型路由偏斜**：`base_model` 虽配了 deepseek-flash + deepseek-chat，但实测调用严重偏向 flash（340:1），chat 几乎不被选中——这是解题效率问题，值得单独排查路由/配额分配逻辑。
- **TSec adapter 偏重**：797 行集中在一个包，平台协议变更的维护压力都压在这里（但这正是"把脏东西关在边界层"的代价，是有意的）。
- **单一 model spec 实跑**：swarm 支持多 spec 并行，但当前只跑一个，"swarm"的协作潜力未完全释放。

---

## 13. 代码地图（快速索引）

| 包 | 行数 | 职责 |
|---|---|---|
| `solver` | 2474 | 执行器核心：cycle、工具面、压缩、scope 守卫、原语/探针 |
| `platform` | 797 | 平台接口 + TSec adapter（JWT/JSON 细节） |
| `scheduler` | 791 | 机械调度：并发窗口、spawn/reap/defer、容器 reconcile |
| `blackboard` | 671 | 可持久化知识层（Facts/Findings/SubGoals） |
| `swarm` | 461 | 多执行器协作、提交去重/冷却/熔断 |
| `llm` | 444 | 模型解析 + Key 池轮转 |
| `report` | 418 | 产物渲染（json/md/html） |
| `web` | 367 | SSE 控制台 |
| `traffic` | 345 | 证据流量日志（JSONL） |
| `cmd/chronos` | 354 | 入口：preflight / engagement / scheduler 三模式 |
| `poller` | 287 | 抖动轮询 + 终态检测 |
| `termination` | 246 | stall 停止策略 |
| `config` | 237 | env 配置 |
| `scope` | 217 | 硬出网边界 |
| `engrun`/`engagement` | 415 | 真实评估模式 |
| 其余 | — | mission / prompts / nettools / console / llmconfig |
