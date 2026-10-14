"""Engagement — a real-world assessment mission defined by an authorized scope.

An engagement is a real-world mission defined locally: instead of a platform
handing out isolated flag challenges, the operator writes one config file
describing

  - what the assessment is after (a diffuse goal, not a flag),
  - the authorized scope (hosts / IPs / CIDRs) — the HARD egress boundary, and
  - the rules of engagement (free-text constraints the operator must honor).

The scope is not optional and not advisory: an assessment engagement with an
empty scope fails to load (see `validate`). The agent's egress is then locked to
that scope by the same structural mechanism the platform target-lock uses —
there is no separate "trust me" path (docs/mission-modes.md §3).

The config is read-only input the operator authored; it carries no instructions
to the model beyond the goal/RoE text the operator wrote.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from backend.prompts import ChallengeMeta
from backend.scope import Scope


class EngagementError(ValueError):
    """Raised when an engagement config is missing or fails its scope check."""


@dataclass
class Engagement:
    """One assessment mission: goal + authorized scope + rules of engagement."""

    name: str
    goal: str = ""
    scope_targets: list[str] = field(default_factory=list)
    rules_of_engagement: str = ""
    mode: str = "assessment"
    category: str = "assessment"
    time_budget_seconds: int | None = None

    # ------------------------------------------------------------------ #
    @classmethod
    def load(cls, path: str | Path) -> Engagement:
        """Load from a YAML or JSON file and validate the scope invariant."""
        p = Path(path)
        if not p.exists():
            raise EngagementError(f"engagement config not found: {p}")
        text = p.read_text(encoding="utf-8")
        data: dict[str, Any]
        if p.suffix.lower() in (".json",):
            data = json.loads(text)
        else:
            import yaml

            data = yaml.safe_load(text) or {}
        if not isinstance(data, dict):
            raise EngagementError(f"engagement config must be a mapping: {p}")

        scope_raw = data.get("scope", {}) or {}
        # scope may be a list (targets only) or a mapping {targets, rules_of_engagement}.
        if isinstance(scope_raw, list):
            targets = list(scope_raw)
            roe = data.get("rules_of_engagement", "") or ""
        else:
            targets = list(scope_raw.get("targets", []) or [])
            roe = (
                scope_raw.get("rules_of_engagement")
                or data.get("rules_of_engagement")
                or ""
            )

        eng = cls(
            name=str(data.get("name") or p.stem),
            goal=str(data.get("goal", "") or ""),
            scope_targets=[str(t).strip() for t in targets if str(t).strip()],
            rules_of_engagement=str(roe or ""),
            mode=str(data.get("mode", "assessment") or "assessment"),
            category=str(data.get("category", "assessment") or "assessment"),
            time_budget_seconds=(
                int(data["time_budget_seconds"])
                if data.get("time_budget_seconds") is not None
                else None
            ),
        )
        eng.validate()
        return eng

    # ------------------------------------------------------------------ #
    def validate(self) -> None:
        """Enforce the authorization boundary: an assessment MUST declare a
        non-empty, parseable scope. This is the hard §3 invariant — refuse to
        run an assessment that has no explicit authorized targets."""
        if self.mode == "assessment":
            if not self.scope_targets:
                raise EngagementError(
                    f"engagement '{self.name}' is an assessment but declares no scope; "
                    "an assessment MUST list explicit authorized targets "
                    "(scope.targets: hosts / IPs / CIDRs). Refusing to run unscoped."
                )
            if self.scope().is_empty:
                raise EngagementError(
                    f"engagement '{self.name}' scope {self.scope_targets!r} parsed to nothing "
                    "usable; list valid hosts, IPs, or CIDRs."
                )

    def scope(self) -> Scope:
        return Scope.parse(self.scope_targets)

    def to_meta(self) -> ChallengeMeta:
        """Project the engagement onto the ChallengeMeta the swarm/solver consume.

        connection_info carries the authorized scope (hosts + CIDRs) so the
        existing structural egress lock applies unchanged; description carries
        the goal and rules of engagement.
        """
        desc_parts: list[str] = []
        if self.goal:
            desc_parts += ["## Assessment goal", self.goal.strip(), ""]
        if self.rules_of_engagement:
            desc_parts += ["## Rules of engagement", self.rules_of_engagement.strip(), ""]
        return ChallengeMeta(
            name=self.name,
            category=self.category,
            description="\n".join(desc_parts).strip(),
            connection_info=", ".join(self.scope_targets),
            flag_count=0,
        )


@dataclass(slots=True)
class LocalEngagementPlatform:
    """A no-oracle stand-in for CompetitionPlatform used by assessment runs.

    An engagement has no flag platform: findings live on the mission blackboard,
    not a submission endpoint. These methods exist only to satisfy the swarm's
    interface — submit_flag never confirms anything, and the discovery tools
    (fetch_*) return nothing because the engagement, not a platform, is the work
    source.
    """

    name: str = "engagement"

    async def fetch_challenge_stubs(self) -> list[dict[str, Any]]:
        return []

    async def fetch_all_challenges(self) -> list[dict[str, Any]]:
        return []

    async def fetch_solved_names(self) -> set[str]:
        return set()

    async def submit_flag(self, challenge_name: str, flag: str):  # noqa: ANN201
        from backend.platforms import SubmitResult

        return SubmitResult(
            status="not_applicable",
            message="assessment mode has no flag oracle",
            display=(
                "NOT APPLICABLE — this is an assessment engagement with no flag oracle. "
                "There is no flag to submit; record what you found with add_finding "
                "(candidate → verify_finding) on the blackboard instead."
            ),
        )

    async def pull_challenge(self, challenge: dict[str, Any], output_dir: str) -> str:
        return output_dir

    async def close(self) -> None:
        return None
