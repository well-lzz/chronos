# Go 版本（Assessment 核心）

把 chronos 的 **Assessment 核心**从 Python 迁到 Go。这一版聚焦"真实授权评估"主链路，**暂不含** TSecBench 平台适配器、机械调度器、Web 控制台（那三块是机械外壳，后续按需补）。

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
chronos --engagement <config.json>   # 跑一次授权评估
  --model SPEC      模型规格，可重复（默认 deepseek/deepseek-chat）
  --no-exec         关闭 run_python
  --time-limit N    本次任务墙钟秒数（默认取配置）
```

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
| `ALLOW_LOCAL_EXEC` | `true` | 是否允许 run_python |

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
- `solver` — 单 executor 的工具循环（function calling）：http/tcp/run_python + 黑板/流量工具。
- `swarm` — 并行协作 executor（不竞速、互不取消），termination 钩子 + bump 兜底。

## 与 Python 版的等价性

- **scope lock**：host + CIDR，`http_request`/`tcp_send` 在 Go 侧判定；`run_python` 的 socket 由 python 预序列护栏锁定——护栏用**闭包封装**，内部句柄/允许集不泄漏为全局（修掉了 Python 版那段预序列可被模型代码绕过的问题）。它是"防意外漂移"的结构性护栏，**不是**对抗蓄意逆向的沙箱（真隔离需 OS 级边界）。
- **证据链**：流量 ID → `evidence_refs{ref, role}`（baseline/proof/verification/supporting）→ 报告带可复现报文。
- **验证**：`verify_finding` 判决 `reproduced|refuted|inconclusive`，追加记录、原证据不可变。
- **终止**：无内置，交给 `termination.ShouldTerminate`。

## 未实测边界

沙箱内无 LLM Key / 目标网络，故**未跑真实模型与真实目标**。已验证：`go build ./...`、`go vet ./...`、`go test ./...`（scope/blackboard/traffic/engagement/report 单测）、engagement 空 scope 拒绝、全链路空跑产出报告、python 护栏实测（CIDR 内放行 / 越界拦截 / 内部不泄漏为全局）。
