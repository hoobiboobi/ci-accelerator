#!/usr/bin/env python3
"""Tests of joblogs.py without GitLab or a controller: the log parser on a
saved job log, and the verdicts with both APIs stood in.

    python3 test/verify/joblogs_test.py
"""
import importlib.util, io, json, os, sys, tempfile, unittest
from contextlib import redirect_stdout

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("joblogs", os.path.join(HERE, "joblogs.py"))
joblogs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(joblogs)

# A job log as GitLab serves it: timestamps, stream markers, colours.
TRACE = "\n".join([
    "2026-11-16T10:00:00.000001Z 00O \x1b[32;1m$ buildctl --addr tcp://r:1234 build --frontend dockerfile.v0\x1b[0;m",
    "2026-11-16T10:00:00.100000Z 00O #4 [base 1/3] FROM docker.io/library/alpine:3.22",
    "2026-11-16T10:00:00.200000Z 00O #4 DONE 0.0s",
    "2026-11-16T10:00:00.300000Z 00O #6 [base 2/3] RUN apk add curl",
    "2026-11-16T10:00:00.400000Z 00O #6 CACHED",
    "2026-11-16T10:00:00.500000Z 00O #7 [base 3/3] COPY . .",
    "2026-11-16T10:00:00.600000Z 00O #7 DONE 1.2s",
    "2026-11-16T10:00:00.700000Z 00O #9 [base 3/3] COPY . .",
    "2026-11-16T10:00:00.800000Z 00O #9 CACHED",
])


def page(state="built"):
    return {"Build": {"Started": "2026-11-16T10:00:00Z", "Steps": [
        {"Name": "FROM docker.io/library/alpine:3.22", "Key": "[base 1/3]", "State": "built"},
        {"Name": "RUN apk add curl", "Key": "[base 2/3]", "State": "cached"},
        # 1.2 s, as the log says.
        {"Name": "COPY . .", "Key": "[base 3/3]", "State": state,
         "Started": "2026-11-16T10:00:01Z", "Completed": "2026-11-16T10:00:02.2Z"},
    ]}}


class LogSteps(unittest.TestCase):
    def test_saved_log(self):
        runs = joblogs.log_steps(TRACE)
        self.assertEqual(len(runs), 1)
        # FROM is left out; a step printed twice (a shared vertex) is one
        # step, with the result of the vertex that ran.
        self.assertEqual(runs[0], [("[base 2/3]", "RUN apk add curl", "cached", 0.0),
                                   ("[base 3/3]", "COPY . .", "done", 1.2)])

    def test_no_buildctl(self):
        self.assertEqual(joblogs.log_steps("$ make test\nok\n"), [])


class Verdicts(unittest.TestCase):
    """main() with GitLab and the controller stood in."""

    def run_main(self, trace, builds, pages):
        def get(url, headers, ctx=None, data=None, tries=4):
            if "/pipelines/" in url:
                return json.dumps([{"id": 7, "name": "build", "status": "success"}]).encode()
            if url.endswith("/trace"):
                return trace.encode()
            method = url.rsplit("/", 1)[1]
            if method == "JobBuilds":
                return json.dumps({"r": [builds]}).encode()
            if method == "BuildPage":
                return json.dumps({"r": [pages[json.loads(data)[0]]]}).encode()
            raise AssertionError(url)
        with tempfile.TemporaryDirectory() as d:
            tok = os.path.join(d, "t")
            with open(tok, "w") as f:
                f.write("x")
            argv, real = sys.argv, joblogs.get
            sys.argv = ["joblogs.py", "--gitlab", "https://gitlab.example.com/", "--token-file", tok, "--project", "1",
                        "--pipeline", "2", "--controller", "https://127.0.0.1:1", "--controller-token-file", tok]
            joblogs.get = get
            out = io.StringIO()
            try:
                with redirect_stdout(out):
                    rc = joblogs.main()
            finally:
                sys.argv, joblogs.get = argv, real
        return rc, out.getvalue()

    def test_agree(self):
        rc, out = self.run_main(TRACE, [1], {1: page()})
        self.assertEqual(rc, 0, out)
        self.assertIn("1 builds, 2 steps compared; 0 disagreements", out)

    def test_nothing_compared_fails(self):
        rc, out = self.run_main("$ make test\nok\n", [], {})
        self.assertEqual(rc, 1, out)
        self.assertIn("nothing compared", out)

    def test_console_build_without_a_log_run_fails(self):
        rc, out = self.run_main("$ make test\nok\n", [1], {1: page()})
        self.assertEqual(rc, 1, out)
        self.assertIn("no buildctl build in the log, 1 builds in the console", out)

    def test_wrong_result_fails(self):
        rc, out = self.run_main(TRACE, [1], {1: page(state="cached")})
        self.assertEqual(rc, 1, out)
        self.assertIn("the log says done, the console cached", out)


if __name__ == "__main__":
    unittest.main()
