# Harness 加固与验证（2026-11-02）

对应本轮提出的四项弱点。下面区分代码已实现、在本机实测通过，以及仍未证明的边界。改动尚在工作区，未提交或部署。

**后续更新**：`--engagement` 的整任务 OS 进程隔离已接入，两小时真实模型周期负载与落盘复核已通过（12/12 轮）；当前边界与结果以 [整任务隔离说明](harness-process-isolation.md) 为准，下文保留首轮加固时点的数据。

## 控制台身份与请求边界

全部 `/api/*` 读写（包括流量、模型记录、审批、范围、控制命令和 SSE）先鉴权再进入业务处理。默认生成 256 位随机操作员令牌；可通过 `CHRONOS_CONSOLE_TOKEN` 固定为 32–512 个 URL 安全字符。只监听回环地址，严格匹配监听主机及实际端口，拒绝其他 Host、跨源 Origin 和跨站 Fetch Metadata。

终端输出 `http://127.0.0.1:8000/#token=…`。浏览器用令牌建立 HttpOnly、SameSite=Strict 会话后移除地址栏 fragment；凭证不放入静态资源、localStorage 或 URL 查询参数。进程重启会话失效。所有浏览器写入须带 CSRF 令牌及 `application/json`，会话引导始终需要操作员 Bearer 令牌，不能仅靠已有 cookie。CLI 可用 Bearer 访问 API；身份校验不豁免 Host/Origin 检查。

负向测试覆盖未鉴权写入无副作用、敏感读取被拒、错误令牌、旧会话、Host 重绑定、错误端口、跨源/空 Origin、CSRF 缺失、表单内容类型、未知字段和尾随 JSON。浏览器 cookie 建立及合法范围修改也经真实 HTTP 测试。这里是单操作员能力令牌，并未实现多用户角色、远程登录或对本机同一系统账户的隔离。

## 核心编排测试

新增真实 Go 工具循环测试，模型由可控本地 HTTP 服务替代，目标为测试进程自己的 HTTP fixture。覆盖：

- 主循环真实流量采集、发现入图、另一执行器复现并解决验证目标。
- 429 恢复、400 失败、401 等待配置与取消收尾。
- Scope 在模型请求进行期间改变后，阻止旧响应发起越界请求。
- 会话目录不可写时不联系模型；存储失败、预算耗尽的协作者不能接收验证任务。

同时修复启动时序：全部协作者发布到运行表之后才启动首个循环，避免首个发现向尚未发布的协作者派发验证。

`go test -race ./... -coverprofile=…` 的结果：

| 包/函数 | 修补前 | 修补后 |
| --- | ---: | ---: |
| `internal/swarm` | 30.0% | 64.4% |
| `internal/solver` | 67.3% | 67.8% |
| `internal/engrun` | 75.8% | 75.8% |
| `internal/web` | 19.2% | 47.3% |
| `swarm.runExecutor` | 0% | 50.8% |
| `swarm.routeVerification` | 0% | 100% |
| `swarm.UpdateScope` | 0% | 100% |
| `solver.RunUntilDoneOrGaveUp` | 68.7% | 68.7% |
| `engrun.UpdateScope` | 79.5% | 79.5% |
| `web.protect`（新鉴权中间件） | — | 97.3% |

并非所有核心代码此前都无测试，solver 主循环与 engagement 范围切换已有覆盖。新增 `scripts/check_core_coverage.py` 在 CI 检查包级与关键函数门槛，防止本轮改善退化。覆盖率不证明行为必然正确；供应商故障、取消、范围变更及副作用断言才是这些测试的主要价值。

## 真实模型链路与重复运行

已用本地保存的 `deepseek-flash` 配置完成真实供应商请求，未使用既有外部靶场，也未修改已有任务配置：

| 实测项 | 结果 |
| --- | --- |
| 运行开始 | 2026-11-02 00:32:23 +0800 |
| 目标 | 进程拥有的临时 loopback HTTP fixture，固定公开合成标记 |
| 完整轮次 | 3/3 通过 |
| 模型调用 | 25 次 |
| 供应商报告 token 总数 | 103,072 |
| 目标请求 | 6 次 |
| 实际时长 | 88.0 秒 |
| 链路 | 发现 → 原始流量 → 独立执行器重新请求并验证 → JSON/Markdown/HTML 报告 → 从磁盘重载证据图 |

本机原始结果在 `dist/validation-20261102-evidence/live-result.json`，逐轮证据、会话和报告在同目录 `round-001` 至 `round-003`。结果不记录 API key、供应商地址或原始供应商错误。独立执行器使用独立会话、同一模型；本轮未证明跨模型一致性。

测试期间保留了失败尝试。最初的测试适配器原地过滤工具数组造成重复工具名；修复为新数组。较小工具预算和原执行器自验也暴露了用例配置问题，现将验证目标明确分派给另一执行器并给出有界预算。上述失败不能当成生产模型故障，也没有计入三轮通过结果。

入口默认跳过，普通单测不产生供应商费用。显式运行示例（在仓库根目录；输出目录应为空，避免覆盖先前证据）：

```bash
CHRONOS_RUN_LIVE_E2E=1 \
CHRONOS_LIVE_PROFILE_DIR="$PWD/.chronos-state" \
CHRONOS_LIVE_OUTPUT="$PWD/dist/live-validation-$(date +%s)" \
CHRONOS_LIVE_ROUNDS=3 CHRONOS_LIVE_MINUTES=5 \
go test -v ./internal/e2e -run TestLiveModelEvidenceLifecycle -count=1 -timeout 6m
```

重复负载可设 `CHRONOS_LIVE_ROUNDS=100 CHRONOS_LIVE_MINUTES=120` 并把 Go 测试超时设为 `121m`。轮数范围 1–100，整体时间上限 1–120 分钟；每个模型请求上限 45 秒、输出上限 1536 token，每个执行器最多 10 个工具步，总请求上限为轮数 × 12，供应商累计用量达到轮数 × 50,000 token 后不再发起请求（最后一次响应可能使阈值略有超出）。结束后检查实际时长和完成轮数，不能把最大时间预算当作运行时长。

网络工具被严格限制为 **GET 此进程创建的完整 fixture URL**；其他地址、路径、方法、TCP、Python、浏览器及任意未允许工具都会在派发前拒绝。允许的额外工具仅用于本轮黑板和已采集流量的读取、记录、验证与子目标收尾。目标没有重定向，响应内容完全由测试代码控制。

**88 秒不是数小时 soak。** 本轮没有证明真实平台、真实授权生产目标或长时间运行的稳定性，也没有证明发现质量。重复运行入口可提供这类后续观测，当前结果不能替代它们。

## Python 隔离

移除 Python 层 socket monkey patch，不再把它当安全边界。模型代码在独立 OS 沙箱子进程中运行：

| 平台 | 边界 |
| --- | --- |
| macOS | `sandbox-exec` 默认拒绝，仅读解释器/标准库/系统运行库，私有临时目录可读写；禁止联网、宿主工作区/凭证读取与新建子进程。 |
| Linux | bubblewrap 用户、PID、网络等命名空间，丢弃 capabilities；仅挂载运行时库与私有目录，不挂载宿主 home、状态目录或工作区。子进程继续在同一命名空间内。 |

不继承宿主环境。每次执行最长 60 秒、CPU 10 秒、文件大小 8MiB、文件描述符 64、输出保留 24,000 字节；Linux 另限制数据段 256MiB、地址空间 512MiB。macOS 尚无可靠的内存硬限额，不宜把此边界视为资源耗尽防护。沙箱缺失、启动失败或命名空间被宿主禁用时拒绝执行，绝不回退为裸跑。`--preflight` 在显式启用 Python 时实际启动一次无网络沙箱探针。Python 仍默认关闭；目标通信改走受 Scope 控制的 Go HTTP/TCP 工具。

本机 **macOS 内核级测试通过**：宿主 canary 文件读取/写入失败，`ctypes` 直接 libc 读取失败，凭证环境不可见，真实 socket 连接不到本地测试服务，私有临时文件可读写；输出截断与取消也通过。本轮在 macOS 上完成 Linux 交叉编译，未本地运行 Linux 内核测试；CI 新增 macOS/Linux 强制沙箱测试，环境不支持时失败，不能用跳过当作通过。Docker 镜像安装 bubblewrap 且默认禁用 Python；宿主未授权命名空间时仍不可启用。

Scope 切换后的 mission 编排仍在同一受信任 Go 进程，**没有实现整任务容器/VM、独立控制进程或多租户隔离**。Python 子进程边界也不保护浏览器进程或 Go 工具本身的漏洞。需要跨租户/强资源隔离时，仍需按任务部署独立容器/VM；不能仅凭 generation ID、不同目录或 `NoNewPrivileges` 宣称已实现。

## confirmed 的语义与检查

报告 JSON 新增 `verdict_semantics`，Markdown、HTML 和控制台发现详情均展示说明：`confirmed` 仅表示满足当前程序证据规则，不等于绝对正确。模型仍参与证据选择与观察解释，使用者应复核引用证据和复现结果。证据图让复核可追溯，不替代复核；规则和后续反驳机制保持不变。

本轮通过：全量竞态测试、`go vet ./...`、`go build ./...`、Linux amd64 交叉编译、覆盖率门槛、Next.js 生产构建、macOS OS 沙箱实测、三轮真实模型链路。前端静态导出已同步至 Go 嵌入目录。

本机 Go 为 1.26.5，验证使用 `GOTOOLCHAIN=local`，未声称在仓库指定的 1.26.10 工具链上验证；CI 继续读取仓库版本。前端构建使用 Node 20.20.2。
