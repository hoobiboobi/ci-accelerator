# Local cluster with kind

Prerequisites (macOS): Docker Desktop or OrbStack, then

```sh
brew install kind kubectl helm
brew install buildkit   # optional: buildctl on the laptop, for the NodePort example
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

You get one control-plane and three build workers in three fake zones,
labelled `ci-accelerator.io/node-role=build` and tainted with the same key
(the taint is applied separately: kubelet refuses to self-assign
`node-role.kubernetes.io/*` labels, and kubeadm join patches for taints are
fragile across kind versions). The chart's default node selector and
tolerations match this label and taint. Node images are pinned by digest.

Install a development release:

```sh
kubectl --context kind-buildkit create namespace ci-accelerator
kubectl --context kind-buildkit label namespace ci-accelerator \
  pod-security.kubernetes.io/enforce=privileged
helm --kube-context kind-buildkit upgrade --install bk charts/buildkit \
  -n ci-accelerator -f examples/kind/values.yaml
```

The namespace is labelled before the install on purpose: the daemon is a
privileged pod, and clusters that enforce Pod Security `baseline` by default
would otherwise create no pods (the chart refuses namespaces labelled with
another level, but cannot see a cluster-wide default).

The first worker maps host port `1234` to NodePort `31234` on `127.0.0.1`
only, so the daemon on that node is reachable from the laptop as
`tcp://127.0.0.1:1234` and not from the network: it has no TLS yet.

Run the end-to-end checks (a separate release that cleans up after itself;
`KEEP=1` leaves everything for debugging, `make e2e-clean` removes it):

```sh
make e2e
```

Delete the cluster:

```sh
make kind-down
```

Notes:

- On Apple Silicon the nodes are arm64; test Dockerfiles must use multi-arch
  base images.
- There is no fast local disk here: timings mean nothing, cache hits do.
  All nodes share the Docker VM disk, and kind turns off kubelet's disk
  eviction thresholds, so disk-pressure behaviour cannot be tested here.
- A multi-node cluster is only needed for routing and failover. For quick
  chart iteration a single-node cluster (`kind create cluster`) is enough
  with `--set global.buildNodes.enabled=false`, and `make lint unittest
  validate` needs no cluster at all.
- A cluster created from an older version of this config publishes the port
  on all interfaces; recreate it (`make kind-down kind-up`).
