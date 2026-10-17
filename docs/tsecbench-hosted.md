# TSecBench 托管评测接入

本说明对应第三届智能攻防挑战赛的 Agent 托管模式。目标是准备可上传材料；构建、导出和 `--preflight` 均不会调用赛题 API，也不会开始评测。

## 已适配的协议

- 从运行环境读取平台注入的 `BENCHMARK_BASE_URL` 与 `BENCHMARK_TOKEN`。
- 使用 `BENCHMARK_TOKEN` 请求头调用 `/openapi/v1/challenges`、`start`、`submit`、`hint`、`close`。
- 最多并行处理 3 个题目；优先复用已经处于 `available` 状态的容器。
- 支持多 Flag 题目的累计提交，平台确认全部 Flag 后才把题目标为完成。
- SIGTERM、总预算耗尽或正常退出时并行关闭本进程接管的题目容器。
- `TSECBENCH_HOSTED=true` 时，把模型地址自动转换为平台网关：`https://api.example.com/v1` → `http://api.example.com.tsecbench.gw/v1`。
- 托管镜像启动后直接运行调度器，不启动 Web UI，不依赖人工输入。

## 构建提交镜像

```bash
./scripts/package-tsecbench.sh
```

产物：

- `dist/chronos-tsecbench-amd64.tar.gz`
- `dist/chronos-tsecbench-amd64.tar.gz.sha256`

镜像为 `linux/amd64`，使用非 root 用户运行，包含 CPython 标准库供受 Scope 锁约束的 `run_python` 使用。构建上下文不会复制 `.env`、本地状态、日志或 Git 历史。平台限制为 3 GB；脚本会在导出后检查大小。

## 平台运行变量

平台会自动注入以下变量，不要手工写入镜像，也不要提交到仓库：

- `BENCHMARK_TOKEN`
- `BENCHMARK_BASE_URL`

在托管表单中增加两个独立变量。DeepSeek 双 Key 推荐配置：

```text
DEEPSEEK_API_KEY=<第一个 DeepSeek Key>
DEEPSEEK_API_KEY_2=<第二个 DeepSeek Key>
```

只有一个 Key 时仅填写 `DEEPSEEK_API_KEY` 即可。配置两个 Key 后，每个模型 HTTP 请求会在 Key 之间轮询分流；遇到 401、403、408、429、网络错误或 5xx 时，同一请求会自动改用另一个 Key。失败切换不会并发重复请求。若两个 Key 属于同一 DeepSeek 账户，它们仍可能共享账户级额度，分流不能绕过服务商总配额。

可选地显式设置公共地址；Chronos 会在托管模式自动改写为网关地址：

```text
DEEPSEEK_BASE_URL=https://api.deepseek.com/v1
```

Qwen 或 GLM 可分别使用 `QWEN_API_KEY`、`QWEN_BASE_URL`，或 `GLM_API_KEY`、`GLM_BASE_URL`、`GLM_MODEL_ID`，同时在容器启动参数追加 `--model qwen/qwen-plus` 或 `--model glm/glm-4-plus`。若平台表单不能配置启动参数，保持默认 DeepSeek。

## 离线预检

预检只解析配置、检查状态目录和 Python，不访问模型、平台或题目：

```bash
docker run --rm \
  -e BENCHMARK_BASE_URL=https://tsecbench.zc.tencent.com \
  -e BENCHMARK_TOKEN=placeholder-for-offline-check \
  -e DEEPSEEK_API_KEY=placeholder-key-one \
  -e DEEPSEEK_API_KEY_2=placeholder-key-two \
  chronos-tsecbench:submission --preflight
```

成功输出必须包含：

```text
Chronos preflight (offline; no evaluation was started)
Hosted routing: true
keys configured=2
Result: ready
```

## 正式开始前

1. 先完成平台要求的本地模式评测；获取接入配置会创建评测任务，因此只在准备实际验证时操作。
2. 在“我的数据”创建 Agent，使用 `submission/tsecbench/agent-profile.md` 中的简介。
3. 上传导出的 `.tar.gz`，填写模型密钥，确认没有上传 `.env` 或历史解答。
4. 核对镜像架构、压缩包 SHA-256、体积和离线预检输出。
5. 最后由操作员点击“提交程序，进入运行队列”或“获取接入配置，开始评测”。在此之前 Chronos 不会主动触发评测。

平台页面：<https://tsecbench.zc.tencent.com/benchmark/prepare/tch3?mode=hosted&evaluation_type=agent>
