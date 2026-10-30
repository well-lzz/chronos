# 可废止证据图(Defeasible Evidence Graph)

> **状态（2026-10-30 核对）**：本方案已实现：`internal/blackboard/graph.go`，见提交 `7e452b5`。**正式规则以实现契约 [`docs/evidence-graph.md`](../evidence-graph.md) 为准**；本文记录设计动机与对照分析。 本文保留设计取舍记录，实现细节以代码为准。

> 设计基线文档(纯设计,不含实现)。Chronos 的图设计差异化方案:把黑板从「探索/血缘图」升级为「**可证伪的证据论证图**」——一条结论的判定(confirmed/suspected/refuted)**由图按固定规则算出,而不是模型写上去的 label**。承接现有 `blackboard`(Fact/Finding/Sub-Goal + 血缘边)、`evidence_refs`(真实流量证据)、`verify_finding`(验证生命周期)。

---

## 0. 动机:为什么不再做"又一张探索图"

同类开源项目 [Cairn](https://github.com/oritera/Cairn)(腾讯云黑客松 AI 渗透赛 54/54 AK)已经把「状态空间搜索 + Blackboard + fact-intent 探索图 + stigmergy」这条路跑到极致。它的图极简且有效:

- `Fact{id, description}` —— **只有一段文字描述**;无证据、无置信度、无状态、无严重度。
- `Intent{from[], to, description, …}` —— 探索方向,连接 fact,从 origin 朝 goal 生长。
- 执行与证据都在 worker 容器(Claude Code/Codex/Pi)里,**不进图**。

**Cairn 图的结构性盲点**:Fact 是「谁写的就算真」——图里没有「凭什么信它、它会不会错」。这对 **CTF/夺旗**无所谓(flag 有 oracle,自证真伪);但对**渗透 / 代审 / 漏挖**——**没有 oracle、误报才是核心痛点**——这个盲点就是致命的。

Chronos 若再做一张更好的探索图,只会是「更差的 Cairn」。**换轴**才有设计亮点:

> Cairn 的图回答「**接下来往哪搜**」;Chronos 的图回答「**凭什么信这条结论,它会不会被推翻**」。

这根轴踩在 **Chronos 独有、Cairn 结构上拿不到的资产**上:**证据 = 真实流量(带 baseline/proof/verification 角色)+ Finding 的 candidate→verified→refuted 生命周期**。Cairn 把执行/证据丢给 worker 容器,图里天然没有;Chronos 自己握着工具面与 traffic,**所以只有 Chronos 能把证据做进图**。

---

## 1. 核心主张

**一条结论的判定是证据论证图上的可计算、可复现属性,而非模型的断言。**

- 模型只负责:提出断言(claim)、把真实流量连成证据边(grounds)、提出对其它断言的支持/反驳(supports/refutes)。
- harness 负责:对图跑一个**固定的纯函数**,算出每条断言的判定。
- 于是「confirmed / suspected / failed」**谁来跑都一样**,报告就是这张可审计的图本身。

理论背书:Dung 抽象论证框架 / defeasible reasoning——一条断言的地位,由它在「支持-攻击」图里**是否被未被反驳的证据接地**决定。

---

## 2. 图模型(节点 / 边)

### 节点

| 节点 | 含义 | 对应现状 |
|---|---|---|
| `claim` | 一条断言:事实或发现 | 现有 Fact / Finding |
| `evidence` | 一段真实流量片段 | 现有 `[traffic tN]` |

claim 携带描述性属性(severity、category、target…),但**不携带 verdict 字段**——verdict 不再存储,而是**查询时算出**(或增量维护,见 §4)。

### 边(有向)

| 边 | 从 → 到 | 含义 | 角色 |
|---|---|---|---|
| `grounds` | evidence → claim | 这段真实流量**接地**该断言 | `baseline` / `proof` / `verification` / `supporting`(沿用现有 evidence_refs 角色) |
| `supports` | claim → claim | 一条断言支持另一条 | — |
| `refutes` | claim → claim | 一条断言**反驳/废止**另一条 | — |
| `verifies` | verification → claim | 一次独立验证作用于某断言 | 承载判决 reproduced/refuted/inconclusive |
| `derived_from` | claim → claim | 血缘(谁催生谁) | 复用 10-26 的 `from_ids` |

> `grounds` 其实就是现有 `evidence_refs` 的图化;`verifies` 就是 `verify_finding` 的图化。本设计**不新发明数据,只把已有字段重新组织成边,并补上 `supports`/`refutes`**。

---

## 3. 判决:verdict 是图的纯函数

对每条 claim,按如下**可废止**规则计算(区分证据的角色强度):

```
proof-grounded(c)   := 存在 grounds(role∈{proof,verification}) 的 evidence 指向 c
has-live-refuter(c) := 存在一条 refutes→c 的断言 r,且 r 自身未被"胜出的"反驳废止
                       (递归:r 是"存活"的)

verdict(c) =
  refuted    若 has-live-refuter(c) 且 该反驳方 proof-grounded
  confirmed  若 proof-grounded(c) 且 ¬has-live-refuter(c)
  suspected  其余:裸断言(无 proof)/ 只有 supporting 证据 / 有未决 inconclusive 验证 / 挑战未解
```

要点:
- **proof 接地是 confirmed 的硬门槛**——没有真实 proof 报文的断言永远到不了 confirmed,只能 suspected。这把「证据即真实」从口号变成**判决闸门**。
- **可废止**:一条 confirmed 可被后来的 proof-grounded refuter 翻成 refuted;反驳方自己也可被再反驳(递归 reinstatement)。判决随图演进而变,但永远是当前图的确定函数。
- 与现有呈现层对齐:`FindingsSorted`、控制台的 确认/疑似/失败 直接消费算出的 verdict,**消除「重注入用 recency、判定用 severity、verdict 靠模型写」的三处不一致**。

> 冲突裁决(两条互相 refutes 且都 proof-grounded)默认判 `suspected` 并生成一个「裁决」Sub-Goal,交回搜索去找决定性证据——**不让图自己猜,把矛盾暴露给模型**。

---

## 4. 工程落地(增量维护 + 性能)

- **存储**:claim/evidence 存黑板;边单独一张邻接表,落盘(复用 `CHRONOS_STATE_DIR`)。
- **增量 verdict**:每次新增 `grounds`/`refutes`/`verifies` 边,只重算**受影响连通子图**的 verdict(反驳是局部的),而非全图扫描;缓存 verdict,边变更即失效对应节点。
- **与 cold-digest 协同**:digest 折叠冷节点时,**必须保留判决相关的边**(proof grounds、live refutes),只折叠纯叙述;verified / critical/high 本就不折叠,天然安全。
- **与 recency-free 重注入协同**:注入给模型的工作记录里,每条 Finding 带**算出的 verdict + 一句"为什么是这个判定"**(有 proof / 被某条反驳挑战 / 等待验证),让模型知道该去补哪条证据。

---

## 5. 模型侧工具(最小增量)

现有工具基本够用,只需微调语义 + 补两个:

| 工具 | 变化 |
|---|---|
| `add_finding(... evidence_refs)` | 不变;`evidence_refs` 即建 `grounds` 边。**不再接受 verdict 入参**——verdict 只能算出来 |
| `verify_finding(id, verdict, evidence_refs)` | 不变;建 `verifies` 边,其 reproduced/refuted 参与判决 |
| `challenge_finding(target_id, reason, evidence_refs)` | **新增**:对某断言建 `refutes` 边(鼓励独立验证者去证伪,不只是证实) |
| `relate_findings(a, b, kind)` | **新增**:建 `supports`,显式表达断言间依赖 |

契约原则不变:**中性、不含方法论**;模型提供断言与证据连边,判决交给 harness。

---

## 6. 为什么这是真亮点(与 Cairn 对照)

| | Cairn 的探索图 | Chronos 的证据图 |
|---|---|---|
| 节点 | Fact = 文字描述 | claim + 接地的真实流量 evidence |
| 信任模型 | 谁写的就算真 | **proof 接地才可 confirmed,可被证伪** |
| 判定 | 无 | 图的**纯函数**,可复现可审计 |
| 证据位置 | 在 worker 容器,不进图 | **在图里**,报告即图 |
| 擅长 | CTF(有 oracle 自证) | **渗透/代审/漏挖(无 oracle,误报为王)** |
| 结构可行性 | 瘦图 + 强 worker | 握着工具面与 traffic,**只有它做得成** |

四点总结:
1. **不同维度**,不是更密的探索图——是另一种图(认识论结构 vs 血缘结构)。
2. **把独有资产变成组织原则**:别人抄得走黑板,抄不走「判决=真实流量论证图上的可计算属性」。
3. **打 Cairn 回避的战场**:没有 oracle 的真实业务,误报是核心痛点,本图直接产出可审计判决 + 可复现报文。
4. **不破坏极简**:判决规则是机械外壳里一个纯函数(非剧本);顺手修掉「发现者自验」的确认偏置。

---

## 7. 迁移路径(演进,非重写)

Chronos 已有大部分零件:

- Finding = claim ✓
- `evidence_refs` + role = `grounds` 边 ✓
- `verify_finding`(reproduced/refuted/inconclusive) = `verifies` 边雏形 ✓
- `from_ids` 血缘(10-26)= `derived_from` 边 ✓

**只需三步**:
1. 把 `verdict` 从黑板的**存储字段**改成**算出的属性**(§3 纯函数);现有呈现层改为消费算出值。
2. 补 `refutes` / `supports` 边 + `challenge_finding` / `relate_findings` 工具(§5)。
3. 增量 verdict 维护 + 与 cold-digest/重注入协同(§4)。

搜索主干(solver/swarm/scheduler)完全不动——这是给黑板**加一层判决语义**,不是换引擎。

---

## 8. 与其它设计文件的关系

- `docs/design/orchestration-plan.md` 的 **P1 独立验证**(生成者≠评判者)在这里有了归宿:独立验证者就是去建 `verifies` / `refutes` 边的人,判决闸门让「证实」和「证伪」对等重要。
- `docs/design/subagent-delegation.md` 的子 Agent 写黑板回传 ID:子 Agent 产出的 claim/evidence 直接进同一张证据图,判决统一计算。
- cold-digest(10-26)折叠冷节点时保留判决相关边——两者正交共存。

---

## 9. 开放问题

- **证据强度分级**:proof 是否再细分(直接命中 vs 推断)?先保持 baseline/proof/verification/supporting 四级,够用再细化。
- **冲突裁决的默认**:两条 proof-grounded 互相反驳 → 默认 suspected + 裁决 Sub-Goal;是否允许操作员在控制台手动裁决(建一条带 operator 来源的 `verifies`)。
- **时间维度**:目标状态会变(一个漏洞被修),proof 是否带时效?可给 evidence 打时间戳,verdict 计算时标注「证据采集于 T」。
- **可视化**:控制台把这张图画出来(节点按 verdict 着色、refutes 边高亮),让「为什么是这个判定」一眼可见——天然的审计视图。
