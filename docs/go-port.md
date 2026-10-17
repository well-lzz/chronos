# Go 版本

把 chronos 从 Python 迁到 Go：**Assessment 核心**（真实授权评估主链路）**加机械外壳**（TSecBench 平台 + 机械调度器 + Web 控制台）。两种运行模式共用同一内核（scope / 黑板 / traffic / solver / swarm）。

## 为什么是 Go

- **并发模型最贴**：并行 executor + 共享黑板/流量，goroutine + mutex 比 asyncio 更直白。
- **OpenAI 兼容**：`go-openai` 直吃 DeepSeek / Qwen / GLM 的兼容端点；agent 循环自己写（~1 个包），不引框架——契合 Less-Is-More。
- **单静态二进制**：丢到机器上就能跑，无需 venv/uv。
- 与作者的 [ARTEX](https://github.com/well-lzz/ARTEX) 同栈，便于维护。

唯一外部依赖：`github.com/sashabaranov/go-openai`。engagement 配置用 **JSON**（stdlib，无额外依赖）。

## 构建与运行

```bash
# 本沙箱内 go 模块走 git 直连（proxy.golang.org 不可达时）：
go env -w GOPROXY=direct GOSUMDB=off

go build -o chronos ./cmd/chronos
./chronos --engagement examples/engagement.example.json
```

环境变量（见下）至少要配一个模型的 API Key，否则 executor 会以 "no API key" 退出（其余链路仍会跑通并产出空报告）。

### CLI

```
chronos                               # 默认：对 TSec 平台跑机械调度器
chronos --web                         # 调度器 + Web 控制台（默认 http://127.0.0.1:8000）
chronos --engagement <config.json>    # 一次真实授权评估（无平台/调度器）
  --model SPEC         模型规格，可重复（默认 deepseek/deepseek-chat）
  --allow-exec         显式允许 run_python（不安全；仅在隔离环境使用）
  --no-exec            关闭 run_python（兼容参数；默认已关闭）
  --time-limit N       单任务墙钟秒数（默认取配置）
  --max-challenges N   并发窗口（默认取配置，硬上限 3）
  --web-host/--web-port
```

### 两条运行主链路

- **调度器模式（默认）**：`platform`(TSec) → `poller`(轮询 new/solved 事件) → `scheduler`(按分值/难度填满并发窗口、deferred 滚动重试、全局预算、死锁自愈、平台判真即 kill 该 swarm) → 每题一个协作 `swarm`。`submit_flag` 经 swarm 去重 + 递增冷却 + 猜测熔断打到平台;flag 只是平台可判真的一种 Finding,不竞速。
- **engagement 模式**：`--engagement` 跑单个多目标 mission,无 oracle,产物进黑板 + 本地报告。

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `DEEPSEEK_API_KEY` | — | DeepSeek Key（主） |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com/v1` | |
| `QWEN_API_KEY` / `QWEN_BASE_URL` | — / dashscope 兼容端点 | |
| `GLM_API_KEY` / `GLM_BASE_URL` / `GLM_MODEL_ID` | — / bigmodel v4 / `glm-4-plus` | |
| `CHRONOS_STATE_DIR` | `.chronos-state` | 黑板/流量/报告/工作区根目录 |
| `MAX_BUMPS` | `20` | 每个 executor 的硬性 bump 上限（兜底） |
| `CHALLENGE_TIME_LIMIT_SECONDS` | `1800` | 默认墙钟预算 |
| `ALLOW_LOCAL_EXEC` | `false` | 是否允许 run_python；仅在 OS/容器隔离中开启 |
| `TARGET_INSECURE_TLS` | `false` | 是否跳过目标 TLS 证书校验 |
| `PLATFORM_INSECURE_TLS` | `false` | 是否跳过平台 TLS 证书校验 |

## 包结构（`internal/`）

- `scope` — 授权范围匹配（host + CIDR），三处出网工具唯一判定；空=拒绝一切。
- `blackboard` — Fact / Finding（含 evidence_refs + 追加式 verifications，原证据不可变）/ Sub-Goal；JSON 落盘。
- `traffic` — per-mission 流量日志（真实请求/响应对作证据），JSONL 落盘。
- `engagement` — JSON 配置加载 + **空 scope 拒绝** + ToMeta。
- `prompts` — 统一中性契约（Finding 为核心；带 flag 才出现 submit_flag）。
- `termination` — `ShouldTerminate(bb, state)` 用户钩子（默认永不提前停）。
- `report` — JSON + Markdown + HTML（含证据流量、验证历史）。
- `nettools` — DoRawHTTP / DoTCPSend（纯 IO）。
- `llm` — 模型规格 → go-openai 客户端 + 模型 id。
- `solver` — 单 executor 的工具循环（function calling）：http/tcp/run_python + 黑板/流量工具；可选 SubmitFn（平台提交）与 Record（供 Web 观测）。
- `swarm` — 并行协作 executor（不竞速、互不取消），termination 钩子 + bump 兜底；平台模式下负责 submit 去重/冷却/熔断，并暴露 Status/Activity/Pause/Bump 给控制台。
- `platform` — 竞赛平台接口 + TSecBench REST 适配器（challenges/start/submit/hint/close，pull→meta）。直连、不走代理。
- `poller` — 后台轮询，发 new/solved 事件。
- `scheduler` — 机械调度器（无 LLM）：填窗口/reap/滚动重试/预算/死锁自愈/算子命令；平台判真即 kill。
- `web` — 内嵌控制台后端（net/http + **SSE**，无 websocket 依赖）：`/api/state`、`/api/stream`、`/api/activity`、`POST /api/command`；`/` 及静态资源由 `go:embed all:dist` 提供（见下）。

## 前端（Next.js 静态导出）

控制台前端是 `web/` 下的 **Next.js（App Router，React 19）** 应用，`output: 'export'` 静态导出后**嵌进 Go 二进制**，单二进制部署不变。

```bash
cd web && npm ci && npm run build        # 产出 web/out/
rm -rf ../internal/web/dist && cp -r out ../internal/web/dist
cd .. && go build -o chronos ./cmd/chronos
```

- `internal/web/dist`（构建产物）已随仓库提交，所以**无需 Node 也能 `go build` 跑起来**；改前端后按上面重建并替换 `dist`。
- 开发：`cd web && npm run dev`（:3000），`next.config.mjs` 的 rewrites 把 `/api/*` 代理到 `chronos --web`（:8000）；静态导出时同源,无需代理。
- UI 同 SSE 实时快照 + `/api/activity` 轮询活动流 + `POST /api/command` 下发算子操作（pause/resume/kill/bump），暗色/亮色自适应。

## 与 Python 版的等价性

- **scope lock**：host + CIDR，`http_request`/`tcp_send` 在 Go 侧判定；`run_python` 的 socket 由 python 预序列护栏锁定——护栏用**闭包封装**，内部句柄/允许集不泄漏为全局（修掉了 Python 版那段预序列可被模型代码绕过的问题）。它是"防意外漂移"的结构性护栏，**不是**对抗蓄意逆向的沙箱（真隔离需 OS 级边界）。
- **证据链**：流量 ID → `evidence_refs{ref, role}`（baseline/proof/verification/supporting）→ 报告带可复现报文。
- **验证**：`verify_finding` 判决 `reproduced|refuted|inconclusive`，追加记录、原证据不可变。
- **终止**：无内置，交给 `termination.ShouldTerminate`。

## 未实测边界

沙箱内无 LLM Key / TSec VPN / 目标网络，故**未跑真实模型、真实平台与真实目标**。已验证：`go build ./...`、`go vet ./...`、`go test ./...`（scope/blackboard/traffic/engagement/report 单测）、engagement 空 scope 拒绝、engagement 全链路空跑产出报告、python 护栏实测（CIDR 内放行 / 越界拦截 / 内部不泄漏为全局）、Web 控制台起服务并返回 `/`、`/api/state`、`/api/command`。TSec 平台适配器与调度器针对不可达平台会干净地空转(轮询报错降级为空、不误判完成)。
