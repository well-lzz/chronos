"""Per-mission traffic log — real request/response pairs as finding evidence.

Every http_request / tcp_send exchange the solver makes is recorded here with a
short id (t1, t2, …). Findings then cite those ids as evidence with a role
(baseline / proof / verification / supporting), so a finding's proof is a real,
reproducible exchange rather than prose — the single most useful thing borrowed
from ARTEX's assessment model, adapted to chronos's one neutral tool surface
(no Skill/MCP, no capture agent).

The log is shared across executors on one mission (like the blackboard) and
appended to a JSONL file so it survives a restart. Bodies are already bounded by
net_tools' output cap, so entries stay small.
"""

from __future__ import annotations

import asyncio
import json
import re
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path

# Evidence roles a finding can assign to a referenced exchange.
ROLES = ("baseline", "proof", "verification", "supporting")
DEFAULT_ROLE = "supporting"

_MIN_SEARCH = 3  # minimum body_contains length, matching ARTEX


def _now() -> float:
    return time.time()


def _slug(s: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]+", "-", s).strip("-") or "mission"


@dataclass
class Exchange:
    id: str
    kind: str              # "http" | "tcp"
    target: str            # url (http) or host:port (tcp)
    method: str = ""       # http method; "TCP" for tcp
    status: int | None = None   # http status code when parsable
    request_text: str = ""      # reconstructed request (method/url/headers/body or payload)
    response_text: str = ""     # verbatim tool transcript of the response
    resp_len: int = 0
    found_by: str = ""
    ts: float = field(default_factory=_now)

    def index(self) -> dict:
        """Lightweight row for search results — no bodies."""
        return {
            "id": self.id, "kind": self.kind, "method": self.method,
            "target": self.target, "status": self.status, "resp_len": self.resp_len,
        }


_HTTP_STATUS_RE = re.compile(r"^HTTP (\d{3})")


@dataclass
class TrafficLog:
    """Shared, persisted request/response store for one mission."""

    mission_id: str
    path: str | None = None  # JSONL file; None = in-memory only

    exchanges: dict[str, Exchange] = field(default_factory=dict)
    _seq: int = 0
    _lock: asyncio.Lock = field(default_factory=asyncio.Lock, repr=False)

    def _next_id(self) -> str:
        self._seq += 1
        return f"t{self._seq}"

    # ------------------------------------------------------------------ #
    #  Recording (called from the solver's http_request / tcp_send tools)
    # ------------------------------------------------------------------ #
    async def record_http(self, method: str, url: str, headers: dict | None,
                          body: str, response_text: str, found_by: str = "") -> str:
        async with self._lock:
            tid = self._next_id()
            hdr = "\n".join(f"{k}: {v}" for k, v in (headers or {}).items())
            req = f"{method.upper()} {url}\n{hdr}".rstrip()
            if body:
                req += f"\n\n{body}"
            status = None
            m = _HTTP_STATUS_RE.match(response_text or "")
            if m:
                status = int(m.group(1))
            ex = Exchange(
                id=tid, kind="http", target=url[:600], method=method.upper(),
                status=status, request_text=req[:8000], response_text=(response_text or "")[:24000],
                resp_len=len(response_text or ""), found_by=found_by,
            )
            self.exchanges[tid] = ex
            self._append(ex)
            return tid

    async def record_tcp(self, host: str, port: int, payload: str, payload_hex: str,
                         response_text: str, found_by: str = "") -> str:
        async with self._lock:
            tid = self._next_id()
            req = f"TCP {host}:{port}\n" + (f"payload_hex={payload_hex}" if payload_hex else payload)
            ex = Exchange(
                id=tid, kind="tcp", target=f"{host}:{port}", method="TCP",
                request_text=req[:8000], response_text=(response_text or "")[:24000],
                resp_len=len(response_text or ""), found_by=found_by,
            )
            self.exchanges[tid] = ex
            self._append(ex)
            return tid

    # ------------------------------------------------------------------ #
    #  Reads (traffic_search / traffic_get tools)
    # ------------------------------------------------------------------ #
    def exists(self, tid: str) -> bool:
        return tid in self.exchanges

    def search(self, host: str = "", url_contains: str = "", body_contains: str = "",
               page: int = 0, per_page: int = 10) -> dict:
        host = (host or "").strip().lower()
        url_sub = (url_contains or "").strip().lower()
        body_sub = (body_contains or "").strip()
        if body_sub and len(body_sub) < _MIN_SEARCH:
            return {"error": f"body_contains needs at least {_MIN_SEARCH} characters"}
        hits: list[dict] = []
        for ex in self.exchanges.values():
            tgt = ex.target.lower()
            if host and host not in tgt:
                continue
            if url_sub and url_sub not in tgt:
                continue
            if body_sub and body_sub not in ex.request_text and body_sub not in ex.response_text:
                continue
            hits.append(ex.index())
        per_page = max(1, min(per_page, 10))
        start = max(0, page) * per_page
        window = hits[start:start + per_page]
        return {"total": len(hits), "page": max(0, page), "per_page": per_page, "items": window}

    def get(self, tid: str, side: str = "both", offset: int = 0, length: int = 8192) -> dict:
        ex = self.exchanges.get(tid)
        if not ex:
            return {"error": f"no exchange {tid}"}
        length = max(1, min(length, 8192))
        offset = max(0, offset)

        def _slice(text: str) -> str:
            return text[offset:offset + length]

        out: dict = {"id": ex.id, "kind": ex.kind, "target": ex.target,
                     "method": ex.method, "status": ex.status}
        if side in ("request", "both"):
            out["request"] = _slice(ex.request_text)
        if side in ("response", "both"):
            out["response"] = _slice(ex.response_text)
        return out

    # ------------------------------------------------------------------ #
    #  Persistence (JSONL append)
    # ------------------------------------------------------------------ #
    def _append(self, ex: Exchange) -> None:
        if not self.path:
            return
        try:
            p = Path(self.path)
            p.parent.mkdir(parents=True, exist_ok=True)
            with p.open("a", encoding="utf-8") as f:
                f.write(json.dumps(asdict(ex), ensure_ascii=False) + "\n")
        except Exception:  # noqa: BLE001 — persistence is best-effort, never fail a tool call
            pass

    @classmethod
    def load_or_new(cls, mission_id: str, state_dir: str = ".chronos-state") -> TrafficLog:
        path = str(Path(state_dir) / "traffic" / f"{_slug(mission_id)}.jsonl")
        log = cls(mission_id=mission_id, path=path)
        try:
            for line in Path(path).read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if not line:
                    continue
                d = json.loads(line)
                ex = Exchange(**d)
                log.exchanges[ex.id] = ex
                n = int(ex.id[1:]) if ex.id[1:].isdigit() else 0
                log._seq = max(log._seq, n)
        except FileNotFoundError:
            pass
        except Exception:  # noqa: BLE001 — corrupt/partial file: start fresh rather than crash
            pass
        return log
