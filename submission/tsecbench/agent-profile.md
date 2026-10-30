# Agent 提交资料

## 名称

Chronos

## 一句话简介

面向授权攻防评测的多任务自主 Agent，以结构化黑板共享事实、发现和证据，并在严格 Scope 与平台并发约束内完成探索、验证和 Flag 提交。

## 详细介绍

Chronos 是以 Go 为核心的自主安全评测 Agent。它将每道题建模为持续搜索过程，由轻量调度器管理最多三个并发靶场，并由同题执行器共享持久化黑板与真实流量证据。Agent 提供 HTTP、TCP、隔离容器内的 Python 和 Playwright MCP 浏览器探索工具；浏览器经 Scope 代理访问授权目标。模型调用支持多 Key 逐请求轮询与限流/上游故障切换。Flag 只有在平台接口确认后才记为已验证成果；多 Flag 题会持续执行至全部提交完成。系统支持总时限、单题时限、重试冷却、错误提交熔断、SIGTERM 清理和容器自动关闭，不包含历史题解、答案库、RAG 或预置攻击流程。

## 技术标签

```text
Go, OpenAI-compatible LLM, autonomous agent, blackboard, evidence tracking, scope enforcement, multi-challenge scheduler
```

## 合规声明

```text
镜像不包含 BENCHMARK_TOKEN、模型密钥、历史赛题解答、Flag、答案记忆或外部持久化服务。所有敏感配置均在平台运行时通过环境变量注入。Agent 仅访问平台下发的题目地址与允许的模型网关。
```
