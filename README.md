# chronos

面向智能体攻防（Agentic Offensive Security）的**极简**自动化系统，**纯 Go**，单静态二进制。核心信念：决定成绩的是模型本身的 agentic 推理能力，不是外层臃肿的 Agent 架构，也不是一堆 Skill / 预设流程。因此本项目刻意做减法——**0 Skill、0 RAG、0 MCP、无任务角色分解、无预设攻击流程、无跨任务记忆**；只保留「模型 + 中性工具面 + 遵守平台规则所必需的机械外壳」。

唯一外部依赖：[`go-openai`](https://github.com/sashabaranov/go-openai)（任意 OpenAI 兼容端点）。设计基线见 [`docs/mission-modes.md`](docs/mission-modes.md) 与 [`docs/rewrite-plan.md`](docs/rewrite-plan.md)；实现说明见 [`docs/go-port.md`](docs/go-port.md)。

> 本仓库由早期 Python 原型迁移而来，现为纯 Go 实现。设计文档里出现的 `backend/*.py` 路径指早期原型，能力已对应移植到 `internal/`。

## 任务模型：一切都是搜索

Agent 的核心循环被建模为「**搜索 + 随手捕获每一个产物**」，而不是「追逐那一个终极 flag」。用四个概念描述：

- **Fact** — 已确认的世界状态（主机 / 服务版本 / 端点 / 凭据 / 已获访问…）。
- **Goal / Sub-Goal** — 搜索的终止条件，可动态增删。
- **Step** — 从已有 Fact 产生新 Fact 的一步（工具调用 + 解读）。
- **Finding** — 搜索途中的产物；漏洞类任务里就是安全漏洞（候选 → 验证 → 已验证 / 已否决 / 存疑）。

这些状态显式记录在一块 **Blackboard（黑板）** 上——跨 Step、跨同题 executor 共享、落盘持久化，让长程搜索的记忆不受上下文窗口限制。黑板是**领域中立的认知结构**，不是攻击方法论。

### 单一行为（无模式开关）

没有 capture/assessment 之分，全系统只有**一种行为**：在授权 scope 内搜索，把 Fact/Finding 记到黑板，多个同构 executor **协作**（不竞速、互不取消）。若目标恰好带 flag（可被平台验证的终极产物），`submit_flag` 把它作为"已验证 Finding"记录——flag 只是能被 oracle 确认的一种 Finding，不是单独模式。

**终止条件由你定义**：见 [`internal/termination/termination.go`](internal/termination/termination.go) 的 `ShouldTerminate(bb, state)` 钩子（默认永不提前终止，只受 `MAX_BUMPS` 步数兜底与墙钟预算约束）。在那里写你的策略——覆盖度、边际收益递减、发现数阈值、确认目标达成等。

### 证据即真实流量

每次 `http_request` / `tcp_send` 的响应都带一个 `[traffic tN]` ID，全程记进 per-mission 的流量日志（落盘 JSONL）。`add_finding` / `verify_finding` 用 `evidence_refs=[{ref, role}]` **引用真实的请求/响应对**作为证据（role：`baseline` / `proof` / `verification` / `supporting`），报告里直接带可复现报文，而非散文。`verify_finding` 判决为 `reproduced`（→已验证）/ `refuted` / `inconclusive`（确认不了，保留候选并写明阻塞原因）；验证是**追加**记录，Finding 原始证据不可变。

### Scope Lock（范围锁定）

纯结构性：executor 自身的出网一律限制在授权 scope 内——平台题下是题目 `connection_info` 的主机，engagement 下是配置的主机 + 授权 CIDR 段。`http_request` / `tcp_send` / `run_python` 三处由同一个 `internal/scope` 判定（host 精确匹配 + CIDR 数值匹配），无任何绕过；scope 为空则 fail-loud 拒绝一切出网。`run_python` 的 socket 护栏用**闭包封装**，护栏内部句柄/允许集不泄漏为全局——是"防意外漂移"的结构性护栏，不是对抗蓄意逆向的沙箱（真隔离需 OS 级边界）。

## 构建

```bash
# 本沙箱内 go 模块走 git 直连（proxy.golang.org 不可达时）：
go env -w GOPROXY=direct GOSUMDB=off

go build -o chronos ./cmd/chronos
```

TSecBench 托管评测的镜像、离线预检、运行变量和提交材料见 [`docs/tsecbench-hosted.md`](docs/tsecbench-hosted.md)。执行 `./scripts/package-tsecbench.sh` 只会构建并导出镜像，不会启动评测。

## 运行

```bash
# 1) 对一个平台工作源跑机械调度器（默认；内置 TSecBench 适配器，可换）
chronos

# 2) 机械调度器 + Web 控制台（默认 http://127.0.0.1:8000）
chronos --web

# 3) 一次真实授权评估（无平台/调度器；可加 --web 看实时发现）
chronos --engagement examples/engagement.example.json --web
```

### 控制台：以「发现」为中心

控制台不讲「题目解没解」——真实业务(渗透/漏挖/代审)没有 solved/unsolved/deferred，只有**发现的判定**：**确认 confirmed / 疑似 suspected / 失败 failed**(= 内核 verified / candidate+inconclusive / refuted 的呈现归并)。页面顶部是三类判定计数 + 发现总数 + facts + tokens;主体是发现列表(严重度 + 判定徽章 + 证据数);右侧是 executor 列表,点进去可实时看它的推理流并 pause/resume/kill/发指令。两种模式(scheduler / engagement)都用这套发现视图。

启动后日志会额外打印一次带随机 `#operator=...` 片段的 **Authenticated operator console** 地址。只有从这个地址打开的浏览器会话可以发消息、暂停/恢复任务、修改模型或更新 Scope；普通 `http://127.0.0.1:8000` 页面保持只读。

- **直接对话**：输入框会触发一个独立的无工具模型回合，Agent 必须给操作员文字回复；同一条消息同时进入执行队列。任务暂停时仍可对话，动作会等待恢复。
- **Scope 设置**：engagement 控制台顶部的 `Scope` 按钮可替换当前授权主机/IP/URL/CIDR。更新立即应用到 HTTP、TCP 和 `run_python` 的结构性范围锁。
- **边界不由聊天改变**：聊天或提示词永远不能扩大 Scope；超范围请求会被拒绝，必须由持有操作员令牌的人在 Scope 界面显式修改。
- **安全启动**：带 `--web` 的 engagement 默认暂停；操作员检查或修改 Scope 后点击 `resume` 才开始自主执行。

两条运行主链路共用同一内核（scope / 黑板 / traffic / solver / swarm）：

- **调度器模式（默认）**：`platform`(**可插拔工作源**，内置 TSecBench 适配器) → `poller`(轮询事件) → `scheduler`(按分值→难度填满并发窗口、滚动重试、全局预算、死锁自愈、**平台判真即 kill 该 swarm**) → 每个目标一个协作 `swarm`。`submit_flag` 经 swarm 去重 + 递增冷却 + 猜测熔断打到平台。
- **engagement 模式**：`--engagement` 跑单个多目标 mission，**不依赖任何平台**，无 oracle，产物累积进黑板 + 本地报告（`<state_dir>/reports/<name>/report.{json,md,html}`）。scope 为空的 engagement **开跑前直接拒绝**。

> **Agent 不绑定 TSecBench**：TSecBench 只是 `internal/platform` 里**内置的一个评测平台适配器**（`PLATFORM_MODE=tsec`）。调度器 / swarm / 控制台都与平台无关——换平台只需实现 `platform.Platform` 接口并在 `platform.New` 注册;真实业务(engagement)根本不需要平台。

### CLI

```
chronos [flags]
  --engagement PATH    一次授权评估（JSON 配置；无平台/调度器）
  --model SPEC         模型规格，可重复（默认 deepseek/deepseek-chat）
  --allow-exec         显式允许 run_python（不安全；仅在隔离环境使用）
  --no-exec            关闭 run_python（兼容参数；默认已关闭）
  --time-limit N       单任务墙钟秒数（默认取配置）
  --max-challenges N   并发窗口（默认取配置，硬上限 3）
  --preflight          仅离线检查配置，不访问平台、模型或题目
  --web                启动 Web 控制台
  --web-host/--web-port
```

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `DEEPSEEK_API_KEY` | — | DeepSeek Key（主） |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com/v1` | |
| `QWEN_API_KEY` / `QWEN_BASE_URL` | — / dashscope 兼容端点 | |
| `GLM_API_KEY` / `GLM_BASE_URL` / `GLM_MODEL_ID` | — / bigmodel v4 / `glm-4-plus` | |
| `PLATFORM_MODE` | `tsec` | 平台适配器(内置仅 TSecBench;可自行扩展) |
| `BENCHMARK_BASE_URL` / `BENCHMARK_TOKEN` | TSecBench / — | TSecBench 适配器的地址与令牌 |
| `TSECBENCH_HOSTED` | `false` | 托管模式下自动把模型 Base URL 改写到 `*.tsecbench.gw` |
| `CHRONOS_STATE_DIR` | `.chronos-state` | 黑板 / 流量 / 报告 / 工作区根目录 |
| `MAX_CONCURRENT_CHALLENGES` | `3` | 并发窗口（硬上限 3） |
| `CHALLENGE_TIME_LIMIT_SECONDS` | `1800` | 默认单任务墙钟预算 |
| `KILLCHAIN_TIME_LIMIT_SECONDS` | `2700` | 多阶段渗透题放宽的墙钟 |
| `TOTAL_BENCHMARK_BUDGET_SECONDS` | `0`（关闭） | 全局预算；设置后钳制单题时限 + 预算告急停开新题 |
| `MAX_CHALLENGE_ATTEMPTS` | `3` | 每题重试次数上限 |
| `DEFER_RETRY_COOLDOWN_SECONDS` | `120` | deferred 重试冷却 |
| `AUTO_RESUME_IDLE_SECONDS` | `240` | 死锁自愈阈值 |
| `MAX_BUMPS` | `20` | 每个 executor 的硬性 bump 上限（兜底） |
| `ALLOW_LOCAL_EXEC` | `false` | 是否允许 run_python；仅在 OS/容器隔离中开启 |
| `TARGET_INSECURE_TLS` | `false` | 是否跳过目标 TLS 证书校验 |
| `PLATFORM_INSECURE_TLS` | `false` | 是否跳过平台 TLS 证书校验 |

## 包结构（`internal/`）

| 包 | 职责 |
|---|---|
| `scope` | 授权范围匹配（host + CIDR），三处出网工具唯一判定；空 = 拒绝一切 |
| `blackboard` | Fact / Finding（evidence_refs + 追加式 verifications、原证据不可变）/ Sub-Goal；JSON 落盘 |
| `traffic` | per-mission 流量日志（真实请求/响应对作证据），JSONL 落盘 |
| `engagement` | JSON 配置加载 + 空 scope 拒绝 + ToMeta |
| `prompts` | 统一中性契约（Finding 为核心；带 flag 才出现 submit_flag） |
| `termination` | `ShouldTerminate(bb, state)` 用户钩子（默认永不提前停） |
| `report` | JSON + Markdown + 自包含 HTML（含证据流量、验证历史） |
| `nettools` | DoRawHTTP / DoTCPSend（纯 IO，不走代理） |
| `llm` | 模型规格 → go-openai 客户端 + 模型 id（DeepSeek/Qwen/GLM） |
| `solver` | 单 executor 的工具循环（function calling）：http/tcp/run_python + 黑板/流量工具 |
| `swarm` | 并行协作 executor（不竞速、互不取消）；平台模式下 submit 去重/冷却/熔断 |
| `platform` | 可插拔平台工作源接口 + `New(mode,…)` 工厂；内置 TSecBench REST 适配器(其一) |
| `poller` | 后台轮询，发 new/solved 事件 |
| `scheduler` | 机械调度器（无 LLM）：填窗口 / reap / 滚动重试 / 预算 / 死锁自愈 / 算子命令 |
| `console` | 控制台数据契约（Source 接口 + 发现排序）；scheduler 与 engagement 都实现它 |
| `engrun` | 让 engagement 模式也能开 `--web`（单 mission 的发现视图 + 算子操作） |
| `web` | 内嵌控制台后端（net/http + SSE，无 websocket 依赖）；前端是 `web/` 的 Next.js 应用，静态导出后嵌入二进制；UI 以发现判定(确认/疑似/失败)为中心 |

运行态目录（默认 `.chronos-state/`）：`blackboards/`、`traffic/`、`reports/`。

## 平台规则适配（调度器模式）

- 并发窗口严格 ≤ 3 个活跃靶场容器；题目解决 / 超时后自动 close 释放名额并补入下一题。
- 容器防重：同一题进程内只 start 一次（地址缓存复用）。
- 多 flag 语义：`correct` 但未通关返回部分得分提示，executor 继续协作直到 `challenge_complete`。
- 提交熔断：一题累计 4 次错误提交（跨模型合计）即锁定本题提交，防止"猜 flag"烧光预算。
- deferred 滚动重试：窗口有空位且无"新鲜"题可填时自动捞回 deferred 的题（受冷却 + 次数上限约束），无需人工 `/resume`。
- 死锁自愈：所有未解题都 deferred 且无活跃 swarm 持续 `AUTO_RESUME_IDLE_SECONDS` 后，自动清空 deferred 重新补位。
- 全局预算感知（可选）：设置 `TOTAL_BENCHMARK_BUDGET_SECONDS` 后，预算只剩 `MIN_REMAINING_SECONDS_TO_SPAWN` 时停开新题，并把单题时限钳制到实际剩余。

## 现状与边界

未启动真实 TSecBench 评测，也未访问真实赛题（需平台生成任务并由操作员明确开始）。适配层提供官方 API 契约测试、托管网关改写测试与离线 `--preflight`；其余已验证项包括 `go build ./...`、`go vet ./...`、`go test ./...`、engagement 空 scope 拒绝、报告生成、Web API 与 `run_python` Scope 护栏。
