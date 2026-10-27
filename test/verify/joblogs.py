#!/usr/bin/env python3
"""Check the console's step figures against the CI jobs' own logs.

Every CI job that builds with `buildctl --progress plain` prints each
step as BuildKit's client saw it: "#N [stage 2/4] RUN ...", then
"#N CACHED" or "#N DONE 1.2s" (or "#N ERROR"). The console reads the same
builds from the daemons' history instead. This script fetches a
pipeline's jobs and logs from GitLab and the console's build of each job
from the controller's API, and compares them step by step: the same
steps, the same result (from the cache or executed), the same duration
to the log's 0.1 s.

    test/verify/joblogs.py --gitlab https://gitlab.com/ --token-file PAT \\
        --project 87786070 --pipeline 2996722598 \\
        --controller https://127.0.0.1:18443 --controller-token-file TOKEN

Exit status 1 when anything disagrees. Python 3 standard library only.
"""
import argparse, hashlib, json, re, ssl, sys, time, urllib.error, urllib.request
from datetime import datetime

ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]|\r")
PREFIX = re.compile(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z [0-9]+[OE]\+? ?")
HEAD = re.compile(r"^#(\d+) (\[[^\]]*\d+/\d+\]) (.*)$")
DONE = re.compile(r"^#(\d+) DONE (\d+(?:\.\d+)?)s$")
CACHED = re.compile(r"^#(\d+) CACHED$")
ERROR = re.compile(r"^#(\d+) ERROR")


def get(url, headers, ctx=None, data=None, tries=4):
    req = urllib.request.Request(url, headers=headers, data=data, method="POST" if data is not None else "GET")
    for i in range(tries):
        try:
            with urllib.request.urlopen(req, context=ctx, timeout=60) as r:
                return r.read()
        except (OSError, urllib.error.HTTPError) as e:
            # GitLab's API times out or answers 5xx now and then.
            if i == tries - 1 or (isinstance(e, urllib.error.HTTPError) and e.code < 500 and e.code != 429):
                raise
            time.sleep(5 * (i + 1))


def when(x):
    """An RFC 3339 time as Go writes it (any number of fraction digits)."""
    m = re.match(r"^(.*T\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$", x)
    frac = (m.group(2) or "").ljust(6, "0")[:6]
    tz = "+00:00" if m.group(3) == "Z" else m.group(3)
    return datetime.fromisoformat(f"{m.group(1)}.{frac}{tz}")


def log_steps(text):
    """Dockerfile steps of one or more buildctl runs in a job log, in order:
    [(key, name, result, seconds)], result "cached", "done" or "error"."""
    lines = [PREFIX.sub("", ANSI.sub("", l)) for l in text.split("\n")]
    runs, names, results = [], {}, {}
    for l in lines:
        if l.startswith("$ buildctl") and " build " in l:
            runs.append(([], {}, {}))
            continue
        if not runs:
            continue
        order, names, results = runs[-1]
        m = HEAD.match(l)
        if m and m.group(1) not in names:
            names[m.group(1)] = (m.group(2), m.group(3))
            order.append(m.group(1))
            continue
        m = DONE.match(l)
        if m and m.group(1) in names:
            results[m.group(1)] = ("done", float(m.group(2)))
            continue
        m = CACHED.match(l)
        if m and m.group(1) in names:
            results[m.group(1)] = ("cached", 0.0)
            continue
        m = ERROR.match(l)
        if m and m.group(1) in names:
            results[m.group(1)] = ("error", None)
    out = []
    for order, names, results in runs:
        # A step shared with a parallel build appears twice: this build's
        # vertex and the one that ran (BuildKit streams it to both). One
        # step per key: the first vertex's result, else the other's.
        steps, at = [], {}
        for n in order:
            key, name = names[n]
            if name.startswith("FROM "):
                continue  # base images: resolved in every build, not steps of the Dockerfile's own
            res, secs = results.get(n, ("unknown", None))
            if key in at:
                i = at[key]
                if steps[i][2] in ("unknown", "cached") and res == "done":
                    steps[i] = (key, steps[i][1], res, secs)
                continue
            at[key] = len(steps)
            steps.append((key, name, res, secs))
        out.append(steps)
    return out


def main():
    a = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    a.add_argument("--gitlab", required=True)
    a.add_argument("--token-file", required=True)
    a.add_argument("--project", required=True)
    a.add_argument("--pipeline", required=True, action="append")
    a.add_argument("--controller", required=True)
    a.add_argument("--controller-token-file", required=True)
    a.add_argument("--tolerance", type=float, default=0.15, help="seconds between the log's and the console's duration")
    a.add_argument("--verbose", action="store_true")
    o = a.parse_args()
    gl = {"PRIVATE-TOKEN": open(o.token_file).read().strip()}
    ct = {"Authorization": "Bearer " + open(o.controller_token_file).read().strip()}
    insecure = ssl.create_default_context()
    insecure.check_hostname, insecure.verify_mode = False, ssl.CERT_NONE
    instance = hashlib.sha256(o.gitlab.encode()).hexdigest()[:6]
    api = o.gitlab.rstrip("/") + "/api/v4"

    def call(method, *args):
        r = json.loads(get(o.controller + "/internal/v1/call/" + method, dict(ct, **{"Content-Type": "application/json"}),
                           insecure, json.dumps(list(args)).encode()))
        if r.get("err"):
            raise RuntimeError(f"{method}: {r['err']}")
        return r["r"]

    problems, checked_steps, checked_builds = [], 0, 0
    for pid in o.pipeline:
        jobs, page = [], 1
        while True:
            more = json.loads(get(f"{api}/projects/{o.project}/pipelines/{pid}/jobs?per_page=100&page={page}", gl))
            jobs += more
            if len(more) < 100:
                break
            page += 1
        for j in jobs:
            if j["status"] in ("skipped", "created", "manual", "pending"):
                continue
            trace = get(f"{api}/projects/{o.project}/jobs/{j['id']}/trace", gl).decode("utf-8", "replace")
            runs = log_steps(trace)
            if not runs:
                continue
            ids = call("JobBuilds", instance, j["id"])[0] or []
            pages = sorted((call("BuildPage", i)[0] for i in ids), key=lambda p: p["Build"]["Started"])
            where = f"pipeline {pid} job {j['id']} ({j['name']})"
            if len(pages) != len(runs):
                problems.append(f"{where}: {len(runs)} buildctl builds in the log, {len(pages)} builds in the console")
                continue
            for steps, page in zip(runs, pages):
                checked_builds += 1
                b = page["Build"]
                console = [s for s in b["Steps"] if not s["Name"].startswith("FROM ")]
                # The log prints steps as they finish (cached ones all at
                # once, in any order); the console in the Dockerfile's.
                bykey = {s["Key"]: s for s in console}
                ckeys = sorted(bykey)
                lkeys = sorted(s[0] for s in steps)
                if ckeys != lkeys:
                    problems.append(f"{where}: steps differ: log {lkeys} console {ckeys}")
                    continue
                for key, name, res, secs in steps:
                    s = bykey[key]
                    checked_steps += 1
                    state = s["State"]
                    want = {"cached": "cached", "done": ("built", "received"), "error": ("failed", "canceled", "built")}.get(res, ())
                    if state != want and state not in (want if isinstance(want, tuple) else ()):
                        problems.append(f"{where} {key}: the log says {res}, the console {state}")
                        continue
                    if res == "done" and state == "built":
                        d = (when(s["Completed"]) - when(s["Started"])).total_seconds()
                        if abs(d - secs) > o.tolerance:
                            problems.append(f"{where} {key}: the log says {secs:.1f}s, the console {d:.2f}s")
                    if o.verbose:
                        print(f"ok {where} {key} {res} {state}")
    print(f"{checked_builds} builds, {checked_steps} steps compared; {len(problems)} disagreements")
    for p in problems:
        print("  " + p)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
