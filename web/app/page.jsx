"use client";

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import EvidenceGraph from "./EvidenceGraph";
import s from "./page.module.css";

const EMPTY = {
  summary: {}, findings: [], subgoals: [], traffic: [], swarms: {}, evidence_graphs: {}, mode: "", engagement: null,
};

const VERDICT_LABEL = { confirmed: "已确认", suspected: "待验证", failed: "已排除" };
const VERIFICATION_LABEL = { verified: "已复现", reproduced: "已复现", refuted: "已驳回", inconclusive: "暂无法确认" };
const STATUS_LABEL = {
  running: "运行中", pausing: "暂停中", paused: "已暂停", stopped: "已停止", done: "已完成",
  finished: "已完成", cancelled: "已取消", interrupted: "已中断", flag_found: "已提交结果",
  "needs-config": "待配置", "waiting-provider": "模型重试中", failed: "失败", pending: "等待中",
};
const PRESETS = [
  { id: "deepseek", name: "DeepSeek", base: "https://api.deepseek.com/v1", model: "deepseek-chat" },
  { id: "openai", name: "OpenAI", base: "https://api.openai.com/v1", model: "gpt-4o" },
  { id: "anthropic", name: "Anthropic (OpenAI 兼容)", base: "https://api.anthropic.com/v1/", model: "claude-sonnet-4-5" },
  { id: "qwen", name: "通义千问 Qwen (DashScope)", base: "https://dashscope.aliyuncs.com/compatible-mode/v1", model: "qwen-plus" },
  { id: "glm", name: "智谱 GLM", base: "https://open.bigmodel.cn/api/paas/v4", model: "glm-4-plus" },
  { id: "kimi", name: "月之暗面 Kimi (Moonshot)", base: "https://api.moonshot.cn/v1", model: "moonshot-v1-8k" },
  { id: "openrouter", name: "OpenRouter", base: "https://openrouter.ai/api/v1", model: "openai/gpt-4o" },
  { id: "groq", name: "Groq", base: "https://api.groq.com/openai/v1", model: "llama-3.3-70b-versatile" },
  { id: "mistral", name: "Mistral", base: "https://api.mistral.ai/v1", model: "mistral-large-latest" },
  { id: "siliconflow", name: "SiliconFlow 硅基流动", base: "https://api.siliconflow.cn/v1", model: "deepseek-ai/DeepSeek-V3" },
  { id: "ollama", name: "Ollama (本地)", base: "http://localhost:11434/v1", model: "llama3.1" },
  { id: "custom", name: "自定义", base: "", model: "" },
];
const KIND_LABEL = {
  operator: "操作员", assistant: "Agent 回复", progress: "工作说明", status: "状态", text: "Agent 回复",
  session_init: "会话初始化", input_applied: "消息入轮", turn_start: "轮次开始", turn_end: "轮次结束", result: "本轮结果",
  tool_call: "工具调用", tool_result: "执行结果", error: "错误", context: "上下文",
  delegate_spawn: "委派", delegate_resume: "恢复子任务", delegate_return: "子任务返回", notify: "子任务通知",
  claim: "认领", release: "释放", verify_assign: "验证分派", verify_task: "独立验证", pivot: "调整方向", handoff: "接力", terminate: "结束",
};
const EVENT_FILTERS = [
  { id: "dialogue", label: "对话与进度" },
  { id: "all", label: "全部" },
  { id: "tools", label: "工具" },
  { id: "system", label: "系统" },
];

const ICON_PATHS = {
  activity: <><path d="M3 12h4l2.5-7 5 14 2.5-7h4" /></>,
  shield: <><path d="M12 3 4.5 6v5.5c0 4.6 3.2 7.8 7.5 9.5 4.3-1.7 7.5-4.9 7.5-9.5V6L12 3Z" /><path d="m9 12 2 2 4-4" /></>,
  settings: <><circle cx="12" cy="12" r="3" /><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.8 2.8-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-4V21a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9A1.7 1.7 0 0 0 3 14H2.8v-4H3a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L4.2 7 7 4.2l.1.1a1.7 1.7 0 0 0 1.9.3A1.7 1.7 0 0 0 10 3v-.2h4V3a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 7l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v4H21a1.7 1.7 0 0 0-1.6 1Z" /></>,
  search: <><circle cx="11" cy="11" r="7" /><path d="m20 20-4-4" /></>,
  pause: <><path d="M8 5v14M16 5v14" /></>,
  play: <><path d="m8 5 11 7-11 7V5Z" /></>,
  stop: <><rect x="6" y="6" width="12" height="12" rx="2" /></>,
  send: <><path d="m22 2-7 20-4-9-9-4 20-7Z" /><path d="M22 2 11 13" /></>,
  chevron: <><path d="m9 18 6-6-6-6" /></>,
  down: <><path d="m6 9 6 6 6-6" /></>,
  arrowDown: <><path d="M12 4v16m-6-6 6 6 6-6" /></>,
  close: <><path d="m6 6 12 12M18 6 6 18" /></>,
  target: <><circle cx="12" cy="12" r="8" /><circle cx="12" cy="12" r="3" /><path d="M12 2v3m0 14v3M2 12h3m14 0h3" /></>,
  layers: <><path d="m12 2 9 5-9 5-9-5 9-5Z" /><path d="m3 12 9 5 9-5M3 17l9 5 9-5" /></>,
  bolt: <><path d="m13 2-9 12h8l-1 8 9-12h-8l1-8Z" /></>,
  terminal: <><rect x="3" y="4" width="18" height="16" rx="2" /><path d="m7 9 3 3-3 3m5 0h5" /></>,
  clock: <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>,
  alert: <><path d="M10.3 3.6 2.4 18a2 2 0 0 0 1.8 3h15.6a2 2 0 0 0 1.8-3L13.7 3.6a2 2 0 0 0-3.4 0Z" /><path d="M12 9v4m0 4h.01" /></>,
  check: <><path d="m5 12 4 4L19 6" /></>,
  eye: <><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12Z" /><circle cx="12" cy="12" r="3" /></>,
  external: <><path d="M15 3h6v6M10 14 21 3M18 13v7a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h7" /></>,
};

function Icon({ name, size = 16, className = "" }) {
  return <svg className={className} width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{ICON_PATHS[name]}</svg>;
}

function flattenAgents(swarms) {
  const out = [];
  for (const [name, st] of Object.entries(swarms || {})) {
    for (const [m, a] of Object.entries(st.agents || {})) {
      out.push({ challenge: name, model: m, status: a.status, steps: a.steps || 0, toolCalls: a.tool_calls || 0, tools: a.tools || {}, paused: !!st.paused });
    }
  }
  return out;
}

function fmtTime(ts) {
  if (!ts) return "";
  const date = new Date(ts * 1000);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleTimeString("zh-CN", { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function fmtDateTime(value) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString("zh-CN", { hour12: false });
}

function formatBytes(value) {
  if (!value) return "—";
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / 1024 / 1024).toFixed(1)} MB`;
}

function Metric({ label, value, kind, icon, onClick }) {
  const content = <>
    <span className={s.metricIcon}><Icon name={icon} size={17} /></span>
    <span className={s.metricCopy}><strong>{value ?? 0}</strong><span>{label}</span></span>
    {onClick && <Icon name="chevron" size={14} className={s.metricArrow} />}
  </>;
  const className = `${s.metric} ${kind ? s[kind] : ""} ${onClick ? s.metricAction : ""}`;
  return onClick ? <button type="button" className={className} onClick={onClick}>{content}</button> : <div className={className}>{content}</div>;
}

function EmptyState({ icon = "activity", title, detail, compact = false }) {
  return <div className={`${s.emptyState} ${compact ? s.emptyCompact : ""}`}>
    <span className={s.emptyIcon}><Icon name={icon} size={compact ? 16 : 22} /></span>
    <div><strong>{title}</strong>{detail && <p>{detail}</p>}</div>
  </div>;
}

function Modal({ title, eyebrow, onClose, className = "", children }) {
  const titleId = useId();
  const modalRef = useRef(null);
  useEffect(() => {
    const previousOverflow = document.body.style.overflow;
    const previousFocus = document.activeElement;
    document.body.style.overflow = "hidden";
    modalRef.current?.querySelector("button, input, select, textarea")?.focus();
    const onKey = (event) => {
      if (event.key === "Escape") { onClose(); return; }
      if (event.key !== "Tab") return;
      const focusable = Array.from(modalRef.current?.querySelectorAll("button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [href], [tabindex]:not([tabindex='-1'])") || []).filter((element) => element.getClientRects().length > 0);
      if (focusable.length === 0) { event.preventDefault(); return; }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && (document.activeElement === first || !modalRef.current?.contains(document.activeElement))) {
        event.preventDefault(); last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault(); first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.body.style.overflow = previousOverflow;
      document.removeEventListener("keydown", onKey);
      previousFocus?.focus?.();
    };
  }, [onClose]);
  return <div className={s.overlay} onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section ref={modalRef} className={`${s.modal} ${className}`} role="dialog" aria-modal="true" aria-labelledby={titleId}>
      <header className={s.modalHead}>
        <div>{eyebrow && <span className={s.eyebrow}>{eyebrow}</span>}<h2 id={titleId}>{title}</h2></div>
        <button type="button" className={s.iconButton} onClick={onClose} aria-label="关闭弹窗"><Icon name="close" size={19} /></button>
      </header>
      {children}
    </section>
  </div>;
}

function EventBody({ event }) {
  const [expanded, setExpanded] = useState(false);
  const text = event.text || (event.kind === "turn_start" ? "模型正在处理当前会话上下文。" : event.kind === "turn_end" ? `完成：${event.outcome || "本轮结束"}` : event.kind === "result" ? `执行状态：${event.outcome || "已结束"}` : "");
  const isLong = text.length > 480 || text.split("\n").length > 9;
  return <>
    <div className={`${s.eventBody} ${event.kind === "error" ? s.errBubble : ""} ${isLong && !expanded ? s.eventCollapsed : ""}`}>
      {text || <span className={s.muted}>无文本内容</span>}
    </div>
    {isLong && <button type="button" className={s.expandEvent} onClick={() => setExpanded((value) => !value)}>
      {expanded ? "收起内容" : `展开完整内容 · ${text.length.toLocaleString()} 字符`}<Icon name="down" size={13} className={expanded ? s.rotate : ""} />
    </button>}
  </>;
}

function appendEvents(previous, next, limit) {
  const seen = new Set(previous.map((event) => event.seq).filter(Boolean));
  const fresh = next.filter((event) => !event.seq || !seen.has(event.seq));
  return [...previous, ...fresh].slice(-limit);
}

function ChildStream({ challenge, agentId, depth = 1 }) {
  const [expanded, setExpanded] = useState(false);
  const [events, setEvents] = useState([]);
  const [failed, setFailed] = useState(false);
  const afterRef = useRef(0);

  useEffect(() => {
    if (!expanded || !challenge || !agentId) return undefined;
    let alive = true;
    let timer;
    const controller = new AbortController();
    const tick = async () => {
      try {
        const url = `/api/activity?challenge=${encodeURIComponent(challenge)}&model=${encodeURIComponent(agentId)}&after=${afterRef.current}`;
        const response = await fetch(url, { signal: controller.signal });
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        const data = await response.json();
        const next = data.events || [];
        if (alive && next.length) {
          afterRef.current = Math.max(afterRef.current, ...next.map((event) => event.seq || 0));
          setEvents((previous) => appendEvents(previous, next, 500));
        }
        if (alive) setFailed(false);
      } catch (error) {
        if (alive && error.name !== "AbortError") setFailed(true);
      }
      if (alive) timer = setTimeout(tick, 1000);
    };
    tick();
    return () => { alive = false; clearTimeout(timer); controller.abort(); };
  }, [expanded, challenge, agentId]);

  return <div className={s.childStream}>
    <button type="button" className={s.childToggle} onClick={() => setExpanded((value) => !value)} aria-expanded={expanded}>
      <Icon name="chevron" size={13} className={expanded ? s.childOpen : ""} />
      <span>子任务 <code>{agentId}</code></span><small>{expanded ? `${events.length} 条活动` : "查看活动"}</small>
    </button>
    {expanded && <div className={s.childEvents}>
      {failed && <div className={s.inlineError}>子任务活动暂时无法加载，正在重试</div>}
      {events.length === 0 && !failed && <div className={s.inlineEmpty}>等待子任务活动…</div>}
      {events.map((event, index) => <div key={event.seq || index} className={s.childEvent}>
        <div className={s.eventMeta}>
          <span className={`${s.eventKind} ${s[`b_${event.kind}`] || ""}`}>{KIND_LABEL[event.kind] || event.kind}</span>
          {event.tool && <code>{event.tool}</code>}<span>STEP {event.step || 0}</span>
        </div>
        <EventBody event={event} nested />
        {event.kind === "delegate_spawn" && event.agent_id && depth < 2 && <ChildStream challenge={challenge} agentId={event.agent_id} depth={depth + 1} />}
      </div>)}
    </div>}
  </div>;
}

export default function Console() {
  const [state, setState] = useState(EMPTY);
  const [watch, setWatch] = useState({ c: null, m: null });
  const [events, setEvents] = useState([]);
  const [msg, setMsg] = useState("");
  const [connected, setConnected] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [llm, setLlm] = useState(null);
  const [showSettings, setShowSettings] = useState(false);
  const [showScope, setShowScope] = useState(false);
  const [showRecords, setShowRecords] = useState(false);
	const [showHistory, setShowHistory] = useState(false);
  const [showApprovals, setShowApprovals] = useState(false);
  const [approvals, setApprovals] = useState({ pending: [], audit: [], rules: [] });
  const [approvalsSupported, setApprovalsSupported] = useState(true);
  const [approvalsError, setApprovalsError] = useState("");
  const [findingView, setFindingView] = useState(null);
  const [graphView, setGraphView] = useState(null);
  const [uiError, setUiError] = useState("");
  const [notice, setNotice] = useState(null);
  const [busyAction, setBusyAction] = useState("");
  const [eventFilter, setEventFilter] = useState("dialogue");
  const [findingQuery, setFindingQuery] = useState("");
  const [activityError, setActivityError] = useState(false);
  const [showNewEvents, setShowNewEvents] = useState(false);
  const [composerHeight, setComposerHeight] = useState(116);
  const composerResizeRef = useRef(null);
  const afterRef = useRef(0);
  const runIdRef = useRef("");
  const logRef = useRef(null);
  const followRef = useRef(true);
  const watchRef = useRef(watch);
  watchRef.current = watch;

  useEffect(() => {
    const controller = new AbortController();
    fetch("/api/state", { signal: controller.signal })
      .then((response) => { if (!response.ok) throw new Error("state unavailable"); return response.json(); })
      .then((data) => { setState(data); setConnected(true); setLoaded(true); })
      .catch((error) => { if (error.name !== "AbortError") { setConnected(false); setLoaded(true); } });
    const es = new EventSource("/api/stream");
    es.onopen = () => setConnected(true);
    es.onmessage = (event) => {
      try { setState(JSON.parse(event.data)); setConnected(true); setLoaded(true); } catch { /* Ignore malformed snapshots and keep the last good state. */ }
    };
    es.onerror = () => setConnected(false);
    return () => { controller.abort(); es.close(); };
  }, []);

  const loadLlm = useCallback(() => {
    fetch("/api/llm").then((response) => response.json()).then(setLlm).catch(() => {});
  }, []);
  useEffect(() => { loadLlm(); }, [loadLlm]);

  const loadApprovals = useCallback(async () => {
    try {
      const response = await fetch("/api/approvals");
      if (response.status === 404) { setApprovalsSupported(false); return; }
      if (!response.ok) throw new Error(`审批数据加载失败 (${response.status})`);
      const data = await response.json();
      setApprovals({ pending: data.pending || [], audit: data.audit || [], rules: data.rules || [] });
      setApprovalsSupported(true); setApprovalsError("");
    } catch (error) { setApprovalsError(error.message || String(error)); }
  }, []);
  useEffect(() => {
    loadApprovals();
    const timer = setInterval(loadApprovals, 2000);
    return () => clearInterval(timer);
  }, [loadApprovals]);

  useEffect(() => {
    if (!notice) return undefined;
    const timer = setTimeout(() => setNotice(null), 4000);
    return () => clearTimeout(timer);
  }, [notice]);

  const agents = useMemo(() => flattenAgents(state.swarms), [state.swarms]);
  const runId = state.swarms?.[watch.c]?.run_id || "";

  useEffect(() => {
    if (!runId || runIdRef.current === runId) return;
    runIdRef.current = runId;
    afterRef.current = 0;
    setEvents([]);
    setShowNewEvents(false);
    setActivityError(false);
  }, [watch.c, runId]);
  const needsConfig = agents.some((agent) => agent.status === "needs-config") || (llm && llm.supported && !llm.active);

  useEffect(() => {
    if (watch.c && agents.some((agent) => agent.challenge === watch.c && agent.model === watch.m)) return;
    const live = agents.find((agent) => agent.status === "running") || agents[0];
    if (live) { setWatch({ c: live.challenge, m: live.model }); setEvents([]); afterRef.current = 0; }
  }, [agents, watch.c, watch.m]);

  useEffect(() => {
    let alive = true;
    let timer;
    const controller = new AbortController();
    const tick = async () => {
      const current = watchRef.current;
      if (alive && current.c && current.m) {
        try {
          const response = await fetch(`/api/activity?challenge=${encodeURIComponent(current.c)}&model=${encodeURIComponent(current.m)}&after=${afterRef.current}`, { signal: controller.signal });
          if (!response.ok) throw new Error(`HTTP ${response.status}`);
          const data = await response.json();
          const next = data.events || [];
          if (alive && next.length) {
            afterRef.current = Math.max(afterRef.current, ...next.map((event) => event.seq || 0));
            setEvents((previous) => appendEvents(previous, next, 1000));
            if (!followRef.current) setShowNewEvents(true);
          }
          if (alive) setActivityError(false);
        } catch (error) {
          if (alive && error.name !== "AbortError") setActivityError(true);
        }
      }
      if (alive) timer = setTimeout(tick, 1000);
    };
    timer = setTimeout(tick, 250);
    return () => { alive = false; clearTimeout(timer); controller.abort(); };
  }, [watch.c, watch.m, runId]);

  useEffect(() => {
    if (followRef.current && logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [events, eventFilter]);

  const cmd = useCallback(async (action, extra) => {
    const current = watchRef.current;
    if (!current.c) return false;
    setBusyAction(action);
    try {
      const response = await fetch("/api/command", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action, challenge: current.c, model: current.m, ...(extra || {}) }),
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok || result.ok === false) throw new Error(result.error || `请求失败 (${response.status})`);
      setUiError("");
      const labels = { pause: "暂停已请求，当前操作完成后生效", resume: "恢复指令已发送", kill: "停止指令已发送", chat: "消息已发送" };
      setNotice({ kind: "success", text: labels[action] || "指令已发送" });
      return true;
    } catch (error) {
      setUiError(error.message || String(error));
      return false;
    } finally { setBusyAction(""); }
  }, []);

  const pickWatch = (challenge, model) => {
    setWatch({ c: challenge, m: model });
    setEvents([]); setEventFilter("dialogue"); setShowNewEvents(false); setActivityError(false);
    afterRef.current = 0; followRef.current = true;
  };
  const send = async () => {
    const text = msg.trim();
    if (!text || !watch.c || busyAction) return;
    if (await cmd("chat", { message: text })) setMsg("");
  };
  const requestKill = () => {
    if (window.confirm(`确定停止 ${watch.c} / ${watch.m} 吗？该执行器的自主任务将被终止。`)) cmd("kill");
  };
  const scrollToLatest = () => {
    followRef.current = true; setShowNewEvents(false);
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight, behavior: "smooth" });
  };

  const sm = state.summary || {};
  const findings = state.findings || [];
  const subgoals = state.subgoals || [];
  const traffic = state.traffic || [];
  const claims = state.claims || [];
  const orchestration = state.orchestration || [];
  const modeLabel = state.mode === "engagement" ? "ENGAGEMENT" : state.mode === "scheduler" ? "SCHEDULER" : "STANDBY";
  const scope = state.engagement?.scope || state.swarms?.[watch.c]?.scope || "";
  const cur = agents.find((agent) => agent.challenge === watch.c && agent.model === watch.m);
  const gTotal = sm.goals_total || 0;
  const gDone = sm.goals_done || 0;
  const pct = gTotal > 0 ? Math.round((gDone / gTotal) * 100) : 0;
  const running = agents.filter((agent) => agent.status === "running").length;

  const lastCall = useMemo(() => {
    for (let index = events.length - 1; index >= 0; index -= 1) if (events[index].kind === "tool_call") return events[index];
    return null;
  }, [events]);

  const behavior = useMemo(() => {
    const toolRows = Object.entries(cur?.tools || {}).sort((a, b) => b[1] - a[1]);
    const max = toolRows.reduce((value, [, count]) => Math.max(value, count), 1);
    return { toolRows, max, total: cur?.toolCalls || 0 };
  }, [cur]);

  const filteredEvents = useMemo(() => events.filter((event) => {
    if (eventFilter === "all") return true;
    if (eventFilter === "dialogue") return ["operator", "assistant", "progress", "text", "status", "error"].includes(event.kind);
    if (eventFilter === "tools") return ["tool_call", "tool_result"].includes(event.kind);
    return !["operator", "assistant", "progress", "text", "tool_call", "tool_result"].includes(event.kind);
  }), [events, eventFilter]);

  const visibleFindings = useMemo(() => {
    const query = findingQuery.trim().toLowerCase();
    if (!query) return findings;
    return findings.filter((finding) => [finding.title, finding.target, finding.category, finding.id].some((value) => String(value || "").toLowerCase().includes(query)));
  }, [findings, findingQuery]);

  const closeSettings = useCallback(() => setShowSettings(false), []);
  const closeScope = useCallback(() => setShowScope(false), []);
  const closeRecords = useCallback(() => setShowRecords(false), []);
	const closeHistory = useCallback(() => setShowHistory(false), []);
  const closeApprovals = useCallback(() => setShowApprovals(false), []);
  const closeFindings = useCallback(() => setFindingView(null), []);
  const closeGraph = useCallback(() => setGraphView(null), []);
  const openGraph = useCallback((mission, id) => {
    setFindingView(null);
    setGraphView({ mission, id });
  }, []);

  return <div className={s.app}>
    <header className={s.head}>
      <div className={s.brand}>
        <span className={s.brandMark}><Icon name="activity" size={22} /></span>
        <div className={s.brandCopy}><h1>CHRONOS</h1><span>AGENTIC SECURITY CONSOLE</span></div>
        <span className={s.mode}>{modeLabel}</span>
      </div>
      <div className={s.headerActions}>
        <span className={`${s.connection} ${connected ? s.online : ""}`} role="status"><i />{connected ? "实时连接" : loaded ? "连接中断" : "正在连接"}</span>
        {watch.c && <button type="button" className={s.headerButton} onClick={() => setShowScope(true)}><Icon name="shield" />授权范围</button>}
        {approvalsSupported && <button type="button" className={`${s.headerButton} ${approvals.pending.length > 0 ? s.approvalButton : ""}`} onClick={() => setShowApprovals(true)}><Icon name="check" />审批{approvals.pending.length > 0 && <span className={s.pendingBadge}>{approvals.pending.length}</span>}</button>}
		<button type="button" className={s.headerButton} onClick={() => setShowHistory(true)}><Icon name="clock" />任务历史</button>
        <button type="button" className={s.headerButton} onClick={() => setShowRecords(true)}><Icon name="activity" />LLM 记录</button>
        <button type="button" className={s.modelButton} onClick={() => { loadLlm(); setShowSettings(true); }}><Icon name="settings" /><span>{llm?.model || "模型设置"}</span></button>
      </div>
    </header>

    <section className={s.missionBar}>
      <div className={s.missionIdentity}>
        <span className={s.eyebrow}>ACTIVE WORKSPACE</span>
        <div><h2>{state.engagement?.name || (state.mode === "scheduler" ? "任务调度中心" : "安全任务控制台")}</h2><span className={s.liveLabel}>{running} / {agents.length} 执行器运行中</span></div>
      </div>
      <div className={s.missionMeta}>
        {scope && <div title={scope}><span>授权范围</span><strong>{scope}</strong></div>}
        <div><span>Token 消耗</span><strong>{(sm.tokens || 0).toLocaleString()}</strong></div>
      </div>
    </section>

    <div className={s.notices}>
      {needsConfig && !showSettings && <button type="button" className={`${s.banner} ${s.bannerWarning}`} onClick={() => { loadLlm(); setShowSettings(true); }}><Icon name="alert" /><span><strong>模型需要配置</strong>密钥未配置或已失效，点击打开模型设置。</span><Icon name="chevron" /></button>}
      {uiError && <div className={`${s.banner} ${s.bannerError}`} role="alert"><Icon name="alert" /><span>{uiError}</span><button type="button" className={s.iconButton} onClick={() => setUiError("")} aria-label="关闭错误"><Icon name="close" /></button></div>}
    </div>

    {notice && <div className={`${s.toast} ${notice.kind === "success" ? s.toastSuccess : ""}`} role="status"><Icon name="check" />{notice.text}</div>}
    {showSettings && <SettingsModal initial={llm} onClose={closeSettings} onSaved={loadLlm} />}
    {showScope && <ScopeModal challenge={watch.c} initial={state.engagement?.targets || state.swarms?.[watch.c]?.targets || []} onClose={closeScope} onSaved={() => fetch("/api/state").then((response) => response.json()).then(setState).catch(() => {})} />}
    {showRecords && <RecordsModal onClose={closeRecords} />}
	{showHistory && <HistoryModal onClose={closeHistory} />}
    {showApprovals && <ApprovalsModal data={approvals} error={approvalsError} onRefresh={loadApprovals} onClose={closeApprovals} />}
    {findingView && <FindingsModal findings={findings} initial={findingView} onClose={closeFindings} onOpenGraph={openGraph} />}
    {graphView && <Modal title="证据图谱" eyebrow="DEFEASIBLE EVIDENCE GRAPH" onClose={closeGraph} className={s.graphModal}><EvidenceGraph graphs={state.evidence_graphs || {}} initial={graphView} /></Modal>}

    <section className={s.metrics} aria-label="任务汇总">
      <Metric label="已确认发现" value={sm.confirmed} kind="confirmed" icon="shield" onClick={() => setFindingView({ filter: "confirmed" })} />
      <Metric label="待验证发现" value={sm.suspected} kind="suspected" icon="target" onClick={() => setFindingView({ filter: "suspected" })} />
      <Metric label="已排除发现" value={sm.failed} kind="failed" icon="close" onClick={() => setFindingView({ filter: "failed" })} />
      <Metric label="全部发现" value={sm.findings} icon="layers" onClick={() => setFindingView({ filter: "all" })} />
      <Metric label="事实与图谱" value={sm.facts} icon="layers" onClick={() => setGraphView({})} />
      <Metric label="执行器" value={agents.length} icon="bolt" />
    </section>

    <div className={s.workspace}>
      <aside className={s.leftColumn}>
        <section className={s.panel}>
          <div className={s.panelHead}><div><span className={s.panelIndex}>01</span><h2>任务进展</h2></div><span className={s.panelMeta}>{gDone}/{gTotal}</span></div>
          <div className={s.progressSummary}><strong>{pct}<small>%</small></strong><span>子目标已完成<br /><b>{gTotal - gDone} 项待处理</b></span></div>
          <div className={s.progressTrack} role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label="子目标完成进度"><i style={{ width: `${pct}%` }} /></div>
          <div className={s.currentAction}>
            <span className={s.sectionLabel}>当前执行器</span>
            {cur ? <><div className={s.currentAgent}><span className={`${s.statusDot} ${cur.status === "running" ? s.pulse : ""}`} /><strong>{STATUS_LABEL[cur.status] || cur.status}</strong><span>{cur.steps} steps</span></div><code title={cur.model}>{cur.model}</code></> : <span className={s.muted}>尚未选择执行器</span>}
            {lastCall && <div className={s.currentTool}><Icon name="terminal" size={14} /><div><strong>{lastCall.tool}</strong><span>{lastCall.text || "等待工具返回"}</span></div></div>}
          </div>
          <div className={s.subgoalList}>
            {subgoals.length === 0 && <EmptyState compact icon="target" title="暂无子目标" detail="Agent 创建目标后会显示在这里" />}
            {subgoals.map((goal) => <div key={goal.id} className={`${s.subgoal} ${goal.done ? s.done : ""}`}>
              <span className={s.goalCheck}>{goal.done && <Icon name="check" size={12} />}</span>
              <div><span>{goal.description}</span>{goal.assigned_to && <small>{goal.assigned_to}</small>}</div>
            </div>)}
          </div>
        </section>

        <section className={s.panel}>
          <div className={s.panelHead}><div><span className={s.panelIndex}>02</span><h2>工具行为</h2></div><span className={s.panelMeta}>{behavior.total} 次</span></div>
          {behavior.toolRows.length === 0 ? <EmptyState compact icon="terminal" title="暂无工具活动" /> : <div className={s.behaviorList}>
            {behavior.toolRows.map(([tool, count]) => <div key={tool} className={s.behaviorRow}><div><code>{tool}</code><span>{count}</span></div><div className={s.behaviorTrack}><i style={{ width: `${(count / behavior.max) * 100}%` }} /></div></div>)}
          </div>}
        </section>

        {(claims.length > 0 || orchestration.length > 0) && <details className={`${s.panel} ${s.orchestration}`}>
          <summary><span><span className={s.panelIndex}>03</span>编排动态</span><span className={s.panelMeta}>{claims.length + orchestration.length}<Icon name="down" size={14} /></span></summary>
          <div className={s.orchestrationList}>
            {claims.map((claim) => <div key={`${claim.mission || ""}/${claim.surface}/${claim.owner}`} className={s.orchestrationRow}><div><span className={s.orchestrationKind}>认领</span><code>{claim.surface}</code></div><p>{claim.mission && `${claim.mission} · `}{claim.owner}</p><small>到期 {fmtTime(claim.expires_at)}</small></div>)}
            {[...orchestration].reverse().slice(0, 30).map((event) => <div key={event.seq} className={s.orchestrationRow}><div><span className={s.orchestrationKind}>{event.kind}</span><strong>{event.mission || "全局"}</strong></div><p>{event.reason}</p><small>{fmtTime(event.time)} {event.detail}</small></div>)}
          </div>
        </details>}
      </aside>

      <main className={s.timelinePanel}>
        <div className={s.timelineHead}>
          <div className={s.executorTitle}><span className={s.eyebrow}>AGENT SESSION</span><div><h2>Agent 会话</h2>{cur && <span className={`${s.statusBadge} ${s[`status_${cur.status}`] || ""}`}><i />{STATUS_LABEL[cur.status] || cur.status}</span>}</div></div>
          <div className={s.executorControls}>
            <select aria-label="选择执行器" value={cur ? `${watch.c}||${watch.m}` : ""} onChange={(event) => { const [challenge, model] = event.target.value.split("||"); pickWatch(challenge, model); }}>
              {agents.length === 0 && <option value="">暂无执行器</option>}
              {agents.map((agent) => <option key={agent.challenge + agent.model} value={`${agent.challenge}||${agent.model}`}>{agent.challenge} / {agent.model}</option>)}
            </select>
            <div className={s.controlButtons}>
              <button type="button" onClick={() => cmd("pause")} disabled={!watch.c || !!busyAction} title="暂停执行器"><Icon name="pause" /><span>暂停</span></button>
              <button type="button" onClick={() => cmd("resume")} disabled={!watch.c || !!busyAction} title="恢复执行器"><Icon name="play" /><span>继续</span></button>
              <button type="button" className={s.stopButton} onClick={requestKill} disabled={!watch.c || !!busyAction} title="停止执行器"><Icon name="stop" /><span>停止</span></button>
            </div>
          </div>
        </div>
        <div className={s.timelineToolbar}>
          <div className={s.filterTabs} role="tablist" aria-label="活动类型">{EVENT_FILTERS.map((filter) => <button type="button" role="tab" aria-selected={eventFilter === filter.id} key={filter.id} className={eventFilter === filter.id ? s.active : ""} onClick={() => setEventFilter(filter.id)}>{filter.label}</button>)}</div>
          <span className={s.eventCount}>{filteredEvents.length} 条记录{activityError && <b> · 正在重连</b>}</span>
        </div>
        <div className={s.logWrap}>
          <div className={s.log} ref={logRef} onScroll={() => {
            const element = logRef.current;
            if (!element) return;
            const atBottom = element.scrollHeight - element.scrollTop - element.clientHeight < 64;
            followRef.current = atBottom;
            if (atBottom) setShowNewEvents(false);
          }}>
            {!watch.c && <EmptyState icon="activity" title="等待执行器上线" detail="任务启动后，这里会显示 Agent 的回复与工作进度；工具和流量可在对应视图查看。" />}
            {watch.c && filteredEvents.length === 0 && <EmptyState icon="terminal" title={events.length ? "当前筛选下暂无记录" : "正在等待执行活动"} detail={events.length ? "切换事件类型查看其他记录。" : "Agent 开始执行后，事件将自动出现在这里。"} />}
            {filteredEvents.map((event, index) => <article key={event.seq || index} className={`${s.event} ${event.kind === "operator" ? s.operatorEvent : ""} ${event.kind === "assistant" || event.kind === "text" ? s.assistantEvent : ""} ${event.kind === "progress" ? s.progressEvent : ""}`}>
              <div className={s.eventRail}><span className={`${s.eventNode} ${s[`b_${event.kind}`] || ""}`} />{index < filteredEvents.length - 1 && <i />}</div>
              <div className={s.eventContent}>
                <div className={s.eventMeta}><span className={`${s.eventKind} ${s[`b_${event.kind}`] || ""}`} title={event.message_type || ""}>{KIND_LABEL[event.kind] || event.kind}</span>{event.subtype && <span>{event.subtype}</span>}{event.tool && <code>{event.tool}</code>}{event.turn > 0 && <span>TURN {event.turn}</span>}{event.step > 0 && <span>STEP {event.step}</span>}{event.tool_call_id && <code title={event.tool_call_id}>#{event.tool_call_id.slice(-8)}</code>}{event.tokens > 0 && <span>{event.tokens} tokens</span>}{event.final && <span>FINAL</span>}{event.ts && <time>{fmtTime(event.ts)}</time>}</div>
                <EventBody event={event} />
                {event.kind === "delegate_spawn" && event.agent_id && <ChildStream challenge={watch.c} agentId={event.agent_id} />}
              </div>
            </article>)}
          </div>
          {showNewEvents && <button type="button" className={s.latestButton} onClick={scrollToLatest}><Icon name="arrowDown" size={14} />回到最新活动</button>}
        </div>
        <div className={s.composer} style={{ height: composerHeight }}>
          <div className={s.composerResizeHandle} role="separator" aria-label="上下拖动调整指令输入区高度" aria-orientation="horizontal" aria-valuemin={96} aria-valuemax={800} aria-valuenow={composerHeight} tabIndex={0}
            onPointerDown={(event) => {
              event.preventDefault();
              event.currentTarget.setPointerCapture(event.pointerId);
              composerResizeRef.current = { y: event.clientY, height: composerHeight };
            }}
            onPointerMove={(event) => {
              if (!composerResizeRef.current) return;
              const maxHeight = Math.max(180, window.innerHeight * 0.65);
              const next = composerResizeRef.current.height + composerResizeRef.current.y - event.clientY;
              setComposerHeight(Math.round(Math.min(maxHeight, Math.max(96, next))));
            }}
            onPointerUp={(event) => {
              composerResizeRef.current = null;
              if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
            }}
            onPointerCancel={() => { composerResizeRef.current = null; }}
            onDoubleClick={() => setComposerHeight(116)}
            onKeyDown={(event) => {
              if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
              event.preventDefault();
              const delta = (event.key === "ArrowUp" ? 1 : -1) * (event.shiftKey ? 64 : 24);
              const maxHeight = Math.max(180, window.innerHeight * 0.65);
              setComposerHeight((height) => Math.min(maxHeight, Math.max(96, height + delta)));
            }}><span /></div>
          <div className={s.composerBox}><textarea rows={2} aria-label="发送给 Agent 的消息" placeholder={watch.c ? (cur?.paused ? "执行已暂停；消息会在继续后由同一会话处理…" : "向 Agent 提问或下达指令…") : "暂无可对话的执行器"} value={msg} disabled={!watch.c} onChange={(event) => setMsg(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); send(); } }} />
            <div className={s.composerFoot}><span>Enter 发送 · Shift + Enter 换行</span><button type="button" className={s.sendButton} onClick={send} disabled={!watch.c || !msg.trim() || busyAction === "chat"}>{busyAction === "chat" ? <span className={s.spinner} /> : <Icon name="send" size={15} />}{busyAction === "chat" ? "发送中" : "发送"}</button></div>
          </div>
        </div>
      </main>

      <aside className={s.rightColumn}>
        <section className={`${s.panel} ${s.findingsPanel}`}>
          <div className={s.panelHead}><div><span className={s.panelIndex}>03</span><h2>发现 Findings</h2></div><button type="button" className={s.textButton} onClick={() => setFindingView({ filter: "all" })}>查看全部<Icon name="chevron" size={13} /></button></div>
          <label className={s.searchBox}><Icon name="search" size={15} /><input value={findingQuery} onChange={(event) => setFindingQuery(event.target.value)} placeholder="搜索标题、目标或分类" aria-label="搜索发现" />{findingQuery && <button type="button" onClick={() => setFindingQuery("")} aria-label="清空搜索"><Icon name="close" size={13} /></button>}</label>
          <div className={s.findingList}>
            {visibleFindings.length === 0 && <EmptyState compact icon="shield" title={findings.length ? "没有匹配的发现" : "尚未发现安全问题"} detail={findings.length ? "尝试其他关键词。" : "经 Agent 记录的发现会显示在这里。"} />}
            {visibleFindings.slice(0, 30).map((finding) => <button type="button" key={finding.id} className={s.findingCard} onClick={() => setFindingView({ filter: "all", id: finding.id })}>
              <div className={s.findingTop}><span className={`${s.severity} ${s[`sev_${finding.severity}`] || ""}`}>{finding.severity || "info"}</span><span className={`${s.verdict} ${s[`v_${finding.verdict}`] || ""}`}><i />{VERDICT_LABEL[finding.verdict] || finding.verdict}</span><span className={s.findingId}>{finding.id}</span></div>
              <strong>{finding.title}</strong><div className={s.findingMeta}><span>{finding.target || "未指定目标"}</span><small>{finding.evidence || 0} 项证据<Icon name="chevron" size={12} /></small></div>
            </button>)}
          </div>
          {visibleFindings.length > 30 && <button type="button" className={s.moreButton} onClick={() => setFindingView({ filter: "all" })}>另有 {visibleFindings.length - 30} 条，打开完整列表</button>}
        </section>

        <section className={`${s.panel} ${s.trafficPanel}`}>
          <div className={s.panelHead}><div><span className={s.panelIndex}>04</span><h2>实时流量</h2></div><span className={s.trafficLive}><i />CAPTURE</span></div>
          <div className={s.trafficHeader}><span>请求</span><span>目标</span><span>状态</span></div>
          <div className={s.trafficList}>
            {traffic.length === 0 && <EmptyState compact icon="activity" title="暂无捕获流量" />}
            {[...traffic].reverse().slice(0, 80).map((item) => <div key={`${item.mission || "current"}/${item.id}`} className={s.trafficRow} title={item.target}>
              <div><span className={s.trafficId}>{item.id}</span><code className={s[`tk_${item.kind}`] || ""}>{item.method || item.kind}</code></div>
              <div className={s.trafficTarget}><span>{item.target}</span><small>{item.mission && state.mode === "scheduler" ? `${item.mission} · ` : ""}{formatBytes(item.resp_len)}</small></div>
              <span className={`${s.trafficStatus} ${item.status >= 400 ? s.bad : item.status >= 300 ? s.warn : s.ok}`}>{item.status || "—"}</span>
            </div>)}
          </div>
        </section>
      </aside>
    </div>
    <footer className={s.footer}><span>CHRONOS / 实时任务观测与操作</span><span>证据驱动 · Scope 强制约束 · 本地控制台</span></footer>
  </div>;
}

function FindingsModal({ findings, initial, onClose, onOpenGraph }) {
  const [filter, setFilter] = useState(initial?.filter || "all");
  const [query, setQuery] = useState("");
  const filtered = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return findings.filter((finding) => (filter === "all" || finding.verdict === filter) && (!normalized || [finding.title, finding.target, finding.category, finding.id].some((value) => String(value || "").toLowerCase().includes(normalized))));
  }, [findings, filter, query]);
  const [selectedId, setSelectedId] = useState(initial?.id || null);
  const selected = filtered.find((finding) => finding.id === selectedId) || filtered[0] || null;

  return <Modal title="发现详情" eyebrow="FINDING EXPLORER" onClose={onClose} className={s.findingsModal}>
    <div className={s.findingsToolbar}>
      <div className={s.filterTabs}>{[["all", "全部"], ["confirmed", "已确认"], ["suspected", "待验证"], ["failed", "已排除"]].map(([id, label]) => <button type="button" key={id} className={filter === id ? s.active : ""} onClick={() => setFilter(id)}>{label}<span>{id === "all" ? findings.length : findings.filter((finding) => finding.verdict === id).length}</span></button>)}</div>
      <label className={s.searchBox}><Icon name="search" size={15} /><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索发现" aria-label="在发现详情中搜索" /></label>
    </div>
    <div className={s.findingsDetailGrid}>
      <div className={s.findingsNav}>
        {filtered.length === 0 && <EmptyState compact icon="search" title="当前分类暂无记录" />}
        {filtered.map((finding) => <button type="button" key={finding.id} className={`${s.findingNavItem} ${selected?.id === finding.id ? s.findingNavActive : ""}`} onClick={() => setSelectedId(finding.id)}>
          <div><span className={`${s.severity} ${s[`sev_${finding.severity}`] || ""}`}>{finding.severity}</span><span className={`${s.verdict} ${s[`v_${finding.verdict}`] || ""}`}>{VERDICT_LABEL[finding.verdict] || finding.verdict}</span></div><strong>{finding.title}</strong><small>{finding.id} · {finding.target || "未指定目标"}</small>
        </button>)}
      </div>
      <div className={s.findingDetail}>{selected ? <FindingDetail finding={selected} onOpenGraph={onOpenGraph} /> : <EmptyState icon="shield" title="选择一条发现" detail="查看证据、复现步骤和验证记录。" />}</div>
    </div>
  </Modal>;
}

function FindingDetail({ finding, onOpenGraph }) {
  const verifications = finding.verifications || [];
  const refs = finding.evidence_refs || [];
  const graphId = finding.mission && finding.id.startsWith(`${finding.mission}/`) ? finding.id.slice(finding.mission.length + 1) : finding.id;
  return <>
    <div className={s.detailHead}><div><span className={`${s.severity} ${s[`sev_${finding.severity}`] || ""}`}>{finding.severity}</span><span className={`${s.verdict} ${s[`v_${finding.verdict}`] || ""}`}><i />{VERDICT_LABEL[finding.verdict] || finding.verdict}</span></div><h3>{finding.title}</h3><div className={s.detailActions}><span>{finding.id}</span><button type="button" className={s.detailGraphButton} onClick={() => onOpenGraph(finding.mission, graphId)}>查看证据链<Icon name="chevron" size={13} /></button></div></div>
    <dl className={s.detailMeta}>
      <div><dt>状态</dt><dd>{finding.status || finding.verdict}</dd></div><div><dt>分类</dt><dd>{finding.category || "—"}</dd></div>
      <div><dt>目标</dt><dd>{finding.target || "—"}</dd></div><div><dt>发现者</dt><dd>{finding.found_by || "—"}</dd></div><div><dt>Mission</dt><dd>{finding.mission || "—"}</dd></div><div><dt>记录时间</dt><dd>{fmtTime(finding.verified_ts || finding.ts) || "—"}</dd></div>
    </dl>
    <DetailBlock title="图判决依据 Verdict rationale" value={finding.verdict_reason} />
    <DetailBlock title="证据 Evidence" value={finding.evidence_text} />
    <DetailBlock title="复现步骤 Reproduction" value={finding.reproduction} />
    <DetailBlock title="修复建议 Remediation" value={finding.remediation} />
    {refs.length > 0 && <section className={s.detailBlock}><h4>证据引用 · {refs.length}</h4><div className={s.refList}>{refs.map((ref, index) => <span key={`${ref.ref}-${ref.role}-${index}`}><b>{ref.ref}</b>{ref.role && <small>{ref.role}</small>}{ref.note && <em>{ref.note}</em>}</span>)}</div></section>}
    {verifications.length > 0 && <section className={s.detailBlock}><h4>验证记录 · {verifications.length}</h4><div className={s.verificationList}>{verifications.map((verification, index) => <article key={`${verification.ts || index}-${index}`}>
      <div><b>{VERIFICATION_LABEL[verification.verdict] || verification.verdict}</b><span>{verification.by || "Agent"} · {fmtTime(verification.ts) || "—"}</span></div>
      {verification.summary && <p>{verification.summary}</p>}{verification.evidence && <pre>{verification.evidence}</pre>}
      {(verification.evidence_refs || []).length > 0 && <div className={s.refList}>{verification.evidence_refs.map((ref, refIndex) => <span key={`${ref.ref}-${ref.role}-${refIndex}`}><b>{ref.ref}</b>{ref.role && <small>{ref.role}</small>}{ref.note && <em>{ref.note}</em>}</span>)}</div>}
    </article>)}</div></section>}
  </>;
}

function DetailBlock({ title, value }) {
  if (!value) return null;
  return <section className={s.detailBlock}><h4>{title}</h4><pre>{value}</pre></section>;
}

const RUN_STATUS_LABEL = { active: "运行中", completed: "已结束", interrupted: "已中断", archived: "旧任务" };
const RUN_OUTCOME_LABEL = { scope_changed: "授权范围已切换", platform_solved: "平台确认完成", deferred: "已延期", completed: "执行结束", interrupted: "执行中断", finished: "执行结束" };

function HistoryModal({ onClose }) {
  const [runs, setRuns] = useState([]);
  const [selectedId, setSelectedId] = useState("");
  const [detail, setDetail] = useState(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [detailLoading, setDetailLoading] = useState(false);
  const [tab, setTab] = useState("findings");
  const [findingId, setFindingId] = useState("");
  const [graphNodeId, setGraphNodeId] = useState("");
  const [runQuery, setRunQuery] = useState("");
  const [runStatus, setRunStatus] = useState("all");
  const [model, setModel] = useState("all");
  const [activityKind, setActivityKind] = useState("all");
  const [activityQuery, setActivityQuery] = useState("");
  const [trafficQuery, setTrafficQuery] = useState("");
  const [trafficId, setTrafficId] = useState("");
  const [trafficDetail, setTrafficDetail] = useState(null);
  const [trafficLoading, setTrafficLoading] = useState(false);
  const trafficRequestRef = useRef(0);

  useEffect(() => {
    const controller = new AbortController();
    fetch("/api/runs", { signal: controller.signal })
      .then((response) => { if (!response.ok) throw new Error(`任务历史加载失败 (${response.status})`); return response.json(); })
      .then((data) => {
        const next = data.runs || [];
        setRuns(next);
        setSelectedId((current) => current || next.find((run) => run.status !== "active")?.id || next[0]?.id || "");
        setError(""); setLoading(false);
      })
      .catch((err) => { if (err.name !== "AbortError") { setError(err.message || String(err)); setLoading(false); } });
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!selectedId) return undefined;
    const controller = new AbortController();
    trafficRequestRef.current++;
    setDetail(null); setDetailLoading(true); setTab("findings"); setFindingId(""); setGraphNodeId("");
    setModel("all"); setActivityKind("all"); setActivityQuery(""); setTrafficQuery(""); setTrafficId("");
    setTrafficDetail(null); setTrafficLoading(false);
    fetch(`/api/runs/detail?id=${encodeURIComponent(selectedId)}`, { signal: controller.signal })
      .then((response) => { if (!response.ok) throw new Error(`任务详情加载失败 (${response.status})`); return response.json(); })
      .then((data) => { setDetail(data); setDetailLoading(false); setError(""); })
      .catch((err) => { if (err.name !== "AbortError") { setError(err.message || String(err)); setDetailLoading(false); } });
    return () => controller.abort();
  }, [selectedId]);

  const filteredRuns = useMemo(() => {
    const query = runQuery.trim().toLowerCase();
    return runs.filter((item) => (runStatus === "all" || item.status === runStatus) && (!query || [item.id, item.mission, item.scope, item.mode].some((value) => String(value || "").toLowerCase().includes(query))));
  }, [runs, runQuery, runStatus]);
  useEffect(() => {
    if (filteredRuns.length > 0 && !filteredRuns.some((item) => item.id === selectedId)) setSelectedId(filteredRuns[0].id);
  }, [filteredRuns, selectedId]);

  const selectionVisible = filteredRuns.some((item) => item.id === selectedId);
  const run = runs.find((item) => item.id === selectedId);
  const findings = detail?.findings || [];
  const facts = detail?.facts || [];
  const selectedFinding = findings.find((item) => item.id === findingId) || findings[0];
  const models = [...new Set((detail?.activity || []).map((item) => item.model))];
  const activity = (detail?.activity || []).filter(({ model: agent, event }) => {
    if (model !== "all" && agent !== model) return false;
    if (activityKind === "dialogue" && !["operator", "assistant", "progress", "text", "status", "error"].includes(event.kind)) return false;
    if (activityKind === "tools" && !["tool_call", "tool_result"].includes(event.kind)) return false;
    if (activityKind === "system" && ["operator", "assistant", "progress", "text", "tool_call", "tool_result"].includes(event.kind)) return false;
    const query = activityQuery.trim().toLowerCase();
    return !query || [agent, event.kind, event.tool, event.text, event.outcome].some((value) => String(value || "").toLowerCase().includes(query));
  });
  const traffic = detail?.traffic || [];
  const filteredTraffic = traffic.filter((item) => {
    const query = trafficQuery.trim().toLowerCase();
    return !query || [item.id, item.method, item.kind, item.target, item.status].some((value) => String(value || "").toLowerCase().includes(query));
  });
  const openGraphNode = (id) => { setGraphNodeId(id); setTab("graph"); };
  const openTraffic = async (id) => {
    const request = ++trafficRequestRef.current;
    setTrafficId(id); setTrafficDetail(null); setTrafficLoading(true);
    try {
      const response = await fetch(`/api/runs/traffic?id=${encodeURIComponent(selectedId)}&traffic=${encodeURIComponent(id)}`);
      if (!response.ok) throw new Error(`流量详情加载失败 (${response.status})`);
      const data = await response.json();
      if (request === trafficRequestRef.current) { setTrafficDetail(data); setError(""); }
    } catch (err) { if (request === trafficRequestRef.current) setError(err.message || String(err)); }
    if (request === trafficRequestRef.current) setTrafficLoading(false);
  };
  const changeTrafficQuery = (value) => {
    trafficRequestRef.current++;
    setTrafficQuery(value); setTrafficId(""); setTrafficDetail(null); setTrafficLoading(false);
  };

  return <Modal title="任务历史" eyebrow="PERSISTED RUNS" onClose={onClose} className={s.historyModal}>
    <p className={s.modalHint}>按独立运行 ID 保存。切换授权范围后，旧任务可在这里查看；所有历史内容均为只读。</p>
    {error && <div className={s.inlineError} role="alert">{error}</div>}
    {loading ? <EmptyState icon="clock" title="正在加载任务历史" /> : runs.length === 0 ? <EmptyState icon="clock" title="暂无任务记录" detail="任务启动后会在这里留下运行记录。" /> :
      <div className={s.historyGrid}>
        <aside className={s.historySidebar}>
          <div className={s.historyRunFilters}>
            <label className={s.searchBox}><Icon name="search" size={14} /><input value={runQuery} onChange={(event) => setRunQuery(event.target.value)} placeholder="Mission、运行 ID 或 Scope" aria-label="搜索历史任务" />{runQuery && <button type="button" onClick={() => setRunQuery("")} aria-label="清空任务搜索"><Icon name="close" size={12} /></button>}</label>
            <label><span>运行状态</span><select value={runStatus} onChange={(event) => setRunStatus(event.target.value)}><option value="all">全部状态 · {runs.length}</option>{Object.entries(RUN_STATUS_LABEL).map(([id, label]) => <option key={id} value={id}>{label} · {runs.filter((item) => item.status === id).length}</option>)}</select></label>
            <small>{filteredRuns.length} 个运行记录</small>
          </div>
          <nav className={s.historyNav} aria-label="历史任务">
            {filteredRuns.length === 0 && <EmptyState compact icon="search" title="没有匹配的任务" detail="尝试清空搜索或切换状态。" />}
            {filteredRuns.map((item) => <button type="button" key={item.id} className={`${s.historyRun} ${selectedId === item.id ? s.historyRunActive : ""}`} onClick={() => setSelectedId(item.id)}>
              <span><strong>{item.mission}</strong><em className={s[`run_${item.status}`]}>{RUN_STATUS_LABEL[item.status] || item.status}</em></span>
              <small>{fmtDateTime(item.started)} · {item.mode || "legacy"}</small>
              <code title={item.scope || "旧记录未保存范围"}>{item.scope || "旧记录未保存范围"}</code>
              <small>确认 {item.summary?.confirmed || 0} · 待验证 {item.summary?.suspected || 0} · 事实 {item.summary?.facts || 0}</small>
              <code className={s.historyRunId} title={item.id}>{item.id}</code>
            </button>)}
          </nav>
        </aside>
        <div className={s.historyDetail}>
          {detailLoading && <EmptyState icon="activity" title="正在加载任务详情" />}
          {!detailLoading && !selectionVisible && <EmptyState icon="search" title="没有可显示的运行详情" detail="调整左侧搜索与状态筛选。" />}
          {!detailLoading && selectionVisible && run && detail && <>
            <div className={s.historyMeta}>
              <div><span>运行 ID</span><code>{run.id}</code></div><div><span>授权范围</span><strong>{run.scope || "旧记录未保存范围"}</strong></div>
              <div><span>开始</span><strong>{fmtDateTime(run.started)}</strong></div><div><span>结束 / 状态</span><strong>{run.ended ? fmtDateTime(run.ended) : RUN_STATUS_LABEL[run.status] || run.status}{run.outcome ? ` · ${RUN_OUTCOME_LABEL[run.outcome] || run.outcome}` : ""}</strong></div>
            </div>
            <div className={s.historyCounts}><span>已确认 {run.summary?.confirmed || 0}</span><span>待验证 {run.summary?.suspected || 0}</span><span>已排除 {run.summary?.failed || 0}</span><span>事实 {run.summary?.facts || 0}</span></div>
            <div className={`${s.filterTabs} ${s.historyTabs}`} role="tablist" aria-label="历史详情">
              {[["findings", `发现 ${findings.length}`], ["facts", `事实 ${facts.length}`], ["activity", `活动 ${detail.event_count || 0}`], ["traffic", `流量 ${traffic.length}`], ["graph", "证据图"]].map(([id, label]) => <button type="button" role="tab" aria-selected={tab === id} key={id} className={tab === id ? s.active : ""} onClick={() => setTab(id)}>{label}</button>)}
            </div>
            {tab === "findings" && (findings.length === 0 ? <EmptyState compact icon="shield" title="该任务没有发现" /> : <div className={s.historyFindings}>
              <div className={s.historyFindingNav}>{findings.map((finding) => <button type="button" key={finding.id} className={selectedFinding?.id === finding.id ? s.historyFindingActive : ""} onClick={() => setFindingId(finding.id)}><span className={`${s.severity} ${s[`sev_${finding.severity}`] || ""}`}>{finding.severity}</span><strong>{finding.title}</strong><small>{finding.id} · {VERDICT_LABEL[finding.verdict] || finding.verdict}</small></button>)}</div>
              <div className={s.historyFindingDetail}><FindingDetail finding={{ ...selectedFinding, mission: run.mission }} onOpenGraph={(_mission, id) => openGraphNode(id)} /></div>
            </div>)}
            {tab === "facts" && (facts.length === 0 ? <EmptyState compact icon="terminal" title="该任务没有事实记录" /> : <div className={s.historyFacts}>{facts.map((fact) => <article key={fact.id} className={s.historyFact}>
              <div className={s.historyFactHead}><span className={`${s.verdict} ${s[`v_${fact.verdict}`] || ""}`}><i />{VERDICT_LABEL[fact.verdict] || fact.verdict || "未判决"}</span><code>{fact.id}</code><button type="button" className={s.textButton} onClick={() => openGraphNode(fact.id)}>查看证据链<Icon name="chevron" size={12} /></button></div>
              <p className={s.historyFactStatement}>{fact.statement}</p>
              {fact.verdict_reason && <p className={s.historyFactReason}><strong>图判决依据</strong>{fact.verdict_reason}</p>}
              <div className={s.historyFactMeta}><span>{fact.found_by || "Agent"}</span><time>{fmtTime(fact.ts) || "—"}</time></div>
              {(fact.evidence_refs || []).length > 0 && <div className={s.refList}>{fact.evidence_refs.map((ref, index) => <span key={`${ref.ref}-${ref.role}-${index}`}><b>{ref.ref}</b>{ref.role && <small>{ref.role}</small>}{ref.note && <em>{ref.note}</em>}</span>)}</div>}
            </article>)}</div>)}
            {tab === "activity" && <div className={s.historyActivity}>
              <div className={s.historyToolbar}>
                <label><span>执行器</span><select value={model} onChange={(event) => setModel(event.target.value)}><option value="all">全部执行器</option>{models.map((name) => <option key={name} value={name}>{name}</option>)}</select></label>
                <label><span>事件类型</span><select value={activityKind} onChange={(event) => setActivityKind(event.target.value)}>{EVENT_FILTERS.map((filter) => <option key={filter.id} value={filter.id}>{filter.label}</option>)}</select></label>
                <label className={s.historyGrow}><span>全文搜索</span><input value={activityQuery} onChange={(event) => setActivityQuery(event.target.value)} placeholder="内容、工具或结果" /></label>
                <span className={s.historyResultCount}>{activity.length} / {detail.activity?.length || 0} 条</span>
              </div>
              {activity.length === 0 ? <EmptyState compact icon="activity" title={detail.activity?.length ? "当前筛选无活动" : "无活动日志"} detail={detail.activity?.length ? "调整执行器、类型或搜索条件。" : "旧版本任务仍可查看黑板、发现和流量；新任务会保存活动事件。"} /> : activity.map(({ model: agent, event }, index) => <article key={`${event.seq}-${index}`} className={s.historyEvent}>
                <div className={s.historyEventMeta}><span className={`${s.eventKind} ${s[`b_${event.kind}`] || ""}`}>{KIND_LABEL[event.kind] || event.kind}</span><code title={agent}>{event.tool || agent}</code>{event.step > 0 && <small>STEP {event.step}</small>}{event.turn > 0 && <small>TURN {event.turn}</small>}<time>{fmtTime(event.ts)}</time></div>
                <EventBody event={event} />
              </article>)}
              {detail.event_count > (detail.activity?.length || 0) && <p className={s.modalHint}>已加载最近 {detail.activity?.length || 0} 条，共保存 {detail.event_count} 条；筛选只作用于已加载事件。</p>}
            </div>}
            {tab === "traffic" && (traffic.length ? <div className={s.historyTraffic}>
              <div className={s.historyToolbar}><label className={s.historyGrow}><span>搜索流量</span><input value={trafficQuery} onChange={(event) => changeTrafficQuery(event.target.value)} placeholder="ID、方法、目标或状态" /></label><span className={s.historyResultCount}>{filteredTraffic.length} / {traffic.length} 条</span></div>
              <div className={s.historyTrafficLayout}>
                <div className={s.historyTrafficList}>{filteredTraffic.length === 0 ? <EmptyState compact icon="search" title="没有匹配的流量" /> : filteredTraffic.map((item) => <button type="button" key={item.id} className={trafficId === item.id ? s.historyTrafficActive : ""} onClick={() => openTraffic(item.id)}><code>{item.id}</code><strong>{item.method || item.kind} {item.target}</strong><span>{item.status || "—"} · {fmtTime(item.ts)}</span></button>)}</div>
                <div className={s.historyTrafficDetail}>{trafficLoading ? <EmptyState compact icon="activity" title="正在加载流量详情" /> : trafficDetail ? <><div className={s.historyTrafficTitle}><strong>{trafficDetail.id}</strong><span>{trafficDetail.method} {trafficDetail.status || ""}</span></div><code className={s.historyTrafficTarget}>{trafficDetail.target}</code><h4>请求 Request</h4><pre>{trafficDetail.request || "—"}</pre><h4>响应 Response</h4><pre>{trafficDetail.response || "—"}</pre></> : <EmptyState compact icon="activity" title="选择一条流量" detail="查看完整请求与响应。" />}</div>
              </div>
            </div> : <EmptyState compact icon="activity" title="无流量记录" />)}
            {tab === "graph" && <EvidenceGraph key={`${run.id}/${graphNodeId}`} graphs={{ [run.mission]: detail.graph }} initial={{ mission: run.mission, id: graphNodeId || undefined }} />}
          </>}
        </div>
      </div>}
  </Modal>;
}

function RecordsModal({ onClose }) {
  const [rows, setRows] = useState([]);
  const [trace, setTrace] = useState(null);
  const [error, setError] = useState("");
  const [busyTrace, setBusyTrace] = useState("");
  const load = useCallback(async () => {
    try {
      const response = await fetch("/api/llm/records");
      if (!response.ok) throw new Error(response.status === 404 ? "当前后端不支持 LLM 记录，请重启到最新版本" : `调用记录加载失败 (${response.status})`);
      const data = await response.json();
      setRows([...(data.records || [])].reverse()); setError("");
    } catch (err) { setError(err.message || String(err)); }
  }, []);
  useEffect(() => {
    let alive = true;
    const refresh = () => { if (alive) load(); };
    refresh();
    const timer = setInterval(refresh, 3000);
    return () => { alive = false; clearInterval(timer); };
  }, [load]);
  const openTrace = async (row) => {
    setTrace(null); setBusyTrace(row.id);
    try {
      const response = await fetch(`/api/llm/records/trace?mission=${encodeURIComponent(row.mission)}&id=${encodeURIComponent(row.id)}`);
      if (!response.ok) throw new Error(response.status === 404 ? "完整记录未开启或当前调用没有保存 trace；可通过 LLM_RECORD=1 开启后续调用录制。" : `Trace 加载失败 (${response.status})`);
      setTrace(await response.json()); setError("");
    } catch (err) { setError(err.message || String(err)); }
    setBusyTrace("");
  };
  return <Modal title="LLM 调用记录" eyebrow="MODEL USAGE" onClose={onClose} className={s.recordsModal}>
    <p className={s.fieldHint}>每次调用都计量 token、延迟和状态。完整请求与响应需启用 LLM_RECORD=1。点击一条记录查看 trace。</p>
    {error && <p className={s.inlineError}>{error}</p>}
    <div className={s.auditList}>{rows.length === 0 ? <p className={s.inlineEmpty}>暂无调用记录</p> : rows.map((row) => <button type="button" key={row.id} className={`${s.auditRow} ${s.recordRow}`} onClick={() => openTrace(row)} disabled={busyTrace === row.id}>
      <div className={s.recordHead}><strong>{row.executor} · {row.model}</strong><span className={`${s.recordStatus} ${s[`record_${row.status}`] || ""}`}>{row.status}{row.http_status ? ` · HTTP ${row.http_status}` : ""}</span></div>
      <span>{row.mission} · {fmtDateTime(row.time)}</span>
      <div className={s.usageGrid}><span>输入 <b>{(row.input_tokens || 0).toLocaleString()}</b></span><span>输出 <b>{(row.output_tokens || 0).toLocaleString()}</b></span><span>总计 <b>{(row.total_tokens || 0).toLocaleString()}</b></span><span>延迟 <b>{(row.latency_ms || 0).toLocaleString()} ms</b></span></div>
      {row.error && <small className={s.recordError}>{row.error}</small>}{busyTrace === row.id && <small>正在加载 trace…</small>}
    </button>)}</div>
    {trace && <section className={s.traceDetail}>
      <div className={s.traceHead}><h3>调用 {trace.usage?.id}</h3><span>{trace.usage?.model} · {trace.usage?.total_tokens || 0} tokens</span></div>
      <details open><summary>请求 Request</summary><pre>{JSON.stringify(trace.request || {}, null, 2)}</pre></details>
      {trace.response && <details><summary>响应 Response</summary><pre>{JSON.stringify(trace.response, null, 2)}</pre></details>}
      {trace.provider_error && <div className={s.formResult}><Icon name="alert" />{trace.provider_error}</div>}
    </section>}
  </Modal>;
}

function ApprovalsModal({ data, error, onRefresh, onClose }) {
  const [localError, setLocalError] = useState("");
  const [busy, setBusy] = useState("");
  const decide = async (id, approve) => {
    setBusy(id); setLocalError("");
    try {
      const response = await fetch("/api/approvals/decision", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id, approve }) });
      const body = await response.json().catch(() => ({}));
      if (!response.ok || !body.ok) throw new Error(body.error || "审批失败");
      await onRefresh();
    } catch (err) { setLocalError(err.message || String(err)); }
    setBusy("");
  };
  const pending = data.pending || [];
  const rules = data.rules || [];
  const audit = data.audit || [];
  return <Modal title="工具审批" eyebrow="PRE TOOL USE" onClose={onClose} className={s.approvalsModal}>
    <p className={s.fieldHint}>命中 ask 规则的工具调用会在这里等待操作人员决定；超过两分钟自动拒绝。</p>
    {(localError || error) && <p className={s.inlineError}>{localError || error}</p>}
    <h3 className={s.auditTitle}>待处理 · {pending.length}</h3>
    <div className={s.auditList}>{pending.length === 0 ? <p className={s.inlineEmpty}>当前没有待审批调用</p> : pending.map((item) => {
      const seconds = Math.max(0, Math.ceil((new Date(item.deadline).getTime() - Date.now()) / 1000));
      return <article className={s.approvalCard} key={item.id}>
        <div className={s.approvalHead}><strong>{item.tool} · {item.executor}</strong><span className={seconds < 30 ? s.deadlineUrgent : ""}>{seconds > 0 ? `${seconds}s 后超时` : "已超时"}</span></div>
        <small>{item.mission} · {item.reason}</small><pre>{item.arguments}</pre>
        <div className={s.modalActions}><button type="button" onClick={() => decide(item.id, false)} disabled={!!busy}>{busy === item.id ? "处理中…" : "拒绝"}</button><button type="button" className={s.primaryButton} onClick={() => decide(item.id, true)} disabled={!!busy}>{busy === item.id ? "处理中…" : "批准本次调用"}</button></div>
      </article>;
    })}</div>
    <h3 className={s.auditTitle}>命中规则 · {rules.length}</h3>
    <div className={s.auditList}>{rules.length === 0 ? <p className={s.inlineEmpty}>当前没有自定义工具规则</p> : rules.map((rule, index) => <div className={s.auditRow} key={`${rule.tool}-${rule.priority}-${index}`}>
      <strong><span className={`${s.ruleAction} ${s[`rule_${rule.action}`] || ""}`}>{rule.action}</span> · {rule.tool} <small>优先级 {rule.priority || 0}</small></strong>
      <span>{rule.argument_contains ? `参数包含：${rule.argument_contains}` : "匹配全部参数"}{rule.reason ? ` · ${rule.reason}` : ""}</span>
    </div>)}</div>
    <h3 className={s.auditTitle}>近期审计</h3>
    <div className={s.auditList}>{audit.length === 0 ? <p className={s.inlineEmpty}>暂无工具审计记录</p> : audit.slice().reverse().slice(0, 50).map((row, index) => <div className={s.auditRow} key={`${row.time}-${index}`}>
      <strong>{row.action} · {row.tool}</strong><span>{row.executor} · {row.mission}{row.reason ? ` · ${row.reason}` : ""}</span><small>{fmtDateTime(row.time)}{row.pending ? ` · ${row.pending}` : ""}</small>
    </div>)}</div>
  </Modal>;
}

function ScopeModal({ challenge, initial, onClose, onSaved }) {
  const [value, setValue] = useState((initial || []).join("\n"));
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState(null);

  useEffect(() => {
    const controller = new AbortController();
    fetch(`/api/scope?challenge=${encodeURIComponent(challenge || "")}`, { signal: controller.signal })
      .then((response) => response.json())
      .then((data) => {
        if (data.editable) setValue((data.targets || []).join("\n"));
        else setResult({ ok: false, text: "当前任务没有可编辑的授权范围" });
      })
      .catch((error) => { if (error.name !== "AbortError") setResult({ ok: false, text: error.message || String(error) }); });
    return () => controller.abort();
  }, [challenge]);

  const save = async () => {
    const targets = value.split(/[\n,]+/).map((item) => item.trim()).filter(Boolean);
    setBusy(true); setResult(null);
    try {
      const response = await fetch("/api/scope", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ challenge, targets }) });
      const body = await response.json().catch(() => ({}));
      if (!response.ok || body.ok === false) throw new Error(body.error || `请求失败 (${response.status})`);
      setResult({ ok: true, text: `授权范围已更新：${body.description}` });
      onSaved?.();
      setTimeout(onClose, 700);
    } catch (error) { setResult({ ok: false, text: error.message || String(error) }); }
    setBusy(false);
  };

  return <Modal title="授权范围" eyebrow="SCOPE CONTROL" onClose={onClose} className={s.formModal}>
    <div className={s.modalNotice}><Icon name="shield" size={18} /><p>逐行填写获授权的主机、IP、URL 或 CIDR。<strong>变更范围会结束当前执行器并创建隔离的新任务</strong>，旧会话、黑板和流量记录保留归档；URL 按主机授权，不按路径限制。</p></div>
    <label className={s.scopeField}><span>授权目标</span><textarea value={value} onChange={(event) => setValue(event.target.value)} placeholder={"https://staging.example.com\n10.10.5.0/24\n192.0.2.10"} rows={8} /></label>
    <p className={s.fieldHint}>后端会对 HTTP、TCP 和本地脚本网络访问继续执行强制校验。</p>
    {result && <div className={`${s.formResult} ${result.ok ? s.resultOk : s.resultBad}`}><Icon name={result.ok ? "check" : "alert"} />{result.text}</div>}
    <div className={s.modalActions}><button type="button" onClick={onClose} disabled={busy}>取消</button><button type="button" className={s.primaryButton} onClick={save} disabled={busy || !value.trim()}>{busy ? "正在切换…" : "保存并切换任务"}</button></div>
  </Modal>;
}

function SettingsModal({ initial, onClose, onSaved }) {
  const match = PRESETS.find((preset) => preset.base === (initial?.base_url || "")) || PRESETS.find((preset) => preset.id === (initial?.provider || "")) || PRESETS[PRESETS.length - 1];
  const [provider, setProvider] = useState(match.id);
  const [baseURL, setBaseURL] = useState(initial?.base_url || match.base);
  const [model, setModel] = useState(initial?.model || match.model);
  const [contextWindow, setContextWindow] = useState(initial?.context_window_tokens || "");
  const [apiKey, setApiKey] = useState("");
  const [fallbacks, setFallbacks] = useState((initial?.fallbacks || []).map((item) => ({ ...item, api_key: "" })));
  const [busy, setBusy] = useState("");
  const [test, setTest] = useState(null);
  const hasKey = !!initial?.has_key;

  const pickPreset = (id) => {
    const preset = PRESETS.find((item) => item.id === id) || PRESETS[0];
    setProvider(id);
    if (preset.id !== "custom") { setBaseURL(preset.base); setModel(preset.model); }
  };
  const body = () => ({ provider, base_url: baseURL.trim(), model: model.trim(), api_key: apiKey, context_window_tokens: Number(contextWindow) || 0,
    fallbacks: fallbacks.map((item, index) => ({ name: item.name || `backup-${index + 1}`, provider: item.provider || "custom", base_url: (item.base_url || "").trim(), model: (item.model || "").trim(), api_key: item.api_key || "", context_window_tokens: Number(item.context_window_tokens) || 0, priority: index + 1 })) });
  const updateFallback = (index, field, value) => setFallbacks((items) => items.map((item, i) => i === index ? { ...item, [field]: value } : item));
  const save = async () => {
    setBusy("save"); setTest(null);
    try {
      const response = await fetch("/api/llm", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body()) });
      const data = await response.json().catch(() => ({}));
      if (!response.ok || data.ok === false) throw new Error(data.error || `请求失败 (${response.status})`);
      setApiKey(""); onSaved?.(); onClose();
    } catch (error) { setTest({ ok: false, error: error.message || String(error) }); }
    setBusy("");
  };
  const doTest = async () => {
    setBusy("test"); setTest(null);
    try {
      const response = await fetch("/api/llm/test", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body()) });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.error || `请求失败 (${response.status})`);
      setTest(data);
    } catch (error) { setTest({ ok: false, error: error.message || String(error) }); }
    setBusy("");
  };

  return <Modal title="模型设置" eyebrow="MODEL CONFIGURATION" onClose={onClose} className={s.formModal}>
    <p className={s.modalHint}>可自行填写任意 OpenAI 兼容模型，无需从预设中选择。未指定 --model 或 --models-file 时，保存后运行中的 Agent 会在下一轮自动切换。</p>
    <div className={s.formGrid}>
      <label className={s.field}><span>提供商</span><select value={provider} onChange={(event) => pickPreset(event.target.value)}>{PRESETS.map((preset) => <option key={preset.id} value={preset.id}>{preset.name}</option>)}</select></label>
      <label className={`${s.field} ${s.fieldWide}`}><span>Base URL</span><input value={baseURL} onChange={(event) => setBaseURL(event.target.value)} placeholder="https://api.deepseek.com/v1" /></label>
      <label className={s.field}><span>模型</span><input value={model} onChange={(event) => setModel(event.target.value)} placeholder="deepseek-chat" /></label>
      <label className={s.field}><span>上下文窗口</span><input type="number" min="4096" value={contextWindow} onChange={(event) => setContextWindow(event.target.value)} placeholder={`自动 ${initial?.effective_context_window_tokens || 16000}`} /></label>
      <label className={`${s.field} ${s.fieldWide}`}><span>API Key</span><input type="password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder={hasKey ? `已保存 ${initial.key_hint || "••••"}，留空则不修改` : "sk-…"} autoComplete="off" /></label>
    </div>
    <h3 className={s.auditTitle}>备用模型链</h3>
    <p className={s.fieldHint}>仅主模型在输出前失败时按顺序尝试备用模型；任务取消和无效请求不会切换。备用模型的密钥单独保存。</p>
    {fallbacks.map((item, index) => <div className={s.fallbackCard} key={index}>
      <div className={s.fallbackHead}><strong>备用 {index + 1}</strong><button type="button" onClick={() => setFallbacks((items) => items.filter((_, i) => i !== index))}>移除</button></div>
      <div className={s.formGrid}>
        <label className={`${s.field} ${s.fieldWide}`}><span>Base URL</span><input value={item.base_url || ""} onChange={(event) => updateFallback(index, "base_url", event.target.value)} placeholder="https://api.example.com/v1" /></label>
        <label className={s.field}><span>模型</span><input value={item.model || ""} onChange={(event) => updateFallback(index, "model", event.target.value)} /></label>
        <label className={s.field}><span>上下文窗口</span><input type="number" min="4096" value={item.context_window_tokens || ""} onChange={(event) => updateFallback(index, "context_window_tokens", event.target.value)} /></label>
        <label className={`${s.field} ${s.fieldWide}`}><span>API Key</span><input type="password" value={item.api_key || ""} onChange={(event) => updateFallback(index, "api_key", event.target.value)} placeholder={item.has_key ? "已保存；Base URL 不变时留空可沿用" : "填写备用模型密钥"} autoComplete="off" /></label>
      </div>
    </div>)}
    <button type="button" className={s.headerButton} onClick={() => setFallbacks((items) => [...items, { base_url: "", model: "", api_key: "", context_window_tokens: "" }])}>添加备用模型</button>
    {test && <div className={`${s.formResult} ${test.ok ? s.resultOk : s.resultBad}`}><Icon name={test.ok ? "check" : "alert"} />{test.ok ? "连接成功，密钥与模型可用" : test.error}</div>}
    <div className={s.modalActions}><button type="button" onClick={doTest} disabled={!!busy || !baseURL.trim() || !model.trim()}>{busy === "test" ? "测试中…" : "测试连接"}</button><span className={s.spacer} /><button type="button" onClick={onClose} disabled={!!busy}>取消</button><button type="button" className={s.primaryButton} onClick={save} disabled={!!busy || !baseURL.trim() || !model.trim() || (!apiKey && !hasKey)}>{busy === "save" ? "保存中…" : "保存设置"}</button></div>
  </Modal>;
}
