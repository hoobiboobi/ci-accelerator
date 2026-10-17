# Local cluster with kind

Prerequisites (macOS): Docker Desktop or OrbStack, then

```sh
brew install kind kubectl helm buildkit   # buildkit provides buildctl
```

Create the cluster and mark the build nodes (idempotent):

```sh
make kind-up
```

which runs:

```sh
kind create cluster --name buildkit --config examples/kind/kind-config.yaml
kubectl --context kind-buildkit taint nodes -l ci-accelerator.io/node-role=build \
  ci-accelerator.io/node-role=build:NoSchedule --overwrite
```

Run the end-to-end checks (separate release, cleans up after itself;
`KEEP=1` leaves everything for debugging):

```sh
make e2e
```

You get one control-plane and three build workers in three fake zones,
labelled `ci-accelerator.io/node-role=build` and tainted with the same key
(the taint is applied separately: kubelet refuses to self-assign
`node-role.kubernetes.io/*` labels, and kubeadm join patches for taints are
fragile across kind versions). The chart's default `nodeSelector` and
`tolerations` match this label/taint.

The first worker maps host port `1234` to NodePort `31234`, so once the chart
is installed with the kind values file the daemon on that node is reachable
from the host as `tcp://127.0.0.1:1234`.

Delete:

```sh
make kind-down
```

Notes:

- On Apple Silicon the nodes are arm64; test Dockerfiles must use multi-arch
  base images. GitHub Actions runs the same config on amd64.
- There is no fast local disk here. Timings mean nothing; cache hits do.
- A multi-node cluster is only needed to test node-local routing. For quick
  chart iteration a single-node cluster (`kind create cluster`) is enough,
  and `helm lint` / `helm template` / helm-unittest need no cluster at all.
