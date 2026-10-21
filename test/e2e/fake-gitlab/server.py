"""A stand-in for the few GitLab API calls a gitlab-runner makes, so the e2e
can run the real runner (official chart, Kubernetes executor) against the
buildkit daemons without a GitLab instance.

Runner side (gitlab-runner 19.4, network/gitlab.go):
  POST   /api/v4/runners/verify      registration and liveness (200 + id)
  POST   /api/v4/jobs/request        204 = no job, 201 + job JSON
  PATCH  /api/v4/jobs/<id>/trace     job log chunks (202)
  PUT    /api/v4/jobs/<id>           final state (200)
  DELETE /api/v4/runners...          unregister on shutdown (204)

Test side:
  POST   /e2e/jobs                   {"image": ..., "script": [...], "pipeline_id": n} -> {"id": n}
  GET    /e2e/jobs/<id>              {"state": ..., "trace": ...}

Not GitLab: no git, artifacts, cache or services. Python stdlib only.
"""

import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
QUEUE = []  # job ids waiting for a runner
JOBS = {}  # id -> {"spec": ..., "state": ..., "trace": ...}


def job_payload(job_id, spec, host):
    timeout = spec.get("timeout", 600)
    return {
        "id": job_id,
        "token": "job-token-%d" % job_id,
        "allow_git_fetch": False,
        "job_info": {"name": "e2e-%d" % job_id, "stage": "build", "project_id": 1, "project_name": "e2e"},
        # As GitLab sends it: with the job token in it. The runner derives the
        # job's URL from it (and must drop the token).
        "git_info": {"repo_url": "http://gitlab-ci-token:job-token-%d@%s/e2e/project.git" % (job_id, host), "ref": "main", "sha": "0" * 40, "before_sha": "0" * 40, "ref_type": "branch"},
        "runner_info": {"timeout": timeout},
        # No checkout: the job builds a context it writes itself.
        # A few of the predefined variables GitLab sends; the runner adds the
        # ones it derives itself.
        "variables": [
            {"key": "GIT_STRATEGY", "value": "none", "public": True},
            {"key": "CI_JOB_ID", "value": str(job_id), "public": True},
            {"key": "CI_PIPELINE_ID", "value": str(spec.get("pipeline_id", 1000)), "public": True},
            {"key": "CI_PROJECT_PATH", "value": "e2e/project", "public": True},
            {"key": "CI_PIPELINE_SOURCE", "value": "push", "public": True},
            {"key": "CI_PIPELINE_CREATED_AT", "value": "2026-10-20T10:00:00Z", "public": True},
            {"key": "CI_JOB_STAGE", "value": "build", "public": True},
            {"key": "CI_JOB_STARTED_AT", "value": "2026-10-20T10:00:05Z", "public": True},
            # A commit title is written by whoever pushes: it must reach the
            # pod as text, never expanded.
            {"key": "CI_COMMIT_TITLE", "value": 'e2e: "quoted" $CI_JOB_ID ${CI_PIPELINE_ID}', "public": True},
            {"key": "CI_COMMIT_AUTHOR", "value": "E2E Author <e2e@example.com>", "public": True},
        ],
        "steps": [{"name": "script", "script": spec["script"], "timeout": timeout, "when": "on_success", "allow_failure": False}],
        "image": {"name": spec["image"]},
        "features": {"trace_sections": True},
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _send(self, code, body=None, headers=None):
        data = b"" if body is None else json.dumps(body).encode()
        self.send_response(code)
        if body is not None:
            self.send_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def do_POST(self):
        body = self._body()
        if self.path == "/api/v4/runners/verify":
            return self._send(200, {"id": 1, "token": "verified", "token_expires_at": None})
        if self.path == "/api/v4/jobs/request":
            with LOCK:
                job_id = QUEUE.pop(0) if QUEUE else None
                if job_id is not None:
                    JOBS[job_id]["state"] = "running"
                    return self._send(201, job_payload(job_id, JOBS[job_id]["spec"], self.headers.get("Host", "fake-gitlab")))
            return self._send(204)
        if self.path == "/e2e/jobs":
            spec = json.loads(body or b"{}")
            with LOCK:
                job_id = len(JOBS) + 1
                JOBS[job_id] = {"spec": spec, "state": "pending", "trace": ""}
                QUEUE.append(job_id)
            return self._send(201, {"id": job_id})
        return self._send(404, {"message": "not faked: POST " + self.path})

    def do_PATCH(self):
        body = self._body()
        m = re.match(r"^/api/v4/jobs/(\d+)/trace", self.path)
        if not m or int(m.group(1)) not in JOBS:
            return self._send(404, {"message": "not faked: PATCH " + self.path})
        with LOCK:
            job = JOBS[int(m.group(1))]
            job["trace"] += body.decode("utf-8", "replace")
            size = len(job["trace"].encode())
        return self._send(202, headers={"Job-Status": "running", "Range": "0-%d" % size})

    def do_PUT(self):
        body = self._body()
        m = re.match(r"^/api/v4/jobs/(\d+)$", self.path)
        if not m or int(m.group(1)) not in JOBS:
            return self._send(404, {"message": "not faked: PUT " + self.path})
        update = json.loads(body or b"{}")
        with LOCK:
            job = JOBS[int(m.group(1))]
            job["state"] = update.get("state", job["state"])
            job["exit_code"] = update.get("exit_code")
        return self._send(200, {})

    def do_DELETE(self):
        self._body()
        return self._send(204)

    def do_GET(self):
        m = re.match(r"^/e2e/jobs/(\d+)$", self.path)
        if m and int(m.group(1)) in JOBS:
            with LOCK:
                job = dict(JOBS[int(m.group(1))])
            job.pop("spec", None)
            job["id"] = int(m.group(1))
            return self._send(200, job)
        if self.path == "/healthz":
            return self._send(200, {"ok": True})
        return self._send(404, {"message": "not faked: GET " + self.path})

    def log_message(self, fmt, *args):
        print("%s %s" % (self.command, self.path), flush=True)


if __name__ == "__main__":
    ThreadingHTTPServer(("", 8080), Handler).serve_forever()
