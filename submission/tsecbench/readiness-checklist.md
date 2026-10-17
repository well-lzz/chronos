# TSecBench 提交前检查表

- [ ] `go test ./...`、`go test -race ./...`、`go vet ./...` 全部通过。
- [ ] `./scripts/package-tsecbench.sh` 成功生成 Linux AMD64 镜像归档。
- [ ] `.tar.gz` 小于 3 GB，SHA-256 已记录。
- [ ] 使用占位变量运行 `--preflight`，输出 `Result: ready`。
- [ ] 镜像中没有 `.env`、真实 API Key、`BENCHMARK_TOKEN`、历史答案或 Flag。
- [ ] 平台 Agent 名称和简介已按 `agent-profile.md` 填写。
- [ ] 平台运行变量仅包含模型密钥及必要的模型配置。
- [ ] 双 Key 模式分别填写 `DEEPSEEK_API_KEY` 与 `DEEPSEEK_API_KEY_2`，没有把两个 Key 粘在同一变量中。
- [ ] 已单独完成平台要求的本地模式验证。
- [ ] 操作员确认后再点击开始评测；准备阶段不点击开始或入队按钮。
