"""Engagement scope — the single, structural authorization boundary.

Every mission (a CTF range or a real engagement) runs inside an explicit scope:
the set of hosts and IP ranges the agent is authorized to touch. The solver's
own egress (http_request / tcp_send / run_python sockets) is restricted to this
set with no bypass — exactly the old CTF "target lock", generalized from a flat
host list to host + CIDR so a real engagement's authorized ranges fit too.

This module is the ONE definition of "is this host in scope". Both the parent
process (http_request/tcp_send) and the run_python subprocess guard derive their
decision from here, so there is no second, divergent matcher to drift.

A scope is NEVER widened by guesswork: an unparsable or empty scope yields an
empty Scope, which allows nothing (callers treat empty as the fail-loud
"no target known" state). Hostname entries match by exact (lowercased) string;
IP/CIDR entries match numeric IPs. No DNS resolution happens in the guard —
resolving there would be unreliable and a rebinding risk.
"""

from __future__ import annotations

import ipaddress
import re
from dataclasses import dataclass


def _looks_like_ip(s: str) -> bool:
    try:
        ipaddress.ip_address(s)
        return True
    except ValueError:
        return False


def _normalize_host(token: str) -> str:
    """Reduce a 'host[:port][/path]' / 'scheme://host...' token to a bare host."""
    t = re.sub(r"^\w+://", "", token.strip())
    t = t.split("/", 1)[0]  # drop any path (CIDRs are handled before this is called)
    t = t.strip("[]")  # bracketed IPv6
    # Strip a trailing :port only for IPv4/hostnames (a single colon) or when the
    # value is plainly not a bare IPv6 address.
    if ":" in t and t.count(":") == 1:
        t = t.rsplit(":", 1)[0]
    return t.lower()


@dataclass(frozen=True)
class Scope:
    """An immutable set of authorized hosts + IP networks."""

    hosts: frozenset[str] = frozenset()
    networks: tuple[ipaddress.IPv4Network | ipaddress.IPv6Network, ...] = ()
    raw: tuple[str, ...] = ()

    @property
    def is_empty(self) -> bool:
        return not self.hosts and not self.networks

    def allows(self, host: str) -> bool:
        """True iff host is authorized. Empty scope allows nothing."""
        h = (host or "").strip().strip("[]").lower()
        if not h:
            return False
        if h in self.hosts:
            return True
        if _looks_like_ip(h) and self.networks:
            ip = ipaddress.ip_address(h)
            return any(ip in net for net in self.networks)
        return False

    def describe(self) -> str:
        """Human-readable target list for block messages / logs."""
        return ", ".join(self.raw) or "(none)"

    def cidr_strings(self) -> list[str]:
        """CIDR networks as strings, for the run_python subprocess guard."""
        return [str(n) for n in self.networks]

    @classmethod
    def parse(cls, source: str | list[str] | None) -> Scope:
        """Build a Scope from connection_info ("ip:port, ip:port") or a list of
        scope entries (hosts, IPs, CIDRs). Unparsable tokens are skipped."""
        if source is None:
            tokens: list[str] = []
        elif isinstance(source, str):
            tokens = re.split(r"[,\s]+", source)
        else:
            tokens = [str(x) for x in source]

        hosts: set[str] = set()
        networks: list[ipaddress.IPv4Network | ipaddress.IPv6Network] = []
        raw: list[str] = []
        for tok in tokens:
            tok = tok.strip()
            if not tok:
                continue
            body = re.sub(r"^\w+://", "", tok)
            # CIDR? (left side is an IP, right side an integer prefix)
            if "/" in body:
                left, right = body.split("/", 1)
                if right.isdigit() and _looks_like_ip(left):
                    try:
                        networks.append(ipaddress.ip_network(body, strict=False))
                        raw.append(tok)
                        continue
                    except ValueError:
                        pass
            host = _normalize_host(tok)
            if host:
                hosts.add(host)
                raw.append(tok)
        # De-dup raw while preserving order.
        seen: set[str] = set()
        raw_unique = [r for r in raw if not (r in seen or seen.add(r))]
        return cls(hosts=frozenset(hosts), networks=tuple(networks), raw=tuple(raw_unique))
