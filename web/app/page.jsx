"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import s from "./page.module.css";

const EMPTY = {
  summary: {}, findings: [], subgoals: [], traffic: [], swarms: {}, mode: "", engagement: null,
};

const VERDICT_LABEL = { confirmed: "确认", suspected: "疑似", failed: "失败" };
const KIND_LABEL = {
  operator: "操作员", text: "推理", tool_call: "调用", tool_result: "结果", error: "错误",
};

// flatten swarms{name:{agents:{spec:{...}}}} → [{challenge,model,status,steps,paused}]
function flattenAgents(swarms) {
  const out = [];
  for (const [name, st] of Object.entries(swarms || {})) {
    for (const [m, a] of Object.entries(st.agents || {})) {
      out.push({ challenge: name, model: m, status: a.status, steps: a.steps || 0, paused: !!st.paused });
    }
  }
  return out;
}

function fmtTime(ts) {
  if (!ts) return "";
  try { return new Date(ts * 1000).toLocaleTimeString(); } catch { return ""; }
}

function Tile({ label, value, kind }) {
  return (
    <div className={`${s.tile} ${kind ? s[kind] : ""}`}>
      <b>{value ?? 0}</b>
      <span>{label}</span>
    </div>
  );
}

export default function Console() {
  const [state, setState] = useState(EMPTY);
  const [watch, setWatch] = useState({ c: null, m: null });
  const [events, setEvents] = useState([]);
  const [msg, setMsg] = useState("");
  const [connected, setConnected] = useState(false);
  const afterRef = useRef(0);
  const logRef = useRef(null);
  const watchRef = useRef(watch);
  watchRef.current = watch;

  // live snapshot stream
  useEffect(() => {
    fetch("/api/state").then((r) => r.json()).then((j) => { setState(j); setConnected(true); }).catch(() => {});
    const es = new EventSource("/api/stream");
    es.onmessage = (e) => { try { setState(JSON.parse(e.data)); setConnected(true); } catch {} };
    es.onerror = () => setConnected(false);
    return () => es.close();
  }, []);

  const agents = useMemo(() => flattenAgents(state.swarms), [state.swarms]);

  // auto-attach to the first live executor so the dialogue works out of the box
  useEffect(() => {
    if (watch.c && agents.some((a) => a.challenge === watch.c && a.model === watch.m)) return;
    const live = agents.find((a) => a.status === "running") || agents[0];
    if (live) { setWatch({ c: live.challenge, m: live.model }); setEvents([]); afterRef.current = 0; }
  }, [agents, watch.c, watch.m]);

  // per-executor activity → the dialogue + output timeline (ordered by seq)
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      const w = watchRef.current;
      if (alive && w.c && w.m) {
        try {
          const r = await fetch(`/api/activity?challenge=${encodeURIComponent(w.c)}&model=${encodeURIComponent(w.m)}&after=${afterRef.current}`);
          const j = await r.json();
          const evs = j.events || [];
          if (evs.length) {
            afterRef.current = Math.max(afterRef.current, ...evs.map((e) => e.seq || 0));
            setEvents((prev) => [...prev, ...evs]);
          }
        } catch {}
      }
      if (alive) setTimeout(tick, 1000);
    };
    const t = setTimeout(tick, 300);
    return () => { alive = false; clearTimeout(t); };
  }, []);

  useEffect(() => { if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight; }, [events]);

  const cmd = useCallback((action, extra) => {
    const w = watchRef.current;
    fetch("/api/command", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, challenge: w.c, model: w.m, ...(extra || {}) }),
    }).catch(() => {});
  }, []);

  const pickWatch = (c, m) => { setWatch({ c, m }); setEvents([]); afterRef.current = 0; };
  const send = () => {
    const t = msg.trim();
    if (!t || !watch.c) return;
    cmd("bump", { message: t });           // appears in the timeline as an operator turn
    setMsg("");
  };

  const sm = state.summary || {};
  const findings = state.findings || [];
  const subgoals = state.subgoals || [];
  const traffic = state.traffic || [];
  const modeLabel = state.mode === "engagement" ? "assessment" : state.mode === "scheduler" ? "scheduler" : "—";
  const scope = state.engagement?.scope || "";
  const cur = agents.find((a) => a.challenge === watch.c && a.model === watch.m);

  // progress toward the target
  const gTotal = sm.goals_total || 0;
  const gDone = sm.goals_done || 0;
  const pct = gTotal > 0 ? Math.round((gDone / gTotal) * 100) : 0;

  // current action = most recent tool_call in the watched stream
  const lastCall = useMemo(() => {
    for (let i = events.length - 1; i >= 0; i--) if (events[i].kind === "tool_call") return events[i];
    return null;
  }, [events]);

  // behavior visualization: tool-usage + action-type mix for the watched executor
  const behavior = useMemo(() => {
    const tools = {}; const kinds = {}; let total = 0;
    for (const e of events) {
      kinds[e.kind] = (kinds[e.kind] || 0) + 1;
      if (e.kind === "tool_call" && e.tool) { tools[e.tool] = (tools[e.tool] || 0) + 1; total++; }
    }
    const toolRows = Object.entries(tools).sort((a, b) => b[1] - a[1]);
    const max = toolRows.reduce((m, [, n]) => Math.max(m, n), 1);
    return { toolRows, max, total, kinds };
  }, [events]);

  return (
    <div className={s.app}>
      <header className={s.head}>
        <div className={s.brand}>
          <span className={s.dot} data-on={connected ? "1" : "0"} />
          <h1>chronos</h1>
          <span className={s.mode}>{modeLabel}</span>
          {state.engagement?.name && <span className={s.eng}>{state.engagement.name}</span>}
        </div>
        <div className={s.meta}>
          {scope && <span className={s.scope} title={scope}>scope: {scope}</span>}
          {sm.budget_left_min != null && sm.budget_left_min >= 0 && <span>budget {sm.budget_left_min}min</span>}
          <span>{(sm.tokens || 0).toLocaleString()} tokens</span>
        </div>
      </header>

      <section className={s.tiles}>
        <Tile label="确认 confirmed" value={sm.confirmed} kind="confirmed" />
        <Tile label="疑似 suspected" value={sm.suspected} kind="suspected" />
        <Tile label="失败 failed" value={sm.failed} kind="failed" />
        <Tile label="findings" value={sm.findings} />
        <Tile label="facts" value={sm.facts} />
        <Tile label="executors" value={agents.length} />
      </section>

      <div className={s.grid}>
        {/* LEFT — progress toward target + behavior visualization */}
        <aside className={s.left}>
          <div className={s.panel}>
            <h2>进展 Progress</h2>
            <div className={s.progHead}>
              <span>目标覆盖 {gDone}/{gTotal}</span>
              <span className={s.pct}>{pct}%</span>
            </div>
            <div className={s.bar}><i style={{ width: `${pct}%` }} /></div>
            <div className={s.curWrap}>
              <div className={s.curLabel}>当前动作 current action</div>
              <div className={s.cur}>
                {cur ? <><b>{cur.status}</b> · {cur.steps} steps</> : "no executor"}
                {lastCall && <div className={s.curTool}>→ {lastCall.tool}{lastCall.text ? `: ${lastCall.text.slice(0, 90)}` : ""}</div>}
              </div>
            </div>
            <div className={s.checkWrap}>
              {subgoals.length === 0 && <div className={s.empty}>no sub-goals yet</div>}
              {subgoals.map((g) => (
                <div key={g.id} className={`${s.check} ${g.done ? s.done : ""}`}>
                  <span className={s.box}>{g.done ? "✓" : "○"}</span>
                  <span className={s.ckText}>{g.description}</span>
                </div>
              ))}
            </div>
          </div>

          <div className={s.panel}>
            <h2>行为 Behavior</h2>
            {behavior.toolRows.length === 0 && <div className={s.empty}>no tool activity yet</div>}
            <div className={s.bhv}>
              {behavior.toolRows.map(([tool, n]) => (
                <div key={tool} className={s.bhvRow}>
                  <span className={s.bhvName}>{tool}</span>
                  <span className={s.bhvBar}><i style={{ width: `${(n / behavior.max) * 100}%` }} /></span>
                  <span className={s.bhvN}>{n}</span>
                </div>
              ))}
            </div>
            {behavior.total > 0 && (
              <div className={s.kinds}>
                {["text", "tool_call", "tool_result", "operator"].map((k) =>
                  behavior.kinds[k] ? <span key={k} className={s[`k_${k}`]}>{KIND_LABEL[k]} {behavior.kinds[k]}</span> : null
                )}
              </div>
            )}
          </div>
        </aside>

        {/* CENTER — human↔agent dialogue + agent output timeline */}
        <main className={s.center}>
          <div className={s.chatHead}>
            <div>
              <h2>对话 & 输出 Dialogue &amp; Output</h2>
              <select className={s.sel} value={cur ? `${watch.c}||${watch.m}` : ""}
                onChange={(e) => { const [c, m] = e.target.value.split("||"); pickWatch(c, m); }}>
                {agents.length === 0 && <option value="">no executors</option>}
                {agents.map((a) => (
                  <option key={a.challenge + a.model} value={`${a.challenge}||${a.model}`}>
                    {a.challenge} / {a.model} · {a.status}
                  </option>
                ))}
              </select>
            </div>
            <div className={s.ctrls}>
              <button onClick={() => cmd("pause")} disabled={!watch.c}>pause</button>
              <button onClick={() => cmd("resume")} disabled={!watch.c}>resume</button>
              <button className={s.kill} onClick={() => cmd("kill")} disabled={!watch.c}>kill</button>
            </div>
          </div>

          <div className={s.log} ref={logRef}>
            {events.map((e, i) => {
              const op = e.kind === "operator";
              return (
                <div key={e.seq || i} className={`${s.turn} ${op ? s.op : s.ag}`}>
                  <div className={s.turnHead}>
                    <span className={`${s.badge} ${s[`b_${e.kind}`] || ""}`}>{KIND_LABEL[e.kind] || e.kind}</span>
                    {e.tool && <span className={s.toolTag}>{e.tool}</span>}
                    {op ? <span className={s.who}>你</span> : <span className={s.who}>{watch.m}</span>}
                  </div>
                  <div className={`${s.bubble} ${e.kind === "error" ? s.errBubble : ""}`}>{e.text}</div>
                </div>
              );
            })}
            {watch.c && events.length === 0 && <div className={s.empty}>waiting for activity…</div>}
            {!watch.c && <div className={s.empty}>no executor attached</div>}
          </div>

          <div className={s.inputBar}>
            <input placeholder={watch.c ? "给 Agent 下达指令…  (Enter 发送)" : "no executor to talk to"}
              value={msg} disabled={!watch.c}
              onChange={(e) => setMsg(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") send(); }} />
            <button className={s.send} onClick={send} disabled={!watch.c || !msg.trim()}>发送</button>
          </div>
        </main>

        {/* RIGHT — findings + live traffic detection */}
        <aside className={s.right}>
          <div className={s.panel}>
            <h2>Findings</h2>
            {findings.length === 0 && <div className={s.empty}>no findings yet</div>}
            <div className={s.flist}>
              {findings.map((f) => (
                <div key={f.id} className={s.finding}>
                  <div className={s.frow}>
                    <span className={`${s.sev} ${s["sev_" + f.severity] || ""}`}>{f.severity}</span>
                    <span className={`${s.verdict} ${s["v_" + f.verdict] || ""}`}>{VERDICT_LABEL[f.verdict] || f.verdict}</span>
                    <span className={s.ftitle}>{f.title}</span>
                  </div>
                  <div className={s.fmeta}>
                    {f.target ? `${f.target} · ` : ""}{f.category || "—"}
                    {f.evidence > 0 ? ` · ${f.evidence} evidence` : ""}
                  </div>
                </div>
              ))}
            </div>
          </div>

          <div className={s.panel}>
            <h2>流量检测 Traffic <span className={s.count}>{traffic.length}</span></h2>
            {traffic.length === 0 && <div className={s.empty}>no traffic captured yet</div>}
            <div className={s.traf}>
              {[...traffic].reverse().map((t) => (
                <div key={t.id} className={s.trow}>
                  <span className={s.tid}>{t.id}</span>
                  <span className={`${s.tkind} ${s["tk_" + t.kind] || ""}`}>{t.method || t.kind}</span>
                  <span className={s.ttarget} title={t.target}>{t.target}</span>
                  {t.status ? <span className={`${s.tstat} ${t.status >= 400 ? s.bad : t.status >= 300 ? s.warn : s.ok}`}>{t.status}</span> : <span className={s.tstat} />}
                  <span className={s.tlen}>{t.resp_len ? `${t.resp_len}B` : ""}</span>
                </div>
              ))}
            </div>
          </div>
        </aside>
      </div>
    </div>
  );
}
