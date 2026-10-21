#!/usr/bin/env python3
"""Replay public calls, emitting numeric metrics and field paths, never content.
Run this script under the packet's heavy.lock wrapper (it takes no lock itself).
The official checker remains the acceptance gate.
"""
import argparse
import collections
import json
import math
import os
import subprocess
import time


def differences(a, b, path=""):
    if isinstance(a, dict) and isinstance(b, dict):
        if set(a) != set(b):
            return [path + ":keys"]
        return [d for k in a for d in differences(a[k], b[k], path + "." + k)]
    if isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            return [path + ":length"]
        return [d for x, y in zip(a, b) for d in differences(x, y, path + "[]")]
    if isinstance(a, float) or isinstance(b, float):
        if isinstance(a, (float, int)) and isinstance(b, (float, int)):
            if math.isclose(a, b, rel_tol=1e-9, abs_tol=1e-15):
                return []
        return [path + ":float"]
    return [] if a == b else [path + ":value"]


def main():
    p = argparse.ArgumentParser()
    for name in ("bin", "root", "store", "calls", "expected", "actual"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--ingest", action="store_true")
    a = p.parse_args()
    result = {}
    base = ["-root", a.root, "-store", a.store]
    if a.ingest:
        start = time.monotonic()
        proc = subprocess.Popen([a.bin, "ingest"] + base)
        _, status, usage = os.wait4(proc.pid, 0)
        proc.returncode = os.waitstatus_to_exitcode(status)
        if proc.returncode:
            raise SystemExit(proc.returncode)
        result["ingest_wall_s"] = time.monotonic() - start
        result["ingest_peak_bytes"] = usage.ru_maxrss
    calls = [json.loads(line) for line in open(a.calls)]
    expected = {row["id"]: row for row in map(json.loads, open(a.expected))}
    start = time.monotonic()
    proc = subprocess.Popen([a.bin, "batch"] + base, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, text=True)
    mismatch = []
    latencies = collections.defaultdict(list)
    matched = collections.Counter()
    with open(a.actual, "w") as actual:
        for i, call in enumerate(calls):
            proc.stdin.write(json.dumps(call) + "\n")
            proc.stdin.flush()
            line = proc.stdout.readline()
            if not line:
                raise RuntimeError("batch stopped before answering")
            actual.write(line)
            row = json.loads(line)
            if i == 0:
                result["startup_first_call_s"] = time.monotonic() - start
                result["batch_rss_bytes"] = int(subprocess.check_output(
                    ["ps", "-o", "rss=", "-p", str(proc.pid)])) * 1024
            latencies[call["tool"]].append(row["us"])
            exp = expected[call["id"]]
            ds = sorted(set(d for key in ("data", "text", "error")
                            for d in differences(exp[key], row[key], key)))
            if ds:
                mismatch.append({"id": call["id"], "tool": call["tool"], "fields": ds})
            else:
                matched[call["tool"]] += 1
    proc.stdin.close()
    _, status, usage = os.wait4(proc.pid, 0)
    proc.returncode = os.waitstatus_to_exitcode(status)
    result["batch_peak_bytes"] = usage.ru_maxrss
    result["store_bytes"] = sum(os.path.getsize(os.path.join(path, f))
                                for path, _, files in os.walk(a.store) for f in files)
    result["binary_bytes"] = os.path.getsize(a.bin)
    result["matched"] = dict(matched)
    result["calls"] = len(calls)
    result["latency_us"] = {}
    for tool, times in latencies.items():
        times.sort()
        result["latency_us"][tool] = {"p50": times[len(times)//2],
                                      "p99": times[min(len(times)-1, math.ceil(len(times)*.99)-1)]}
    result["mismatches"] = mismatch
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
