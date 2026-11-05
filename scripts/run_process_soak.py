#!/usr/bin/env python3
"""Durably run a bounded real-model soak; preserve binary, result and exit code."""
import argparse
import datetime
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time

parser = argparse.ArgumentParser()
parser.add_argument("--binary", required=True)
parser.add_argument("--test-binary", required=True)
parser.add_argument("--profile-dir", required=True)
parser.add_argument("--output", required=True)
parser.add_argument("--seconds", type=int, default=7200)
parser.add_argument("--interval", type=int, default=600)
args = parser.parse_args()
if not 1 <= args.seconds <= 10800 or not 1 <= args.interval <= 1800:
    parser.error("duration or interval outside the supported bound")
root = Path(args.output).resolve()
root.mkdir(mode=0o700, parents=True, exist_ok=False)
os.chmod(str(root), 0o700)
for source, name in [(args.binary, "chronos"), (args.test_binary, "soak.test")]:
    shutil.copy2(source, str(root / name))
    os.chmod(str(root / name), 0o700)

def save(record):
    temporary = root / ".runner.tmp"
    descriptor = os.open(str(temporary), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        json.dump(record, stream, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(str(temporary), str(root / "runner.json"))

env = dict(os.environ)
env.update({"CHRONOS_RUN_PROCESS_SOAK": "1", "CHRONOS_SOAK_SECONDS": str(args.seconds),
            "CHRONOS_SOAK_INTERVAL_SECONDS": str(args.interval),
            "CHRONOS_SOAK_BINARY": str(root / "chronos"),
            "CHRONOS_LIVE_PROFILE_DIR": str(Path(args.profile_dir).resolve()),
            "CHRONOS_LIVE_OUTPUT": str(root / "evidence")})
record = {"runner_pid": os.getpid(), "started": datetime.datetime.now(datetime.timezone.utc).isoformat(),
          "required_seconds": args.seconds, "interval_seconds": args.interval, "finished": False,
          "result": str(root / "evidence" / "soak-result.json")}
interrupted = False
process = None
awake = None

def stop(signum, frame):
    global interrupted
    interrupted = True
    if process is not None and process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)

signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
started = time.monotonic()
try:
    descriptor = os.open(str(root / "test.log"), os.O_WRONLY | os.O_CREAT, 0o600)
    with os.fdopen(descriptor, "w") as log:
        process = subprocess.Popen([str(root / "soak.test"), "-test.v",
                                    "-test.run=^TestProductionProcessSoak$",
                                    "-test.timeout=" + str(args.seconds + 300) + "s"],
                                   cwd=str(root), env=env, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        record["test_pid"] = process.pid
        save(record)
        if sys.platform == "darwin" and shutil.which("caffeinate"):
            awake = subprocess.Popen(["caffeinate", "-i", "-w", str(process.pid)],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        code = process.wait()
    record.update({"finished": True, "exit_code": code, "interrupted": interrupted,
                   "elapsed_seconds": time.monotonic() - started,
                   "ended": datetime.datetime.now(datetime.timezone.utc).isoformat()})
    result_file = root / "evidence" / "soak-result.json"
    result = json.loads(result_file.read_text()) if result_file.exists() else {}
    record["passed"] = code == 0 and result.get("passed") is True and not interrupted
    save(record)
finally:
    if awake is not None:
        awake.terminate()
