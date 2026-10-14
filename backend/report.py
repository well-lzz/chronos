"""Mission report rendering — blackboard → report.json + report.md + report.html.

A mission's product is its blackboard (Facts / Findings / Sub-Goals). This module
renders that, plus run metadata (mode, scope, timing, models, cost), into three
local artifacts (docs/mission-modes.md §5):

  - report.json : the structured record (machine-readable, the source of truth)
  - report.md   : a readable write-up
  - report.html : a self-contained page (same content, styled, no external deps)

Pure stdlib, no network. Findings are ordered by severity (critical first) then
by status (verified before candidate), so the most important, confirmed issues
read first. When a mission captured a flag, it appears as a verified Finding
(category=flag) and renders like any other, with its evidence.
"""

from __future__ import annotations

import html
import json
import time
from pathlib import Path
from typing import Any

from backend.blackboard import SEVERITIES, Blackboard

# critical → info; verified before candidate before refuted
_SEV_RANK = {s: i for i, s in enumerate(reversed(SEVERITIES))}
_STATUS_RANK = {"verified": 0, "candidate": 1, "refuted": 2}


def _finding_sort_key(f: dict[str, Any]) -> tuple[int, int]:
    return (
        _SEV_RANK.get(f.get("severity", "info"), len(SEVERITIES)),
        _STATUS_RANK.get(f.get("status", "candidate"), 9),
    )


def build_report(blackboard: Blackboard, run_meta: dict[str, Any] | None = None) -> dict[str, Any]:
    """Assemble the structured report dict from a blackboard + run metadata."""
    data = blackboard.to_dict()
    findings = sorted(data.get("findings", []), key=_finding_sort_key)
    return {
        "mission": {
            "name": blackboard.mission_id,
            **(run_meta or {}),
        },
        "generated_at": time.strftime("%Y-%m-%d %H:%M:%S %z") or time.strftime("%Y-%m-%d %H:%M:%S"),
        "summary": blackboard.counts(),
        "findings": findings,
        "facts": data.get("facts", []),
        "subgoals": data.get("subgoals", []),
    }


# ---------------------------------------------------------------------------- #
#  Markdown
# ---------------------------------------------------------------------------- #
def _md_meta(mission: dict[str, Any]) -> list[str]:
    lines = [f"# Mission report — {mission.get('name', 'mission')}", ""]
    order = ("mode", "goal", "scope", "models", "started", "ended",
             "duration_seconds", "cost_usd")
    labels = {
        "mode": "Mode", "goal": "Goal", "scope": "Scope", "models": "Models",
        "started": "Started", "ended": "Ended", "duration_seconds": "Duration (s)",
        "cost_usd": "Cost (USD)",
    }
    for k in order:
        v = mission.get(k)
        if v in (None, "", []):
            continue
        if isinstance(v, list):
            v = ", ".join(str(x) for x in v)
        lines.append(f"- **{labels[k]}**: {v}")
    lines.append("")
    return lines


def render_markdown(report: dict[str, Any]) -> str:
    m = report["mission"]
    s = report["summary"]
    lines = _md_meta(m)
    lines += [
        "## Summary",
        f"- Findings: **{s['findings']}** "
        f"(verified {s['findings_verified']}, candidate {s['findings_candidate']})",
        f"- Facts established: {s['facts']}",
        f"- Sub-goals: {s['subgoals_total']} ({s['subgoals_open']} open)",
        "",
        "## Findings",
    ]
    if not report["findings"]:
        lines += ["_No findings recorded._", ""]
    for f in report["findings"]:
        title = f.get("title", "(untitled)")
        sev = f.get("severity", "info").upper()
        status = f.get("status", "candidate")
        lines.append(f"### [{sev}] {title}  ·  _{status}_")
        if f.get("target"):
            lines.append(f"- **Target**: {f['target']}")
        if f.get("category"):
            lines.append(f"- **Category**: {f['category']}")
        if f.get("evidence"):
            lines += ["- **Evidence**:", "", "  ```", *[f"  {ln}" for ln in str(f['evidence']).splitlines()], "  ```"]
        if f.get("reproduction"):
            lines += ["- **Reproduction**:", "", *[f"  {ln}" for ln in str(f['reproduction']).splitlines()]]
        if f.get("remediation"):
            lines.append(f"- **Remediation**: {f['remediation']}")
        lines.append("")
    # Facts and sub-goals as context appendices.
    if report["facts"]:
        lines += ["## Established facts"]
        lines += [f"- {x.get('statement','')}" for x in report["facts"]]
        lines.append("")
    if report["subgoals"]:
        lines += ["## Sub-goals (coverage checklist)"]
        for g in report["subgoals"]:
            mark = "x" if g.get("status") == "resolved" else " "
            tail = f" — {g['outcome']}" if g.get("outcome") else ""
            lines.append(f"- [{mark}] {g.get('description','')}{tail}")
        lines.append("")
    return "\n".join(lines)


# ---------------------------------------------------------------------------- #
#  HTML (self-contained)
# ---------------------------------------------------------------------------- #
_SEV_COLOR = {
    "critical": "#b00020", "high": "#d9480f", "medium": "#b8860b",
    "low": "#2b6cb0", "info": "#555",
}


def _h(x: Any) -> str:
    return html.escape(str(x))


def render_html(report: dict[str, Any]) -> str:
    m = report["mission"]
    s = report["summary"]
    parts: list[str] = [
        "<!doctype html><html lang='en'><head><meta charset='utf-8'>",
        "<meta name='viewport' content='width=device-width, initial-scale=1'>",
        f"<title>Mission report — {_h(m.get('name','mission'))}</title>",
        "<style>",
        ":root{color-scheme:light dark}",
        "body{font:16px/1.6 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;"
        "max-width:60rem;margin:2rem auto;padding:0 1rem}",
        "h1{margin-bottom:.2rem}h3{margin:1.4rem 0 .2rem}",
        ".meta li{margin:.1rem 0}.sev{font-weight:700;padding:.05rem .4rem;border-radius:.3rem;color:#fff}",
        ".status{color:#666;font-style:italic;margin-left:.4rem}",
        "pre{background:#00000011;padding:.6rem;border-radius:.4rem;overflow:auto;white-space:pre-wrap}",
        ".card{border:1px solid #00000022;border-radius:.5rem;padding:.8rem 1rem;margin:.8rem 0}",
        "</style></head><body>",
        f"<h1>Mission report — {_h(m.get('name','mission'))}</h1>",
        f"<p>Generated {_h(report.get('generated_at',''))}</p>",
        "<ul class='meta'>",
    ]
    for k in ("mode", "goal", "scope", "models", "started", "ended", "duration_seconds", "cost_usd"):
        v = m.get(k)
        if v in (None, "", []):
            continue
        if isinstance(v, list):
            v = ", ".join(str(x) for x in v)
        parts.append(f"<li><b>{_h(k)}</b>: {_h(v)}</li>")
    parts += [
        "</ul>",
        "<h2>Summary</h2><ul>",
        f"<li>Findings: <b>{s['findings']}</b> (verified {s['findings_verified']}, "
        f"candidate {s['findings_candidate']})</li>",
        f"<li>Facts: {s['facts']}</li>",
        f"<li>Sub-goals: {s['subgoals_total']} ({s['subgoals_open']} open)</li>",
        "</ul>",
        "<h2>Findings</h2>",
    ]
    if not report["findings"]:
        parts.append("<p><em>No findings recorded.</em></p>")
    for f in report["findings"]:
        sev = f.get("severity", "info")
        color = _SEV_COLOR.get(sev, "#555")
        parts.append("<div class='card'>")
        parts.append(
            f"<h3><span class='sev' style='background:{color}'>{_h(sev.upper())}</span> "
            f"{_h(f.get('title','(untitled)'))}"
            f"<span class='status'>{_h(f.get('status','candidate'))}</span></h3>"
        )
        if f.get("target"):
            parts.append(f"<p><b>Target:</b> {_h(f['target'])}</p>")
        if f.get("category"):
            parts.append(f"<p><b>Category:</b> {_h(f['category'])}</p>")
        if f.get("evidence"):
            parts.append(f"<p><b>Evidence:</b></p><pre>{_h(f['evidence'])}</pre>")
        if f.get("reproduction"):
            parts.append(f"<p><b>Reproduction:</b></p><pre>{_h(f['reproduction'])}</pre>")
        if f.get("remediation"):
            parts.append(f"<p><b>Remediation:</b> {_h(f['remediation'])}</p>")
        parts.append("</div>")
    if report["facts"]:
        parts.append("<h2>Established facts</h2><ul>")
        parts += [f"<li>{_h(x.get('statement',''))}</li>" for x in report["facts"]]
        parts.append("</ul>")
    if report["subgoals"]:
        parts.append("<h2>Sub-goals (coverage checklist)</h2><ul>")
        for g in report["subgoals"]:
            mark = "☑" if g.get("status") == "resolved" else "☐"
            tail = f" — {_h(g['outcome'])}" if g.get("outcome") else ""
            parts.append(f"<li>{mark} {_h(g.get('description',''))}{tail}</li>")
        parts.append("</ul>")
    parts.append("</body></html>")
    return "\n".join(parts)


# ---------------------------------------------------------------------------- #
#  Write all three
# ---------------------------------------------------------------------------- #
def render_reports(
    blackboard: Blackboard,
    out_dir: str | Path,
    run_meta: dict[str, Any] | None = None,
) -> dict[str, str]:
    """Write report.json / report.md / report.html into out_dir. Returns the paths."""
    report = build_report(blackboard, run_meta)
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    paths = {
        "json": out / "report.json",
        "md": out / "report.md",
        "html": out / "report.html",
    }
    paths["json"].write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    paths["md"].write_text(render_markdown(report), encoding="utf-8")
    paths["html"].write_text(render_html(report), encoding="utf-8")
    return {k: str(v) for k, v in paths.items()}
