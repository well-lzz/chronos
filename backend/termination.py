"""Mission termination — the single, user-defined stop condition.

The search has no built-in "done" signal anymore: an executor keeps searching and
recording Findings until this predicate says stop, or a hard safety bound (max
bumps / wall-clock budget) is hit. Define your own termination policy here —
coverage, diminishing returns, a finding-count threshold, a confirmed objective,
whatever fits the mission.

The default never terminates early, so out of the box only the safety bounds
apply. Keep it pure and cheap: it is called once per executor cycle.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from backend.blackboard import Blackboard


@dataclass(frozen=True)
class CycleState:
    """What the swarm knows at the end of one executor cycle."""

    step_count: int          # steps taken by this executor so far
    bump_count: int          # how many times it has been bumped/resumed
    cycles: int              # executor cycles completed on this mission
    progress_marker: int     # blackboard discovery counter this cycle
    prev_progress_marker: int  # the same counter at the start of the cycle
    elapsed_seconds: float   # wall-clock since the executor started

    @property
    def made_progress(self) -> bool:
        return self.progress_marker > self.prev_progress_marker


def should_terminate(blackboard: Blackboard | None, state: CycleState) -> bool:
    """Return True to stop this executor. DEFINE YOUR POLICY HERE.

    Default: never stop early — let the safety bounds (max bumps, wall-clock
    budget) end the run. Examples you might implement:

        # diminishing returns: N quiet cycles with nothing left to chase
        # if not state.made_progress and blackboard and not blackboard.open_subgoals():
        #     ...track consecutive quiet cycles and stop after N...

        # enough verified findings:
        # if blackboard and len(blackboard.verified_findings()) >= 10:
        #     return True
    """
    return False
