#!/usr/bin/env python3
"""End-to-end run of the real agent-quota-exporter binary against mocked Claude Code homes.

Writes transcripts the way Claude Code does (assistant lines with usage, streamed duplicates of one message id,
subagent files, a second account), serves the exporter, scrapes it repeatedly, restarts it once, and checks the
exported counters against ground truth computed independently here. Everything lives under BASE; every process
this script starts is stopped before it exits.
"""
import json
import os
import random
import re
import signal
import subprocess
import sys
import time
import urllib.request

BIN, BASE = sys.argv[1], sys.argv[2]
if os.path.exists(BASE) and os.listdir(BASE):  # a previous run's state would not match this run's ground truth
    raise SystemExit(f"{BASE} is not empty; give a fresh directory")
PORT = 19878
HOST = "Ubuntu04-10-98-10-13"
random.seed(7)
NOW_MS = int(time.time() * 1000)

HOMES = {  # profile alias -> (config dir, .claude.json path, account uuid)
    "root": (f"{BASE}/root/.claude", f"{BASE}/root/.claude.json", "11111111-1111-4111-8111-111111111111"),
    "claude-work": (f"{BASE}/home/acct-b/.claude-work", f"{BASE}/home/acct-b/.claude-work/.claude.json",
                    "22222222-2222-4222-8222-222222222222"),
}
truth = {p: {} for p in HOMES}  # profile -> msg id -> [in, out, cr, cw, model]
procs = []
mine = {}  # every file this script wrote under a profile tree -> (size, mtime_ns) right after its last write


def write(path, data, mode="w"):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, mode, encoding="utf-8") as f:
        f.write(data)
    if path.startswith((f"{BASE}/root/", f"{BASE}/home/")):
        st = os.stat(path)
        mine[path] = (st.st_size, st.st_mtime_ns)


def line(mid, model, u, ts=None):
    ts = ts or time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime())
    return json.dumps({"type": "assistant", "timestamp": ts, "sessionId": "s", "cwd": "/root",
                       "message": {"id": mid, "model": model, "role": "assistant",
                                   "content": [{"type": "text", "text": "PROMPT-CONTENT-CANARY"}],
                                   "usage": {"input_tokens": u[0], "output_tokens": u[1],
                                             "cache_read_input_tokens": u[2],
                                             "cache_creation_input_tokens": u[3]}}}) + "\n"


def setup():
    for alias, (d, cj, uuid) in HOMES.items():
        write(cj, json.dumps({"oauthAccount": {"accountUuid": uuid, "emailAddress": f"{alias}@example.com",
                                               "displayName": "PROFILE-CONTENT-CANARY"}}))
        write(f"{d}/.credentials.json", json.dumps({"claudeAiOauth": {
            "accessToken": "TOKEN-CANARY-" + "x" * 40, "refreshToken": "REFRESH-CANARY",
            "expiresAt": NOW_MS + 8 * 3600 * 1000, "subscriptionType": "max"}}))
        # history present before the exporter first starts: must never be counted
        write(f"{d}/projects/-root-old/old.jsonl", "".join(line(f"old-{alias}-{i}", "model-old", [9, 9, 9, 9],
                                                               "2026-10-01T00:00:00.000Z") for i in range(50)))
    # live sessions: real processes the exporter can see in /proc
    for alias, (d, _, _) in HOMES.items():
        for status in ("busy", "idle"):
            p = subprocess.Popen(["sleep", "600"])
            procs.append(p)
            write(f"{d}/sessions/{p.pid}.json", json.dumps({"pid": p.pid, "status": status, "cwd": "/root",
                                                             "name": "SESSION-NAME-CANARY"}))
        write(f"{d}/sessions/999999.json", json.dumps({"pid": 999999, "status": "busy"}))  # dead pid
    write(f"{BASE}/config.yaml", f"""version: 1
listen: "127.0.0.1:{PORT}"
host: {HOST}
state_dir: {BASE}/state
discovery:
  mode: only
  roots: ["{BASE}/root", "{BASE}/home/*"]
defaults:
  usage_interval: 30s
  stale_after: 45m
""")


def emit(alias, n):
    """Append n new messages for a profile across files, some streamed twice (second copy has more output)."""
    d = HOMES[alias][0]
    for _ in range(n):
        mid = f"msg_{alias}_{len(truth[alias])}"
        model = random.choice(["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"])
        u = [random.randint(1, 50), random.randint(1, 900), random.randint(0, 50000), random.randint(0, 3000)]
        f = random.choice([f"{d}/projects/-root-app/sess-a.jsonl", f"{d}/projects/-root-app/sess-a/subagents/agent-1.jsonl",
                           f"{d}/projects/-root-lib/sess-b.jsonl"])
        if random.random() < 0.3:  # streamed: first a partial, then the final line with higher output
            partial = [u[0], u[1] // 3, u[2], u[3]]
            write(f, line(mid, model, partial), "a")
        write(f, line(mid, model, u), "a")
        truth[alias][mid] = u + [model]


def start():
    log = open(f"{BASE}/exporter.log", "a")
    p = subprocess.Popen([BIN, "--config", f"{BASE}/config.yaml"], stdout=log, stderr=log)
    procs.append(p)
    for _ in range(50):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{PORT}/healthz", timeout=1)
            return p
        except OSError:
            time.sleep(0.2)
    raise SystemExit("exporter did not come up")


def scrape():
    return urllib.request.urlopen(f"http://127.0.0.1:{PORT}/metrics", timeout=5).read().decode()


def totals(text):
    out = {}
    for m in re.finditer(r'^agent_tokens_total\{([^}]*)\} (\S+)$', text, re.M):
        lab = dict(re.findall(r'(\w+)="([^"]*)"', m.group(1)))
        out[(lab["profile"], lab["model"], lab["type"])] = float(m.group(2))
    for m in re.finditer(r'^agent_requests_total\{([^}]*)\} (\S+)$', text, re.M):
        lab = dict(re.findall(r'(\w+)="([^"]*)"', m.group(1)))
        out[(lab["profile"], lab["model"], "requests")] = float(m.group(2))
    return out


def expected():
    out = {}
    for alias, msgs in truth.items():
        for u in msgs.values():
            for i, typ in enumerate(("input", "output", "cache_read", "cache_write")):
                k = (alias, u[4], typ)
                out[k] = out.get(k, 0) + u[i]
            k = (alias, u[4], "requests")
            out[k] = out.get(k, 0) + 1
    return out


def check_trees(label):
    """Every file under the profile homes (config dirs and the sibling .claude.json) must be exactly as this script
    last wrote it: the exporter may not create, change or touch anything there at any point of the run."""
    seen = {}
    for top in (f"{BASE}/root", f"{BASE}/home"):
        for root, _, files in os.walk(top):
            for fn in files:
                p = os.path.join(root, fn)
                st = os.stat(p)
                seen[p] = (st.st_size, st.st_mtime_ns)
    if seen != mine:
        diff = sorted(p for p in set(seen) | set(mine) if seen.get(p) != mine.get(p))
        raise SystemExit(f"  {label}: the exporter changed files under the profile homes: {diff[:8]}")
    print(f"  {label}: {len(seen)} profile files exactly as the test wrote them")


def wait_converged(label, timeout=75):
    want = expected()
    deadline = time.time() + timeout
    last = None
    history = []
    while time.time() < deadline:
        got = totals(scrape())
        history.append(got)
        if got == want:
            print(f"  {label}: converged, {len(want)} series equal ground truth")
            return history
        last = got
        time.sleep(3)
    diff = {k: (last.get(k), want.get(k)) for k in set(last) | set(want) if last.get(k) != want.get(k)}
    raise SystemExit(f"  {label}: NOT converged; differences (got, want): {dict(list(diff.items())[:8])}")


def monotonic(histories):
    prev = {}
    for h in histories:
        for k, v in h.items():
            if v < prev.get(k, 0):
                raise SystemExit(f"counter went backwards: {k} {prev[k]} -> {v}")
            prev[k] = v
    print(f"  counters monotonic across {len(histories)} scrapes")


try:
    setup()
    p = start()
    time.sleep(2)
    first = scrape()
    assert "model-old" not in first, "pre-existing history was replayed"
    print("phase 1: first start, history present -> not replayed")
    emit("root", 40)
    emit("claude-work", 25)
    h1 = wait_converged("phase 1")
    check_trees("phase 1")

    print("phase 2: restart mid-stream (state kept)")
    p.send_signal(signal.SIGTERM)
    p.wait(timeout=10)
    emit("root", 30)  # written while the exporter is down
    p = start()
    emit("claude-work", 20)
    h2 = wait_converged("phase 2")
    check_trees("phase 2")
    monotonic(h1 + h2)

    text = scrape()
    write(f"{BASE}/final-metrics.txt", text)
    for canary in ("CANARY",):
        assert canary not in text, "content/token canary leaked into /metrics"
        assert canary not in open(f"{BASE}/exporter.log").read(), "canary leaked into the log"
    print("  no content or token canary in /metrics or the log")
    for alias in HOMES:
        m = re.search(rf'^agent_sessions\{{[^}}]*profile="{alias}"[^}}]*state="active"[^}}]*\}} (\S+)$', text, re.M)
        i = re.search(rf'^agent_sessions\{{[^}}]*profile="{alias}"[^}}]*state="idle"[^}}]*\}} (\S+)$', text, re.M)
        assert m and i and float(m.group(1)) == 1 and float(i.group(1)) == 1, f"sessions for {alias}: {m and m.group(1)}, {i and i.group(1)}"
        li = re.search(rf'^agent_account_logged_in\{{[^}}]*profile="{alias}"[^}}]*\}} (\S+)$', text, re.M)
        assert li and float(li.group(1)) == 1, f"logged_in for {alias}"
    print("  sessions: 1 active + 1 idle per profile (dead pid ignored); both profiles logged in")
    accts = set(re.findall(r'account="([^"]+)"', text))
    assert len(accts) == 2 and all(a.startswith("acct-") for a in accts), accts
    print(f"  two distinct hashed account labels: {sorted(accts)}")
    time.sleep(35)  # one more transcripts cycle with nothing new
    check_trees("idle cycle")
    print("ALL CHECKS PASSED")
finally:
    for p in procs:
        if p.poll() is None:
            p.terminate()
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
