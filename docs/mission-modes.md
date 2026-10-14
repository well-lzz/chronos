# Mission 模型：Fact / Goal / Step / Finding

> 本文是"从 CTF 专用 → 通用攻防/代审"重构的**设计基线**。开工前先 review；代码与本文冲突时以本文为准（或先改本文）。承接 [`rewrite-plan.md`](rewrite-plan.md) §0「任务=搜索」。

## 0. 核心抽象

一切任务都是**搜索过程**。用四个概念描述：

- **Fact** — 已确认的事实，即**当前世界状态**（发现的主机/端口/服务版本/端点/凭据/已获访问…）。
- **Goal** — 搜索的**终止条件**；可动态增删 **Sub-Goal**（拿到某立足点 → 新增"在该主机提权"的子目标）。
- **Step** — 下一步做什么：**从已有 Fact 产生新 Fact** 的因果转移（= 一次/多次工具调用 + 解读）。
- **Finding** — 搜索途中记录的**产物**；可动态定义，漏洞类任务里 Finding 就是**安全漏洞**。

统一了两类任务：

| | 产物 vs Goal | 终止 |
|---|---|---|
| **CTF** | 产物 = Goal = flag（flag 既是 Finding 又满足 Goal） | flag 到手 |
| **渗透 / 攻防 / 代审** | Goal 弥散（评估范围）；产物 = 途中的 Finding，持续累积 | 预算 / 覆盖 / 边际收益递减 |

## 1. 映射到现有内核

### 1.1 Blackboard（黑板）—— 显式共享状态
一个 per-mission 的结构化存储：**Facts + Findings + Goals/Sub-Goals**。它替代"只靠 LLM 上下文"的隐式记忆——长程 engagement 的事实量**装不下上下文窗口**，必须显式、可查询、跨 Step 与跨 solver 共享。黑板也天然统一了现有的 `message_bus` / sibling-findings（兄弟共享 = 读写同一黑板）。

### 1.2 Step 循环（取代自由 ReAct 循环）
每一步：
1. **读黑板**：当前 Facts + 未决 Goal/Sub-Goal（注入 prompt）。
2. **模型决定并执行下一个 Step**（自由决策，无预设流程）：http/tcp/run_python 等工具调用。
3. **模型记录产物**：用中性工具把新 Fact / Finding / Sub-Goal 写回黑板。
4. **判终止**：Goal 谓词是否满足。

本质仍是"模型自由驱动搜索",只是把**状态/目标/发现显式化**。

### 1.3 新增中性工具（模型自己维护黑板）
`add_fact(statement, evidence)` · `add_finding(finding)` · `add_subgoal(desc)` · `resolve_subgoal(id, outcome)` · 可选 `list_facts()/list_findings()`。
——这些是**认知结构**工具，不含任何"怎么打"的方法论。现有 http_request/tcp_send/run_python 不变。

### 1.4 终止谓词
- **Capture 模式**：目标 Finding（flag）被确认 → 终止（可保留竞速：先得者结束）。
- **Assessment 模式**：预算耗尽 / 覆盖达标 / **连续 N 步无新 Fact 且无新 Finding**（边际收益递减止损）。

### 1.5 swarm：协作而非竞速
多 solver **共享同一黑板**。Capture 可继续竞速；Assessment 下是**分工覆盖不同攻击面、发现汇聚**，不互相取消。

## 2. 两种 Mission 模式（同一内核，只换 4 个 policy）

| policy | **Capture（现有 CTF）** | **Assessment（渗透/攻防/代审）** |
|---|---|---|
| Goal/终止 | 目标 Finding(flag) 到手 | 预算 / 覆盖 / 无新发现止损 |
| oracle | 有（submit_flag 判真） | 无 |
| 产物 | flag（= 终止目标） | Finding 持续累积，不终止 |
| swarm | 竞速，先得者取消其余 | 协作覆盖，发现汇聚 |

内核不变：LLM + 中性工具 + 机械调度 + scope-lock + tracing + 本地报告。

## 3. scope = 强制授权边界（非可选）
Assessment 模式必须以一份**显式授权范围**为输入（engagement 配置：目标/资产清单 + 交战规则 RoE + 目标描述）。agent 只在范围内行动——现有结构性 target-lock 升级为"engagement scope",**硬约束**。这既是真实渗透的法律/职业底线，也让工具始终处于"仅授权范围"的正道上。

## 4. Finding schema（可动态定义，默认=安全漏洞）
```
Finding {
  id, title, target,
  category,            # 如 web/injection, auth, binary, cloud…（描述性，非预设流程）
  severity,            # info/low/medium/high/critical
  verified | confidence,   # 见待定项 §7.1
  evidence,            # 复现证据 / 轨迹引用（trace step、请求响应片段）
  reproduction,        # 复现步骤
  remediation,         # 修复建议（可选）
  found_by, ts
}
```
flag 是 Finding 的特例（category=flag，verified=平台 oracle）。

## 5. 报告（你已选：本地 JSON + MD/HTML）
黑板的 Findings → `report.json`（结构化）+ `report.md`（可读）+ 可选 `report.html`。含 run 元数据、每条 Finding、覆盖/耗时/token/成本汇总。Capture 模式的报告 = 解出的题 + flag + 证据。

## 6. 与 Less-Is-More 的关系（张力说明）
加入黑板 + Goal/Finding 管理，比"自由 ReAct + 极简工具"**是多了结构**。但它加的是**领域中立的认知结构**（如何表示状态、目标、发现），**不是**攻击方法论、预设流程或 benchmark 拟合——模型每一步仍自由决策"怎么搜"。对弥散目标的长程搜索,显式状态是**必需**（上下文装不下整场 engagement 的事实）。因此与三原则不冲突：0 Skill/RAG/MCP、无角色多 agent、无预设攻击流程均保持。

## 7. 待定决策（请你定）
1. **Finding 门槛**：只记**已验证**漏洞（有可复现证据），还是也记**候选/疑点**（标注 confidence=unverified）？
2. **swarm 协作粒度**：同一目标上"**一个 solver 走到底**" vs "**多 solver 分工子面 + 共享黑板**"？（跨目标并发不变）
3. **终止/覆盖**：纯"预算 + 连续 N 步无新发现"即可，还是要**攻击面覆盖清单**做软目标？（后者易滑回预设流程，慎用）
4. **Sub-Goal 来源**：模型自己提并排序，还是给一层规则/优先级？（倾向：模型自己提，保持通用）
5. **黑板持久化**：Assessment 跑很长、Finding 很值钱——崩溃丢失代价大。是否为黑板恢复一点持久化？（注意我们之前按 Less-Is-More 删了 checkpoint，这里要不要局部回来）

## 8. 对现有代码的影响（迁移面）
- `HeadlessSolver`：自由循环 → **Step 循环 + 黑板注入 + 新工具**；`submit_flag` 降为 Capture 模式的"确认终止目标 Finding"特例。
- 新增：`blackboard.py`（Fact/Finding/Goal 存储）、`findings` schema、`report.py`（渲染）、`mission` 配置与两种 policy。
- `platforms.py` → 泛化为 **mission source**：TSecBench 平台是其一；本地 engagement 配置是其二。
- 机械调度器基本不变（跨目标并发 + 预算 + 滚动重试）。
- 代码审计 = Assessment 的一个**目标类型**（工具面换为读码/静态查询，产物同为 Finding）——后置。

## 9. 落地顺序（review 通过后）
1. 黑板 + Fact/Finding/Sub-Goal 工具 + Step 循环；**先在 Capture 模式跑通，保证 CTF 不回归**。
2. Assessment policy（无 oracle 终止、非竞速、Finding 累积）。
3. engagement 配置 work-source + scope 强制。
4. report 渲染（JSON + MD/HTML）。
5. 代码审计目标类型（后续迭代）。
