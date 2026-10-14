# 攻防 Agent — TSecBench

面向智能体攻防（Agentic Offensive Security）的**极简**自动化系统。核心信念：决定成绩的是模型本身的 agentic 推理能力，不是外层臃肿的 Agent 架构，也不是一堆 Skill / 预设流程。因此本项目刻意做减法——**0 Skill、0 RAG、0 MCP、无任务角色分解、无预设攻击流程、无跨任务记忆**；只保留「模型 + 中性工具面 + 遵守平台规则所必需的机械外壳」。

设计基线与取舍记录见 [`docs/rewrite-plan.md`](docs/rewrite-plan.md)。

当前主攻 [TSecBench](https://tsecbench.zc.tencent.com/)（腾讯云鼎实验室智能攻防 Agent 评测平台）跑分。`Tsecbench v1` 题集共 63 题、23000 分、总时限 360 分钟，覆盖六大能力域：Web 漏洞挖掘、二进制漏洞挖掘、漏洞利用、多阶段渗透、云攻击、对抗规避。多 flag 题按 flag 部分给分，查看 hint 按比例扣分。

同一内核也能跑**真实授权评估**（不止跑分靶场）：见下文「单一行为」与 `--engagement`。无论跑分题、渗透测试还是代码审计，都被统一建模为**同一个搜索过程**，没有模式开关。

## 任务模型：一切都是搜索

Agent 的核心循环被建模为「**搜索 + 随手捕获每一个产物**」，而不是「追逐那一个终极 flag」。`submit_flag` 只是"捕获一个恰好等于可验证终极目标的产物"的特例；多 flag 题与多阶段渗透题本就是"搜索产出多个中间产物、Goal 弥散（要集齐）"的结构，所以发现一个 flag 就立即提交、继续搜索，直到集齐或预算耗尽。

用四个概念描述这个搜索（设计基线见 [`docs/mission-modes.md`](docs/mission-modes.md)）：**Fact**（已确认的世界状态）、**Goal / Sub-Goal**（搜索的终止条件，可动态增删）、**Step**（从已有 Fact 产生新 Fact 的一步）、**Finding**（搜索途中的产物；漏洞类任务里就是安全漏洞）。这些状态显式记录在一块 **Blackboard（黑板）** 上——跨 Step、跨同题 solver 共享、落盘持久化，让长程搜索的记忆不受上下文窗口限制。黑板是**领域中立的认知结构**（如何表示状态/目标/发现），不是攻击方法论，与「0 Skill / 无预设流程」的极简信念不冲突。

### 单一行为（无模式开关）

没有 capture/assessment 之分，全系统只有**一种行为**：在授权 scope 内搜索，把 Fact/Finding 记到黑板，多个同构 executor **协作**（不竞速、互不取消）。若目标恰好带 flag（可被平台验证的终极产物），`submit_flag` 把它作为"已验证 Finding"记录——flag 只是能被 oracle 确认的一种 Finding，不是单独模式。

**终止条件由你定义**：见 [`backend/termination.py`](backend/termination.py) 的 `should_terminate(blackboard, state)` 钩子（默认永不提前终止，只受 `MAX_BUMPS` 步数兜底与墙钟预算约束）。在那里写你的策略——覆盖度、边际收益递减、发现数阈值、确认目标达成等。

> 机械外壳保留：TSecBench 平台适配器、机械调度器、Web 控制台仍可作为"可选工作来源/观测"。带 flag 的平台题下，"平台判真即停"由调度器的 poller（检测到 `challenge_solved` 即 kill 该 swarm）在外部完成，不再由 swarm 内部竞速处理。

## 执行形态

**Headless（无沙箱）模式，唯一形态**：不需要 Docker、不需要本地工具链，Solver 只依靠大模型自身的推理能力 + 极简中性工具面（`http_request` 直连靶场、`tcp_send` 原始协议交互、`run_python` 本机计算）完成解题。底层模型默认 DeepSeek（`deepseek/deepseek-chat`），架构与模型解耦。

## 系统架构

```text
TSecBench API
   ↓
Poller 轮询器（题目列表 / 已解状态 / 分值 / 难度）
   ↓
机械调度器（无 LLM）：按分值降序 + 难度升序填满并发窗口，
   管单题时限 / 全局预算 / 容器生命周期 / 死锁自愈 / REPL
   ↓
Challenge Swarm 题目群组（一题一群）
   ↓
HeadlessSolver × N（同构，可并行竞速；默认单模型）
   ↓
模型 + 中性工具面：http_request / tcp_send / run_python / submit_flag / task_status
```

调度层是**纯机械**的，没有 LLM 协调器做策略推理——确定、可复现、可调试。同题的多个 Solver 之间通过 Message Bus 共享发现（sibling findings / bump），这是同构 worker 的知识共享，不是任务角色分解。

辅助通道：Operator REPL、Trace 日志（JSONL）、Cost Tracker。

## 平台规则适配

- **单题时间预算**默认 1800s（30 分钟，可用 `--time-limit` 覆盖）；超时仍未解出即关闭靶场、标记 `DEFERRED`，并发名额立即让给下一道未解题（之后由"deferred 滚动重试"自动重开，也可 `/resume` 手动重开）
- **并发窗口**严格限制为最多 3 个活跃靶场容器（按容器数限流）；题目解决或超时后自动 close 释放名额，并自动补入下一题
- **容器防重**：同一题在进程内只 start 一次（地址缓存复用）；容器启动中的等待走只读轮询
- **多 flag 语义**：`correct` 但未通关返回部分得分提示，Solver 继续竞速直到 `challenge_complete`
- **hint**：`request_hint` 是一个普通工具，获取平台提示（会扣分、每题限 1 次），由模型自行决定是否值得——不设门槛、不做 benchmark 行为调参
- **deferred 滚动重试**：窗口一旦有空位且没有"新鲜"（从未尝试过）的题可填，就自动把 deferred 的题捞回来重试——**无需操作员 /resume**。每题最多 `MAX_CHALLENGE_ATTEMPTS`（默认 3）次,之后维持 deferred 直至人工重开；距上次 defer 要过 `DEFER_RETRY_COOLDOWN_SECONDS`（默认 120s）冷却才重试,避免刚失败就被塞回原位空转
- **死锁自愈**：所有未解题都被 defer 且无活跃 swarm 的状态持续 240 秒（`AUTO_RESUME_IDLE_SECONDS`）后，自动清空 deferred 列表重新补位（兜底：连 attempts 用尽的题也会在收尾阶段再试一轮）
- **早夭重试**：swarm 不足 15 步就异常结束（容器未就绪/瞬时报错）时自动重拉重试一次
- **提交熔断**：一题累计 4 次错误提交（跨模型合计）即锁定本题提交并让 swarm 退场，防止"猜 flag"烧光时间预算
- **部分得分止损**：多 flag 题拿到部分分后，若 600 秒（`PARTIAL_STALL_SECONDS`）内没有新 flag 被接受，带着已有分数撤退换题
- **差异化时限**：多阶段渗透（killchain）类题目自动放宽到 2700 秒（`KILLCHAIN_TIME_LIMIT_SECONDS`），其余题 1800 秒
- **DeepSeek 多 Key 轮换**：`DEEPSEEK_EXTRA_API_KEYS` 可配额外 API Key（逗号分隔），每个 solver 实例按轮转分配 Key；429 重试上限提升到 6 次（指数退避）
- **启动前 VPN 健康检查**：探测 `VPN_HEALTH_CHECK_URL`（默认 `http://10.0.100.58`），未连接 VPN 直接退出
- **全局时间预算感知**（可选，默认关闭）：设置 `TOTAL_BENCHMARK_BUDGET_SECONDS`（如 360 分钟 = 21600）后，调度器会在总预算只剩 `MIN_REMAINING_SECONDS_TO_SPAWN`（默认 300 秒）时停止开新题，并把新题的单题时限钳制到"实际剩余时间"以内
- **调度优先级**：自动补位按"分值降序、难度等级升序"排列候选队列，而非题名字母序

## 快速开始

### 1. 安装依赖

```bash
uv sync
```

### 2. 配置环境变量

```bash
cp .env.example .env
```

在 TSecBench 平台「开始跑分 → 本地模式」创建跑分任务，获取 `BENCHMARK_BASE_URL`、`BENCHMARK_TOKEN`，并按平台指引连接靶场 VPN。然后填写 `.env`：

```env
PLATFORM_MODE=tsec
BENCHMARK_BASE_URL=https://tsecbench.zc.tencent.com   # 以平台下发为准
BENCHMARK_TOKEN=<创建跑分任务后下发的 token>
DEEPSEEK_API_KEY=your_deepseek_key
```

### 3. 启动

```bash
uv run chronos -i
```

默认用 `deepseek/deepseek-chat` 单模型直连官方 API。如需禁用本机代码执行加 `--no-exec`；想跑多模型同题竞速，显式传多个 `--models`。

### 单题调试

```bash
uv run chronos --challenge <题目目录、题名或 unique_code>
```

### 真实授权评估（engagement）

```bash
uv run chronos --engagement examples/engagement.example.yaml
```

用一份 engagement 配置（YAML/JSON）定义一次**授权评估**：弥散目标 + 授权 scope + 交战规则（RoE）。模板见 [`examples/engagement.example.yaml`](examples/engagement.example.yaml)。要点：

- **scope 是强制授权边界，非可选**：assessment 必须声明非空、可解析的 scope（`scope.targets`：主机 / IP / CIDR），否则**开跑前直接拒绝**。agent 自身出网（`http_request` / `tcp_send` / `run_python` 的 socket）被结构性锁死在 scope 内，无旁路——这是法律/职业底线，也让工具始终处于"仅授权范围"。
- 无 flag oracle：产物是 **Finding**（候选 → 验证 → 已验证 / 已否决），全部累积在该 mission 的黑板上。
- 结束后在 `<state_dir>/reports/<name>/` 生成 `report.json` + `report.md` + `report.html`。

### 只读自检

```bash
uv run chronos --smoke-test         # 平台连通性（题目列表 + 已解状态）
uv run chronos --model-smoke-test   # 大模型 API 连通性
```

### Web 控制台（可视化 + 与 solver 对话）

```bash
uv run chronos --web                 # 默认 http://127.0.0.1:8000
uv run chronos --web --web-port 9000
```

内嵌 FastAPI + WebSocket 服务,机械调度器在后台正常跑分,浏览器里实时看到:

- **汇总**:已解 / 未解 / 活跃 / deferred / 成本 / 剩余全局预算
- **题目网格**:按状态着色,带分值 / 难度 / 多 flag 进度,可按状态筛选
- **Solver 面板**:选中某题看每个 solver 的实时状态、步数、成本、进度条;可**暂停 / 恢复 / 终止**该题
- **Solver 控制台**:点某个 solver 的 "Watch",实时流式显示它的推理轨迹(工具调用 / 结果 / flag / usage);底部输入框可**向该 solver 发送一条提示**——这就是"与 Agent 对话",即把一条操作员 guidance 以 bump 的形式注入正在解题的 solver

它是纯**观测 + 操控层**:调度器与 solver 代码零改动(状态靠读共享状态、轨迹靠 tail JSONL、命令走与 REPL 相同的 operator 通道)。调度器本身是机械的,不接自然语言,所以"对话"的对象是**正在思考的 solver**,不是调度器。

## 核心机制

### 机械调度器

无 LLM，确定性编排：轮询题目、按分值/难度优先填满 3 题滚动窗口、管生命周期与预算、死锁自愈。交互式 REPL 命令：

- `/status`：查看当前状态
- `/pause X` / `/resume X`（`/resume all` 清空整个 deferred 列表）
- `/kill X`：停止某个题目的 swarm
- `/quit`：退出调度器

自由文本不再有 LLM 接收，会被忽略。

### Swarm 与 Solver

一道题对应一个 swarm，可包含多个同构 solver 并行竞速；任一 solver 确认全部 flag 即取消其余。solver 卡住（GAVE_UP/ERROR）时会被注入兄弟 agent 的发现继续（bump，最多 5 次）。

### Headless Solver 工具面（中性）

| 工具 | 用途 |
|---|---|
| `http_request` | 全控制 HTTP（方法/头/body），直连 VPN 段靶场 |
| `tcp_send` | 原始 TCP 协议交互（支持 hex payload） |
| `run_python` | 本机 CPython 草稿本（计算/解码/payload 构造），`--no-exec` 可禁用 |
| `submit_flag` | 提交 flag（去重 + 错误提交递增冷却 0/30/120/300/600s） |
| `request_hint` | 平台提示（扣分，由模型自行决定是否使用） |
| `task_status` | flag 进度 x/y + 剩余时间 |
| `traffic_search` / `traffic_get` | 检索/读取**已抓取**的流量（找端点、回看响应、定位证据 ID），避免重复 curl 同一 URL |
| `check_findings` / `notify_coordinator` | 同题兄弟发现共享 / 向调度器汇报 |

solver 始终带一组**黑板工具**（中性，是"认知结构"而非方法论）：`add_fact` / `add_finding`（可记未验证候选）/ `verify_finding` / `add_subgoal` / `resolve_subgoal`。

**证据即真实流量（借鉴 ARTEX）**：每次 `http_request`/`tcp_send` 的响应都带一个 `[traffic tN]` ID,全程记进 per-mission 的 traffic log(落盘 JSONL)。`add_finding` / `verify_finding` 用 `evidence_refs=[{ref, role}]` **引用真实的请求/响应对**作为证据(role: `baseline` 正常对照 / `proof` 漏洞证明 / `verification` 复验 / `supporting` 辅助),报告里直接带可复现报文,而非散文。

**验证更诚实**:`verify_finding` 的判决词是 `reproduced`(→已验证)/ `refuted`(否决)/ `inconclusive`(确认不了,保留候选并写明阻塞原因)。验证是**追加**一条记录,Finding 的原始证据不可变。

系统提示是**一份统一的纯契约**：目标（在授权 scope 内搜索、把产物记进黑板）、工具契约、硬约束（范围锁定、不许编造 Finding、本地磁盘不可信）。**不含任何攻击方法论或题型 playbook**。仅当目标带 flag 时，契约才额外出现 `submit_flag` 一行；否则连 flag 都不提。

**Scope Lock（范围锁定，原 Target Lock 的泛化）**是纯结构性的：solver 自身的出网一律限制在授权 scope 内——平台题下是题目 `connection_info` 的主机，engagement 下是配置的主机 + 授权 CIDR 段。`http_request` / `tcp_send` / `run_python` 三处由同一个 `backend/scope.py` 判定（host 精确匹配 + CIDR 数值匹配），无任何绕过；scope 为空则 fail-loud 拒绝一切出网。

### 过程可追踪

- `logs/trace-<题名>-<模型>-<时间>.jsonl`：逐步工具调用/结果、bump、usage、finish 事件
- Cost Tracker：按 agent 聚合 token 与成本，运行结束打印汇总

## 命令行参考

```text
uv run chronos [OPTIONS]

--models SPEC               模型规格，可多次（如 deepseek/deepseek-chat、qwen/qwen-plus）
--challenge REF             单题模式（目录、题名或 unique_code）
--engagement PATH           从 scope 配置（YAML/JSON）跑一次真实授权评估（engagement）
--max-challenges N          并发靶场窗口（1-3，默认 3，按存活容器数限流）
--time-limit SECONDS        单题墙钟上限（默认 1800，超时关靶场换题）
--total-budget-seconds N    全局总预算（默认关闭；设置后钳制单题时限 + 预算告急停止开新题）
--no-submit                 干跑，不提交 flag
--no-exec                   禁用 run_python
--smoke-test / --model-smoke-test
--web                       启动 Web 控制台（可视化 + 操控 + 盯 solver / 对话）
--web-host / --web-port     Web 控制台监听地址（默认 127.0.0.1:8000）
-i                          交互式 REPL
-v                          详细日志
```

## 目录结构

- `backend/`：机械调度器、headless solver、TSecBench 平台适配器、中性工具
- `backend/blackboard.py`：Fact/Finding/Sub-Goal 黑板（落盘持久化）
- `backend/scope.py`：授权范围匹配（host + CIDR），三处出网工具的唯一判定
- `backend/engagement.py`：engagement 配置加载 + 空 scope 拒绝 + 本地无 oracle 平台
- `backend/traffic.py`：per-mission 流量日志（真实请求/响应对作 Finding 证据）
- `backend/report.py`：mission 报告渲染（JSON + Markdown + HTML，含证据流量与验证历史）
- `backend/web.py` + `backend/webui/`：Web 控制台（FastAPI 服务 + 单页前端）
- `examples/engagement.example.yaml`：engagement 配置模板
- `challenges/`：拉取后的题目元数据
- `logs/`：trace 日志与运行记录
- `.chronos-state/`：run_python 工作区、黑板（`blackboards/`）、流量（`traffic/`）、报告（`reports/`）
- `docs/rewrite-plan.md`：极简重构基线；`docs/mission-modes.md`：Fact/Goal/Step/Finding 设计

## 运行环境要求

- Python 3.14+
- 无需 Docker；至少一个可用的 LLM API Key（推荐 DeepSeek），以及评测平台的 VPN 连通
