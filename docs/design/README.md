# 设计方案

2026-10-25 至 10-27 的一轮 harness 审计与演进中写下的设计方案。它们记录**为什么这样做、比较过哪些替代方案、刻意不做什么**。截至 2026-10-30，五份方案均已在 `main` 实现；实现细节以代码和各实现契约为准，这里保留的是设计取舍。

| 方案 | 解决的问题 | 状态 | 落地位置 |
|---|---|---|---|
| [可废止证据图](evidence-graph-design.md) | 判决靠模型写标签：有确认偏置、不可审计；与「探索图」系统缺少差异化 | ✅ 已实现（`7e452b5`） | `internal/blackboard/graph.go`；正式规则见 [实现契约](../evidence-graph.md) |
| [子 Agent 委派](subagent-delegation.md) | 单执行器上下文封顶，缺少「隔离子窗口、只回传结论」的无损路径 | ✅ 已实现（`4a37269`） | `internal/solver` 的 `delegate` 工具 |
| [任务编排优化](orchestration-plan.md) | 并行写冲突、发现者自验、崩溃即重来、无理性终止 | ✅ 已实现（`c7f7122`） | 租约 `internal/solver/leases.go`、独立验证与恢复 `internal/swarm`、会话接力 `internal/solver/handoff.go` |
| [借鉴 ARTEX](artex-borrow-plan.md) | 缺调用计量、跨模型故障转移、通用工具闸门、保真压缩 | ✅ 已实现（`7e452b5`） | `internal/llmrec`、`internal/llm/pool.go`、`internal/guard`、`internal/blackboard/digest.go` |
| [问题与解决方案汇总](problems-and-solutions.md) | 以上全部问题的总索引（问题 → 根因 → 方案） | 历史快照 | 表内状态为 10-27 时点，现均已实现 |

## 共同的设计边界

所有方案都遵守同一组约束，这也是它们取舍的依据：

- **借设计，不借依赖**：全部用 `go-openai` 与 Go 标准库实现，`go.mod` 仍只有一个直接依赖。
- **借机制，不借反极简特性**：不引入 Skill 库、RAG、预设攻击流程或固定角色分工。
- **判决交给机械规则，推理留给模型**：harness 提供工具、边界、记录与可计算的判决；方法论由模型临场决定。

## 相关文档

- [Harness 设计](../harness-design.md) —— 当前运行契约
- [证据图实现契约](../evidence-graph.md) —— 判决规则的正式定义
- [任务模型](../mission-modes.md) —— 「任务 = 搜索」的设计基线
