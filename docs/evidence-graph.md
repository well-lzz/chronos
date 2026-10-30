# 可废止证据图：实现契约

黑板在同一个任务的 JSON 中持久化 `edges`、`challenges` 和既有 Fact/Finding/Verification。`status`、`verdict`、`verdict_reason` 是读取时从图计算的呈现字段，不写入黑板文件。旧黑板首次读取时会把 `evidence_refs`、验证记录和血缘转换为图边并写回。

## 边与判决

- `grounds`: 流量 ID → claim；只有 `proof`、`verification` 角色具备确认强度，`baseline` 和 `supporting` 保留审计价值。
- `refutes`: claim → claim；有证据的反驳可以废止结论，反驳本身也可以被反驳。
- `supports`: claim → claim；表达论证关联，但不代替目标自己的 proof 门槛。
- `verifies`: 验证记录 ID → Finding；`reproduced` 的验证流量为 Finding 接地，`refuted` 创建带验证流量的反驳 claim，`inconclusive` 只留下待决记录。
- `derived_from`: 源 claim → 新 claim；保留原有 `from_ids` 血缘。

受影响的 `refutes` 连通分量采用同步三值迭代：被已确认且有 proof 的反驳攻击为 `refuted`；自身有 proof 且所有反驳被废止为 `confirmed`；其余为 `suspected`。无根的互相反驳保持 `suspected`，两条都有 proof 的双向冲突会生成 `adjudication` 子目标。平台确认的 Flag 使用 `oracle:<finding-id>` 作为独立 proof 节点；无平台 oracle 的 Flag 不自动确认。

图边增量更新内存邻接索引，仅重算受影响的攻击连通分量。proof 接地节点和参加反驳的节点不会被 cold digest 折叠。黑板摘要、控制台发现详情以及 JSON/Markdown/HTML 报告都展示当前计算判决及原因；报告 JSON 同时导出完整 `evidence_graph`。
