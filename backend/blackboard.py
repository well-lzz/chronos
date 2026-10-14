"""Mission blackboard — the explicit shared search state.

A per-mission store of Facts (confirmed world state), Findings (products
discovered during the search — e.g. vulnerabilities), and Sub-Goals (the live,
self-generated coverage checklist). Executors read it before each Step and
write new Facts/Findings/Sub-Goals back through the solver tools; it is injected
into the model's context so its memory survives beyond the context window and
is shared across executors on the same mission.

Persisted to disk on every mutation so a long assessment run does not lose
discoveries on a crash/restart (see docs/mission-modes.md §7.5).
"""

from __future__ import annotations

import asyncio
import json
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path

# Finding lifecycle
CANDIDATE = "candidate"
VERIFIED = "verified"
REFUTED = "refuted"

SEVERITIES = ("info", "low", "medium", "high", "critical")


def _now() -> float:
    return time.time()


def _slug(s: str) -> str:
    import re
    return re.sub(r"[^A-Za-z0-9._-]+", "-", s).strip("-") or "mission"


@dataclass
class Fact:
    id: str
    statement: str
    evidence: str = ""
    found_by: str = ""
    ts: float = field(default_factory=_now)


@dataclass
class Finding:
    id: str
    title: str
    target: str = ""
    category: str = ""
    severity: str = "info"
    status: str = CANDIDATE  # candidate | verified | refuted
    evidence: str = ""
    reproduction: str = ""
    remediation: str = ""
    found_by: str = ""
    ts: float = field(default_factory=_now)
    verified_ts: float | None = None


@dataclass
class SubGoal:
    id: str
    description: str
    status: str = "open"  # open | resolved
    outcome: str = ""
    created_by: str = ""
    ts: float = field(default_factory=_now)


@dataclass
class Blackboard:
    """Shared Fact/Finding/Sub-Goal store for one mission."""

    mission_id: str
    path: str | None = None  # JSON file for persistence; None = in-memory only

    facts: dict[str, Fact] = field(default_factory=dict)
    findings: dict[str, Finding] = field(default_factory=dict)
    subgoals: dict[str, SubGoal] = field(default_factory=dict)
    _seq: int = 0
    _lock: asyncio.Lock = field(default_factory=asyncio.Lock, repr=False)

    def _next_id(self, prefix: str) -> str:
        self._seq += 1
        return f"{prefix}{self._seq}"

    # ------------------------------------------------------------------ #
    #  Mutations (each persists)
    # ------------------------------------------------------------------ #
    async def add_fact(self, statement: str, evidence: str = "", found_by: str = "") -> str:
        async with self._lock:
            fid = self._next_id("f")
            self.facts[fid] = Fact(id=fid, statement=statement[:2000],
                                   evidence=evidence[:2000], found_by=found_by)
            self._persist()
            return fid

    async def add_finding(self, title: str, *, target: str = "", category: str = "",
                          severity: str = "info", status: str = CANDIDATE, evidence: str = "",
                          reproduction: str = "", remediation: str = "", found_by: str = "") -> str:
        async with self._lock:
            sev = severity if severity in SEVERITIES else "info"
            st = status if status in (CANDIDATE, VERIFIED, REFUTED) else CANDIDATE
            vid = self._next_id("v")
            self.findings[vid] = Finding(
                id=vid, title=title[:300], target=target[:300], category=category[:80],
                severity=sev, status=st, evidence=evidence[:4000],
                reproduction=reproduction[:4000], remediation=remediation[:2000],
                found_by=found_by, verified_ts=_now() if st == VERIFIED else None,
            )
            self._persist()
            return vid

    async def update_finding(self, finding_id: str, *, status: str | None = None,
                             evidence: str | None = None, severity: str | None = None,
                             reproduction: str | None = None, remediation: str | None = None) -> bool:
        async with self._lock:
            f = self.findings.get(finding_id)
            if not f:
                return False
            if status in (CANDIDATE, VERIFIED, REFUTED):
                f.status = status
                if status == VERIFIED:
                    f.verified_ts = _now()
            if severity in SEVERITIES:
                f.severity = severity
            if evidence is not None:
                f.evidence = evidence[:4000]
            if reproduction is not None:
                f.reproduction = reproduction[:4000]
            if remediation is not None:
                f.remediation = remediation[:2000]
            self._persist()
            return True

    async def add_subgoal(self, description: str, created_by: str = "") -> str:
        async with self._lock:
            gid = self._next_id("g")
            self.subgoals[gid] = SubGoal(id=gid, description=description[:500], created_by=created_by)
            self._persist()
            return gid

    async def resolve_subgoal(self, subgoal_id: str, outcome: str = "") -> bool:
        async with self._lock:
            g = self.subgoals.get(subgoal_id)
            if not g:
                return False
            g.status = "resolved"
            g.outcome = outcome[:1000]
            self._persist()
            return True

    # ------------------------------------------------------------------ #
    #  Reads / signals
    # ------------------------------------------------------------------ #
    def open_subgoals(self) -> list[SubGoal]:
        return [g for g in self.subgoals.values() if g.status == "open"]

    def verified_findings(self) -> list[Finding]:
        return [f for f in self.findings.values() if f.status == VERIFIED]

    def progress_marker(self) -> int:
        """A monotone discovery counter for the diminishing-returns signal.

        Rises on every new Fact, new Finding, and candidate→verified promotion,
        so two consecutive executor cycles with an unchanged marker means the
        search produced no new knowledge. Pure bookkeeping (sub-goal churn,
        refutations) deliberately does not move it.
        """
        return (
            len(self.facts)
            + len(self.findings)
            + sum(1 for f in self.findings.values() if f.status == VERIFIED)
        )

    def counts(self) -> dict[str, int]:
        return {
            "facts": len(self.facts),
            "findings": len(self.findings),
            "findings_verified": sum(1 for f in self.findings.values() if f.status == VERIFIED),
            "findings_candidate": sum(1 for f in self.findings.values() if f.status == CANDIDATE),
            "subgoals_open": len(self.open_subgoals()),
            "subgoals_total": len(self.subgoals),
        }

    def summary(self, max_facts: int = 25, max_findings: int = 25) -> str:
        """Compact text view injected into an executor's context each Step."""
        if not (self.facts or self.findings or self.subgoals):
            return ""
        lines: list[str] = ["## 工作记录 (blackboard)"]
        open_g = self.open_subgoals()
        if open_g:
            lines.append("未决 Sub-Goal:")
            lines += [f"- [{g.id}] {g.description}" for g in open_g[:20]]
        if self.findings:
            lines.append("Findings:")
            for f in list(self.findings.values())[-max_findings:]:
                lines.append(f"- [{f.id}] ({f.status}/{f.severity}) {f.title}"
                             + (f" @ {f.target}" if f.target else ""))
        if self.facts:
            recent = list(self.facts.values())[-max_facts:]
            lines.append("已确认 Facts:")
            lines += [f"- {x.statement}" for x in recent]
        return "\n".join(lines)

    # ------------------------------------------------------------------ #
    #  Persistence
    # ------------------------------------------------------------------ #
    def to_dict(self) -> dict:
        return {
            "mission_id": self.mission_id,
            "seq": self._seq,
            "facts": [asdict(x) for x in self.facts.values()],
            "findings": [asdict(x) for x in self.findings.values()],
            "subgoals": [asdict(x) for x in self.subgoals.values()],
        }

    def _persist(self) -> None:
        if not self.path:
            return
        try:
            p = Path(self.path)
            p.parent.mkdir(parents=True, exist_ok=True)
            tmp = p.with_suffix(p.suffix + ".tmp")
            tmp.write_text(json.dumps(self.to_dict(), ensure_ascii=False, indent=2), encoding="utf-8")
            tmp.replace(p)
        except Exception:  # noqa: BLE001 — persistence is best-effort, never fail a Step
            pass

    @classmethod
    def load_or_new(cls, mission_id: str, state_dir: str = ".ctf-state") -> Blackboard:
        path = str(Path(state_dir) / "blackboards" / f"{_slug(mission_id)}.json")
        bb = cls(mission_id=mission_id, path=path)
        try:
            data = json.loads(Path(path).read_text(encoding="utf-8"))
            bb._seq = int(data.get("seq", 0))
            for d in data.get("facts", []):
                bb.facts[d["id"]] = Fact(**d)
            for d in data.get("findings", []):
                bb.findings[d["id"]] = Finding(**d)
            for d in data.get("subgoals", []):
                bb.subgoals[d["id"]] = SubGoal(**d)
        except FileNotFoundError:
            pass
        except Exception:  # noqa: BLE001 — corrupt/partial file: start fresh rather than crash
            pass
        return bb
