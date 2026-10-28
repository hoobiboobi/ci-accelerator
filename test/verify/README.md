# Checking the console against the jobs' own logs

`joblogs.py` compares what the console says about each image build with
what the CI job printed while it built: every job that runs `buildctl
--progress plain` prints each step as BuildKit's client saw it
(`#N [stage 2/4] RUN ...`, then `#N CACHED`, `#N DONE 1.2s` or `#N
ERROR`). For every job of the given pipelines it reads the log from
GitLab and the job's builds from the controller's API, and checks that
both list the same steps, the same result (from the cache, executed, or
received from another build of the pipeline) and, for executed steps, the
same duration to the log's 0.1 s. Exit status 1 when anything disagrees.

Use it on a live installation after changing how the console reads
builds (internal/history, internal/collector, internal/store), before a
release, and when a page looks wrong.

```sh
# The controller's API, from this machine:
kubectl -n <namespace> port-forward svc/<release>-controller 18443:8443 &
kubectl -n <namespace> create token <release>-web \
  --audience ci-accelerator-controller --duration 30m > /tmp/controller-token

test/verify/joblogs.py --gitlab https://gitlab.com/ --token-file <read_api token file> \
  --project <project id> --pipeline <id> [--pipeline <id> ...] \
  --controller https://127.0.0.1:18443 --controller-token-file /tmp/controller-token
```

`--verbose` prints every step compared; `--tolerance` sets the allowed
difference of a duration (seconds, default 0.15). Python 3 standard
library only. The tokens are read from files, never from the command line.
