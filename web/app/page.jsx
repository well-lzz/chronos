"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import s from "./page.module.css";

const EMPTY = { summary: {}, challenges: [], swarms: {}, deferred: {} };

function Tile({ label, value }) {
  return (
    <div className={s.tile}>
      <b>{value ?? 0}</b>
      <span>{label}</span>
    </div>
  );
}

export default function Console() {
  const [state, setState] = useState(EMPTY);
  const [filter, setFilter] = useState("");
  const [watch, setWatch] = useState({ c: null, m: null });
  const [events, setEvents] = useState([]);
  const afterRef = useRef(0);
  const logRef = useRef(null);
  const watchRef = useRef(watch);
  watchRef.current = watch;

  // Live snapshot via SSE, with a one-shot fetch fallback.
  useEffect(() => {
    fetch("/api/state").then((r) => r.json()).then(setState).catch(() => {});
    const es = new EventSource("/api/stream");
    es.onmessage = (e) => {
      try { setState(JSON.parse(e.data)); } catch {}
    };
    return () => es.close();
  }, []);

  // Poll the watched executor's activity.
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      const w = watchRef.current;
      if (alive && w.c && w.m) {
        try {
          const r = await fetch(
            `/api/activity?challenge=${encodeURIComponent(w.c)}&model=${encodeURIComponent(w.m)}&after=${afterRef.current}`
          );
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

  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [events]);

  const cmd = useCallback((action, challenge, extra) => {
    fetch("/api/command", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, challenge, ...(extra || {}) }),
    }).catch(() => {});
  }, []);

  const pickWatch = (c, m) => { setWatch({ c, m }); setEvents([]); afterRef.current = 0; };

  const sm = state.summary || {};
  const challenges = (state.challenges || [])
    .filter((c) => !filter || c.status === filter)
    .sort((a, b) => (b.value - a.value) || a.name.localeCompare(b.name));
  const swarms = state.swarms || {};
  const budget = sm.budget_left_min != null && sm.budget_left_min >= 0 ? `· budget ${sm.budget_left_min}min left` : "";
  const [bumpMsg, setBumpMsg] = useState("");

  return (
    <div className={s.wrap}>
      <div className={s.head}>
        <h1>chronos console</h1>
        <span className={s.budget}>{budget}</span>
      </div>

      <div className={s.tiles}>
        <Tile label="solved" value={sm.solved} />
        <Tile label="unsolved" value={sm.unsolved} />
        <Tile label="active" value={sm.active} />
        <Tile label="preparing" value={sm.preparing} />
        <Tile label="deferred" value={sm.deferred} />
        <Tile label="tokens" value={sm.total_tokens} />
      </div>

      <div className={s.toolbar}>
        <label>filter</label>
        <select value={filter} onChange={(e) => setFilter(e.target.value)}>
          <option value="">all</option>
          <option>active</option>
          <option>preparing</option>
          <option>deferred</option>
          <option>unsolved</option>
          <option>solved</option>
        </select>
        <button onClick={() => cmd("resume_all", "")}>resume all deferred</button>
      </div>

      <div className={s.grid}>
        {challenges.map((c) => (
          <div key={c.name} className={`${s.ch} ${s[c.status] || ""}`}>
            <div className={s.chtop}>
              <span className={s.nm}>{c.name}</span>
              <span className={`${s.pill} ${s[c.status] || ""}`}>{c.status}</span>
            </div>
            <div className={s.meta}>
              {c.value}pts · L{c.level} · {c.category || "—"} · {c.correct_flag_count}/{c.flag_count} flags
            </div>
          </div>
        ))}
        {challenges.length === 0 && <div className={s.empty}>no challenges</div>}
      </div>

      <div className={s.row}>
        <div className={s.panel}>
          <h2>Swarms</h2>
          {Object.keys(swarms).length === 0 && <div className={s.empty}>no active swarms</div>}
          {Object.entries(swarms).map(([name, st]) =>
            Object.entries(st.agents || {}).map(([m, a]) => {
              const sel = watch.c === name && watch.m === m;
              return (
                <div key={name + m} className={`${s.agent} ${sel ? s.sel : ""}`} onClick={() => pickWatch(name, m)}>
                  <span>{name} / {m}</span>
                  <span className={s.st}>{a.status} · {a.steps} steps{st.paused ? " · paused" : ""}</span>
                </div>
              );
            })
          )}
        </div>

        <div className={s.panel}>
          <h2>Watch: {watch.c ? `${watch.c} / ${watch.m}` : <span className={s.empty}>pick an executor</span>}</h2>
          {watch.c && (
            <div className={s.controls}>
              <button onClick={() => cmd("pause", watch.c)}>pause</button>
              <button onClick={() => cmd("resume", watch.c)}>resume</button>
              <button onClick={() => cmd("kill", watch.c)}>kill</button>
              <input
                placeholder="guidance to the agent…"
                value={bumpMsg}
                onChange={(e) => setBumpMsg(e.target.value)}
                onKeyDown={(e) => { if (e.key === "Enter" && bumpMsg.trim()) { cmd("bump", watch.c, { model: watch.m, message: bumpMsg.trim() }); setBumpMsg(""); } }}
              />
              <button onClick={() => { if (bumpMsg.trim()) { cmd("bump", watch.c, { model: watch.m, message: bumpMsg.trim() }); setBumpMsg(""); } }}>send</button>
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
    </div>
  );
}
