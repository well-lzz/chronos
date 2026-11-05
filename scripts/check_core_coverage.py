#!/usr/bin/env python3
"""Fail CI if exercised core paths regress below the reviewed coverage floor."""
import subprocess
import sys

profile = sys.argv[1]
floors = {"solver": 65, "swarm": 60, "engrun": 70, "web": 40}
totals = {name: [0, 0] for name in floors}
with open(profile) as source:
    next(source)
    for line in source:
        block, statements, count = line.split()
        parts = block.split("/internal/")
        package = parts[-1].split("/")[0] if len(parts) == 2 else ""
        if package in totals:
            n = int(statements)
            totals[package][0] += n
            totals[package][1] += n if int(count) else 0
failed = False
for package, (total, covered) in totals.items():
    percent = 100 * covered / total if total else 0
    print("{}: {:.1f}% (minimum {}%)".format(package, percent, floors[package]))
    failed |= percent < floors[package]
critical = {("solver", "RunUntilDoneOrGaveUp"): 65,
            ("swarm", "runExecutor"): 50,
            ("swarm", "routeVerification"): 90,
            ("swarm", "UpdateScope"): 90,
            ("engrun", "UpdateScope"): 75,
            ("web", "protect"): 90}
output = subprocess.check_output(["go", "tool", "cover", "-func=" + profile], universal_newlines=True)
seen = set()
for line in output.splitlines():
    fields = line.split()
    if len(fields) != 3 or "/internal/" not in fields[0]:
        continue
    package = fields[0].split("/internal/")[1].split("/")[0]
    key = (package, fields[1])
    if key in critical:
        seen.add(key)
        percent = float(fields[2].rstrip("%"))
        print("{}.{}: {}".format(package, fields[1], fields[2]))
        failed |= percent < critical[key]
failed |= seen != set(critical)
sys.exit(1 if failed else 0)
