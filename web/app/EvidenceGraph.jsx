"use client";

import { useMemo, useState } from "react";
import s from "./page.module.css";

const VERDICT_LABEL = { confirmed: "已确认", suspected: "待验证", failed: "已排除" };
const TYPE_LABEL = { finding: "Finding", fact: "Fact", challenge: "Challenge", evidence: "Evidence", verification: "Verification" };
const EDGE_LABEL = { grounds: "证据接地", supports: "支持", refutes: "反驳", verifies: "验证", derived_from: "派生" };
const EDGE_KINDS = ["grounds", "supports", "refutes", "verifies", "derived_from"];
const NODE_WIDTH = 210;
const NODE_HEIGHT = 64;
const MAX_GRAPH_NODES = 72;

function clip(value, length = 25) {
  const text = String(value || "");
  return text.length > length ? `${text.slice(0, length - 1)}…` : text;
}

function edgePath(edge, positions) {
  const from = positions.get(edge.from);
  const to = positions.get(edge.to);
  if (!from || !to) return "";
  if (from.x === to.x) {
    const x1 = from.x + NODE_WIDTH / 2;
    const x2 = to.x + NODE_WIDTH / 2;
    const bend = x1 + 105;
    return `M ${x1} ${from.y} C ${bend} ${from.y}, ${bend} ${to.y}, ${x2} ${to.y}`;
  }
  const forward = from.x < to.x;
  const x1 = from.x + (forward ? 1 : -1) * NODE_WIDTH / 2;
  const x2 = to.x + (forward ? -1 : 1) * NODE_WIDTH / 2;
  const middle = (x1 + x2) / 2;
  return `M ${x1} ${from.y} C ${middle} ${from.y}, ${middle} ${to.y}, ${x2} ${to.y}`;
}

export default function EvidenceGraph({ graphs, initial }) {
  const missions = useMemo(() => Object.keys(graphs || {}).sort(), [graphs]);
  const [chosenMission, setChosenMission] = useState(initial?.mission || missions[0] || "");
  const mission = missions.includes(chosenMission) ? chosenMission : missions[0] || "";
  const graph = graphs?.[mission] || { claims: [], evidence: [], verifications: [], edges: [] };
  const [selectedId, setSelectedId] = useState(initial?.id || null);
  const [query, setQuery] = useState("");
  const [type, setType] = useState("all");
  const [verdict, setVerdict] = useState("all");
  const [directoryOpen, setDirectoryOpen] = useState(false);

  const nodes = useMemo(() => [
    ...(graph.evidence || []).map((node) => ({ ...node, lane: "evidence", kind: "evidence", label: node.id })),
    ...(graph.claims || []).map((node) => ({ ...node, lane: "claim", label: node.text })),
    ...(graph.verifications || []).map((node) => ({ ...node, lane: "verification", kind: "verification", label: node.summary || node.outcome })),
  ], [graph]);
  const edges = graph.edges || [];
  const nodeById = useMemo(() => new Map(nodes.map((node) => [node.id, node])), [nodes]);
  const selected = nodeById.get(selectedId) || null;
  const selectedRelations = useMemo(() => selected ? edges.filter((edge) => edge.from === selected.id || edge.to === selected.id) : [], [edges, selected]);

  const filteredNodes = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return nodes.filter((node) => {
      if (type !== "all" && node.kind !== type) return false;
      if (verdict !== "all" && node.verdict !== verdict) return false;
      if (!normalized) return true;
      return [node.id, node.label, node.reason, node.summary, node.outcome].some((value) => String(value || "").toLowerCase().includes(normalized));
    });
  }, [nodes, query, type, verdict]);

  const graphNodes = useMemo(() => {
    if (filteredNodes.length <= MAX_GRAPH_NODES) return filteredNodes;
    const prioritized = new Set();
    if (selectedId) {
      prioritized.add(selectedId);
      for (const edge of edges) {
        if (edge.from === selectedId) prioritized.add(edge.to);
        if (edge.to === selectedId) prioritized.add(edge.from);
      }
    }
    return [
      ...filteredNodes.filter((node) => prioritized.has(node.id)),
      ...filteredNodes.filter((node) => !prioritized.has(node.id)),
    ].slice(0, MAX_GRAPH_NODES);
  }, [filteredNodes, selectedId, edges]);

  const layout = useMemo(() => {
    const lanes = { evidence: [], claim: [], verification: [] };
    for (const node of graphNodes) lanes[node.lane].push(node);
    const x = { evidence: 135, claim: 480, verification: 825 };
    const positions = new Map();
    for (const [lane, laneNodes] of Object.entries(lanes)) {
      laneNodes.forEach((node, index) => positions.set(node.id, { x: x[lane], y: 48 + index * 88 }));
    }
    return { positions, height: Math.max(160, ...Object.values(lanes).map((lane) => lane.length * 88 + 24)) };
  }, [graphNodes]);

  const selectNode = (id) => {
    setSelectedId(id);
    setQuery("");
    setType("all");
    setVerdict("all");
  };
  const changeMission = (value) => {
    setChosenMission(value);
    setSelectedId(null);
  };
  const stats = {
    claims: graph.claims?.length || 0,
    evidence: graph.evidence?.length || 0,
    verifications: graph.verifications?.length || 0,
    refutes: edges.filter((edge) => edge.kind === "refutes").length,
  };

  if (missions.length === 0) return <div className={s.graphEmpty}><strong>当前后端尚未提供证据图</strong><p>请重启到包含证据图快照的 Chronos 版本；现有发现详情仍可正常查看。</p></div>;

  return <div className={s.graphExplorer}>
    <div className={s.graphStats} aria-label="图谱汇总">
      <span><b>{stats.claims}</b> Claims</span><span><b>{stats.evidence}</b> 证据</span><span><b>{stats.verifications}</b> 验证</span><span className={stats.refutes ? s.graphConflictStat : ""}><b>{stats.refutes}</b> 反驳</span>
    </div>
    <div className={s.graphToolbar}>
      {missions.length > 1 && <label><span>Mission</span><select aria-label="选择图谱 Mission" value={mission} onChange={(event) => changeMission(event.target.value)}>{missions.map((item) => <option key={item} value={item}>{item}</option>)}</select></label>}
      <label className={s.graphSearch}><span>搜索</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="节点 ID、内容或判决原因" /></label>
      <label><span>节点类型</span><select aria-label="筛选节点类型" value={type} onChange={(event) => setType(event.target.value)}><option value="all">全部类型</option>{Object.entries(TYPE_LABEL).map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select></label>
      <label><span>图判决</span><select aria-label="筛选图判决" value={verdict} onChange={(event) => setVerdict(event.target.value)}><option value="all">全部判决</option><option value="confirmed">已确认</option><option value="suspected">待验证</option><option value="failed">已排除</option></select></label>
    </div>
    <div className={s.graphLegend} aria-label="关系图例">
      {EDGE_KINDS.map((kind) => <span key={kind}><i className={s[`edge_${kind}`]} />{EDGE_LABEL[kind]}</span>)}
      <small>选择节点查看完整关系链</small>
    </div>

    {nodes.length === 0 ? <div className={s.graphEmpty}><strong>这个 Mission 暂无图谱数据</strong><p>Agent 添加 Fact、Finding、证据引用或挑战后，关系会显示在这里。</p></div> : <div className={s.graphWorkspace}>
      <div className={s.graphMain}>
        {filteredNodes.length === 0 ? <div className={s.graphEmpty}><strong>没有匹配的节点</strong><p>清空搜索或调整节点类型与判决筛选。</p></div> : <>
          <div className={s.graphViewport}>
            <svg className={s.graphSvg} width="960" height={layout.height} viewBox={`0 0 960 ${layout.height}`} role="group" aria-label={`${mission} 证据关系图`}>
              <defs>{EDGE_KINDS.map((kind) => <marker key={kind} id={`graph-arrow-${kind}`} markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto" markerUnits="strokeWidth" className={s[`edge_${kind}`]}><path d="M0,0 L7,3.5 L0,7 z" /></marker>)}</defs>
              <g className={s.graphEdges} aria-hidden="true">{edges.map((edge, index) => {
                const path = edgePath(edge, layout.positions);
                return path ? <path key={`${edge.kind}-${edge.from}-${edge.to}-${edge.role || ""}-${index}`} d={path} className={`${s.graphEdge} ${s[`edge_${edge.kind}`] || ""}`} markerEnd={`url(#graph-arrow-${edge.kind})`} /> : null;
              })}</g>
              {graphNodes.map((node) => {
                const position = layout.positions.get(node.id);
                return <g key={node.id} transform={`translate(${position.x}, ${position.y})`} className={`${s.graphNode} ${s[`node_${node.kind}`] || ""} ${node.verdict ? s[`v_${node.verdict}`] || "" : ""} ${selectedId === node.id ? s.graphNodeSelected : ""}`} role="button" tabIndex={0} aria-pressed={selectedId === node.id} aria-label={`${TYPE_LABEL[node.kind]} ${node.id}：${node.label}`} onClick={() => setSelectedId(node.id)} onKeyDown={(event) => {
                  if (event.key !== "Enter" && event.key !== " ") return;
                  event.preventDefault();
                  setSelectedId(node.id);
                }}>
                  <title>{node.id} · {node.label}</title>
                  <rect x={-NODE_WIDTH / 2} y={-NODE_HEIGHT / 2} width={NODE_WIDTH} height={NODE_HEIGHT} rx="7" />
                  <text className={s.graphNodeKind} x="-91" y="-11">{TYPE_LABEL[node.kind]} · {node.id}</text>
                  <text className={s.graphNodeLabel} x="-91" y="13">{clip(node.label)}</text>
                  {node.verdict && <circle className={s.graphVerdictDot} cx="88" cy="-18" r="4"><title>{VERDICT_LABEL[node.verdict] || node.verdict}</title></circle>}
                </g>;
              })}
            </svg>
          </div>
          <div className={s.graphCanvasMeta}><span>Evidence → Claims ← Verification</span><span>画布显示 {graphNodes.length} / {filteredNodes.length} 个节点</span></div>
          {filteredNodes.length > MAX_GRAPH_NODES && <p className={s.graphLimitNote}>为保证实时性能，画布优先显示选中节点及其直接关系。打开节点目录可查看并定位全部筛选结果。</p>}
          <details className={s.graphDirectory} onToggle={(event) => setDirectoryOpen(event.currentTarget.open)}>
            <summary>节点目录 · {filteredNodes.length}</summary>
            {directoryOpen && <div>{filteredNodes.map((node) => <button type="button" key={node.id} className={selectedId === node.id ? s.active : ""} onClick={() => selectNode(node.id)}><span>{TYPE_LABEL[node.kind]}</span><strong>{node.label}</strong><small>{node.id}</small></button>)}</div>}
          </details>
        </>}
      </div>

      <aside className={s.graphInspector} aria-live="polite">
        {selected ? <>
          <span className={s.eyebrow}>{TYPE_LABEL[selected.kind]} NODE</span>
          <div className={s.graphInspectorTitle}><h3>{selected.id}</h3>{selected.verdict && <span className={`${s.verdict} ${s[`v_${selected.verdict}`] || ""}`}><i />{VERDICT_LABEL[selected.verdict] || selected.verdict}</span>}</div>
          <p className={s.graphNodeText}>{selected.label || "—"}</p>
          {selected.reason && <section><h4>图判决依据</h4><p>{selected.reason}</p></section>}
          {selected.outcome && <dl><div><dt>验证结果</dt><dd>{selected.outcome}</dd></div><div><dt>验证者</dt><dd>{selected.by || "Agent"}</dd></div></dl>}
          {selected.evidence && <section><h4>验证证据</h4><pre>{selected.evidence}</pre></section>}
          <section><h4>直接关系 · {selectedRelations.length}</h4>{selectedRelations.length === 0 ? <p className={s.muted}>当前节点没有关系边。</p> : <div className={s.graphRelations}>{selectedRelations.map((edge, index) => {
            const outgoing = edge.from === selected.id;
            const other = nodeById.get(outgoing ? edge.to : edge.from);
            return <button type="button" key={`${edge.kind}-${edge.from}-${edge.to}-${index}`} onClick={() => selectNode(other?.id || (outgoing ? edge.to : edge.from))}><span className={s[`edgeText_${edge.kind}`] || ""}>{EDGE_LABEL[edge.kind] || edge.kind} · {outgoing ? "指向" : "来自"}</span><strong>{other?.label || (outgoing ? edge.to : edge.from)}</strong><small>{other?.id || (outgoing ? edge.to : edge.from)}{edge.role ? ` · ${edge.role}` : ""}{edge.note ? ` · ${edge.note}` : ""}</small></button>;
          })}</div>}</section>
        </> : <div className={s.graphInspectorEmpty}><strong>选择一个节点</strong><p>查看判决原因、证据文本与直接关系。点击关系项可以继续沿证据链追溯。</p><span>键盘：Tab 定位节点，Enter 选择。</span></div>}
      </aside>
    </div>}
  </div>;
}
