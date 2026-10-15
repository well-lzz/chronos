"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import s from "./page.module.css";

const EMPTY = { summary: {}, findings: [], swarms: {}, mode: "" };

const VERDICT_LABEL = { confirmed: "确认", suspected: "疑似", failed: "失败" };

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
  const [bumpMsg, setBumpMsg] = useState("");
  const afterRef = useRef(0);
  const logRef = useRef(null);
  const watchRef = useRef(watch);
  watchRef.current = watch;

  useEffect(() => {
    fetch("/api/state").then((r) => r.json()).then(setState).catch(() => {});
    const es = new EventSource("/api/stream");
    es.onmessage = (e) => { try { setState(JSON.parse(e.data)); } catch {} };
    return () => es.close();
  }, []);

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
            afterRef.current = Math.max(afterRef.current, ...evs.map((e) => e.step));
            setEvents((prev) => [...prev, ...evs]);
          }
        } catch {}
      }
      if (alive) setTimeout(tick, 1200);
    };
    const t = setTimeout(tick, 400);
    return () => { alive = false; clearTimeout(t); };
  }, []);

  useEffect(() => { if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight; }, [events]);

  const cmd = useCallback((action, challenge, extra) => {
    fetch("/api/command", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, challenge, ...(extra || {}) }),
    }).catch(() => {});
  }, []);

  const pickWatch = (c, m) => { setWatch({ c, m }); setEvents([]); afterRef.current = 0; };
  const sendBump = () => { if (bumpMsg.trim() && watch.c) { cmd("bump", watch.c, { model: watch.m, message: bumpMsg.trim() }); setBumpMsg(""); } };

  const sm = state.summary || {};
  const findings = state.findings || [];
  const swarms = state.swarms || {};
  const budget = sm.budget_left_min != null && sm.budget_left_min >= 0 ? `· budget ${sm.budget_left_min}min left` : "";
  const modeLabel = state.mode === "engagement" ? "assessment" : state.mode === "scheduler" ? "scheduler" : "";

  return (
    <div className={s.wrap}>
      <div className={s.head}>
        <h1>chronos console</h1>
        <span className={s.budget}>{modeLabel && `· ${modeLabel} `}{budget}</span>
      </div>

      <div className={s.tiles}>
        <Tile label="确认 confirmed" value={sm.confirmed} kind="confirmed" />
        <Tile label="疑似 suspected" value={sm.suspected} kind="suspected" />
        <Tile label="失败 failed" value={sm.failed} kind="failed" />
        <Tile label="发现 findings" value={sm.findings} />
        <Tile label="facts" value={sm.facts} />
        <Tile label="tokens" value={sm.tokens} />
      </div>

      <div className={s.cols}>
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
          <h2>Executors</h2>
          {Object.keys(swarms).length === 0 && <div className={s.empty}>no active executors</div>}
          {Object.entries(swarms).map(([name, st]) =>
            Object.entries(st.agents || {}).map(([m, a]) => {
              const sel = watch.c === name && watch.m === m;
              return (
                <div key={name + m} className={`${s.agent} ${sel ? s.sel : ""}`} onClick={() => pickWatch(name, m)}>
                  <span className={s.anm}>{name} / {m}</span>
                  <span className={s.st}>{a.status} · {a.steps} steps{st.paused ? " · paused" : ""}</span>
                </div>
              );
            })
          )}
        </div>
      </div>

      <div className={s.panel} style={{ marginTop: "1rem" }}>
        <h2>Watch: {watch.c ? `${watch.c} / ${watch.m}` : <span className={s.empty}>pick an executor</span>}</h2>
        {watch.c && (
          <div className={s.controls}>
            <button onClick={() => cmd("pause", watch.c)}>pause</button>
            <button onClick={() => cmd("resume", watch.c)}>resume</button>
            <button onClick={() => cmd("kill", watch.c)}>kill</button>
            <input placeholder="guidance to the agent…" value={bumpMsg}
              onChange={(e) => setBumpMsg(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") sendBump(); }} />
            <button onClick={sendBump}>send</button>
          </div>
        )}
        <div className={s.log} ref={logRef}>
          {events.map((e, i) => (
            <div key={i} className={`${s.ev} ${s[e.kind] || ""}`}>
              <span className={s.evh}>#{e.step} {e.kind}{e.tool ? " " + e.tool : ""}</span>
              {"\n"}{e.text}
            </div>
          ))}
          {watch.c && events.length === 0 && <div className={s.empty}>waiting for activity…</div>}
        </div>
      </div>
    </div>
  );
}
