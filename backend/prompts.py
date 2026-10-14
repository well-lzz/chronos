"""System prompt builder + ChallengeMeta.

The prompt is a neutral task contract: objective, tool contract, submission
format, and hard constraints. It encodes NO attack methodology, no per-scenario
playbook, and no "first do X then Y" procedure — the model's own reasoning
decides how to search. (`_guess_scenario` survives only to pick a per-challenge
time budget and to gate the target lock, never to shape the prompt text.)
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml


@dataclass
class ChallengeMeta:
    name: str = "Unknown"
    category: str = ""
    value: int = 0
    description: str = ""
    tags: list[str] = field(default_factory=list)
    connection_info: str = ""
    hints: list[dict[str, Any]] = field(default_factory=list)
    solves: int = 0
    unique_code: str = ""
    attachments: list[dict[str, Any]] = field(default_factory=list)
    # Multi-flag / benchmark semantics (TSec)
    flag_count: int = 1
    correct_flag_count: int = 0
    level: int = 0
    container_status: str = "stopped"

    @classmethod
    def from_yaml(cls, path: str | Path) -> ChallengeMeta:
        with open(path, encoding="utf-8") as f:
            data = yaml.safe_load(f) or {}
        return cls(
            name=data.get("name", "Unknown"),
            category=data.get("category", ""),
            value=data.get("value", 0),
            description=data.get("description", ""),
            tags=data.get("tags", []),
            connection_info=data.get("connection_info", ""),
            hints=data.get("hints", []),
            solves=data.get("solves", 0),
            unique_code=data.get("unique_code", ""),
            attachments=data.get("attachments", []),
            flag_count=int(data.get("flag_count", 1) or 1),
            correct_flag_count=int(data.get("correct_flag_count", 0) or 0),
            level=int(data.get("level", 0) or 0),
            container_status=data.get("container_status", "stopped"),
        )


def _guess_scenario(meta: ChallengeMeta) -> str:
    """Infer the offensive-security dimension from code/name/category signals.

    A free-text keyword heuristic, used ONLY to pick a per-challenge time
    budget (killchain tasks get longer). It never shapes the prompt text and
    never affects the target lock, which is purely structural.
    """
    blob = f"{meta.unique_code} {meta.name} {meta.category} {meta.description}".lower()
    for key in ("killchain", "evasion", "exploit", "cloud", "binary", "web"):
        if key in blob:
            return key
    for word, key in (
        ("渗透", "killchain"), ("横向", "killchain"), ("内网", "killchain"), ("kill chain", "killchain"),
        ("穿透", "killchain"), ("多级", "killchain"), ("跳板", "killchain"),
        ("免杀", "evasion"), ("绕过", "evasion"), ("规避", "evasion"), ("waf", "evasion"), ("edr", "evasion"),
        ("rce", "exploit"), ("poc", "exploit"), ("利用", "exploit"), ("exp_", "exploit"),
        ("挖掘", "exploit"), ("审计", "exploit"), ("安全测试", "exploit"), ("cve", "exploit"),
        ("comfyui", "exploit"), ("gradio", "exploit"), ("ollama", "exploit"), ("dify", "exploit"),
        ("k8s", "cloud"), ("kubernetes", "cloud"), ("容器逃逸", "cloud"), ("iam", "cloud"), ("云", "cloud"),
        ("pwn", "binary"), ("reverse", "binary"), ("逆向", "binary"), ("二进制", "binary"),
        ("stack", "binary"), ("heap", "binary"), ("overflow", "binary"), ("bof", "binary"),
        ("注入", "web"), ("sql", "web"), ("xss", "web"),
    ):
        if word in blob:
            return key
    return "web"


def build_headless_prompt(
    meta: ChallengeMeta,
    *,
    remaining_seconds: int | None = None,
    allow_exec: bool = True,
) -> str:
    """Build the solver's system prompt: one neutral task contract.

    The search is framed around FINDINGS recorded on the shared blackboard. If
    the target happens to carry a flag (a verifiable terminal artifact), it is
    submitted with submit_flag — a flag is just a finding the platform can
    confirm, not a separate mode. No attack methodology, no per-scenario
    playbook, no procedure, no termination rule baked in here — how to search,
    and when it is done, are decided elsewhere (the model, and the termination
    hook / budget).
    """
    addrs = [a.strip() for a in re.split(r"[,\s]+", meta.connection_info) if a.strip()]
    has_flags = max(0, meta.flag_count) >= 1 or bool(meta.correct_flag_count)

    lines: list[str] = [
        "You are an autonomous security AI working one authorized mission.",
        "You have no local toolchain beyond the tools listed below; your strengths are",
        "reasoning, protocol comprehension, and precise payload construction.",
        "",
        "## Objective",
        "This mission is a SEARCH. Explore the authorized scope and record what you find —",
        "Facts (confirmed world state) and Findings (products worth reporting; for a security",
        "assessment, vulnerabilities) — on the shared working record as you go. Record",
        "candidates too and verify them. Keep covering new ground; other executors share your",
        "record and build on it.",
        "",
        "## Mission",
        f"**Name**       : {meta.name}",
    ]
    if meta.unique_code:
        lines.append(f"**Unique Code**: {meta.unique_code}")
    if meta.category:
        lines.append(f"**Type**       : {meta.category}")
    if meta.value:
        lines.append(f"**Score**      : {meta.value}")
    if has_flags and (max(1, meta.flag_count) > 1 or meta.correct_flag_count):
        lines.append(
            f"**Flags**      : {meta.correct_flag_count}/{max(1, meta.flag_count)} already accepted."
        )
    if remaining_seconds is not None:
        lines.append(f"**Time budget**: ~{max(0, remaining_seconds) // 60} minutes left.")
    lines += ["", "## Description", meta.description or "_No description provided._", ""]

    lines += ["## Authorized scope"]
    if addrs:
        lines += [
            f"In scope: {', '.join(addrs)}",
            "These hosts/ranges are the ONLY authorized targets. A scope lock is enforced:",
            "http_request, tcp_send, and run_python socket connections to anything outside",
            "this scope are blocked, with no bypass. Touching out-of-scope systems is",
            "unauthorized and any result found there is invalid — stay strictly within scope.",
        ]
    else:
        lines += [
            "No target was provided. Without an authorized target you cannot proceed — report",
            "the missing scope via notify_coordinator and do not probe anything.",
        ]

    lines += [
        "",
        "## Working record (shared blackboard)",
        "Maintain an explicit record as you search — shared with other executors and kept",
        "beyond your context window:",
        "- `add_fact(statement, evidence)`: a confirmed fact (service/version, endpoint,",
        "  credential, access gained).",
        "- `add_finding(title, severity, target, category, evidence, reproduction, verified)`:",
        "  record a finding; record CANDIDATES too (verified=false).",
        "- `verify_finding(finding_id, verified, evidence)`: promote a candidate to verified",
        "  after you reproduce it, or mark it refuted; attach the evidence.",
        "- `add_subgoal(description)` / `resolve_subgoal(id, outcome)`: your own live coverage",
        "  checklist.",
        "",
        "## Tools",
        "- `http_request(url, method, headers, body)`: full-control HTTP within scope.",
        "- `tcp_send(host, port, payload, payload_hex)`: one raw TCP exchange (line or binary).",
    ]
    if allow_exec:
        lines.append(
            "- `run_python(code)`: local CPython scratchpad (stdlib plus installed libraries). "
            "print() what you need. Its sockets are scope-locked too."
        )
    if has_flags:
        lines += [
            "- `submit_flag(flag)`: if the target yields a flag (a verifiable terminal artifact),",
            "  submit it verbatim; it is recorded as a verified finding.",
            "- `request_hint()`: fetch the platform hint (DEDUCTS score, once per task) — only",
            "  when your own ideas are exhausted.",
        ]
    lines += [
        "- `task_status()` / `check_findings()` / `notify_coordinator(msg)`: progress + time,",
        "  read sibling findings, report upward.",
        "",
        "## Constraints",
        "- Act ONLY within the authorized scope above.",
        "- Record findings from what you actually observed; never fabricate or infer one you",
        "  could not reproduce. Distinguish candidates from verified findings honestly.",
        "- The local disk is shared and untrusted: something found in a local file may belong",
        "  to a different mission — only results from this target's own responses count.",
    ]
    if has_flags:
        lines += [
            "- Submit flags verbatim as observed in the target output; never guess or derive one",
            "  from the title/description. Never resubmit a rejected flag.",
        ]
    lines += [
        "- Do not chase out-of-scope systems even if an in-scope target points at them, unless",
        "  that pivot is itself inside the authorized scope.",
    ]

    return "\n".join(lines)
