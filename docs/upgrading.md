# Upgrading

Every release upgrades in place from the release before it. The one
exception is 0.3.0, which a 0.2.x install reaches by installing again
([below](#from-02x-to-030-or-later-install-again)). Before every change to
the charts or the code is merged, the end-to-end tests install the previous published release,
upgrade it to the change and roll it back again
([ADR-0027](adr/0027-upgrades-and-branches.md)).

Pick the section for how you installed:

- [With Helm](#helm-installs) (`helm install`, the box or the charts on
  their own);
- [On one machine with quickstart.sh](#one-machine-installs-quickstartsh);
- [With Flux or Argo CD](#gitops-flux-argo-cd).

## Before you upgrade

1. **Read the release's Upgrade notes**, in [CHANGELOG.md](../CHANGELOG.md)
   or on the GitHub release. They say what restarts (whether the build
   daemons roll), what to change in your values, and whether a rollback
   keeps working. If you are several releases behind, read the notes of
   every release in between: only the step from the previous release is
   tested, and so far no release in place has needed a stop at an
   intermediate one (0.2.x to 0.3.0 is a reinstall either way).
2. **Note what runs now and keep your values:**

   ```sh
   helm list -n ci-accelerator
   helm get values ci-accelerator -n ci-accelerator -o yaml > values-before-upgrade.yaml
   ```

   On a one-machine install, use the script's Helm and the cluster's
   kubeconfig ([below](#apply-your-own-changes-or-upgrade-by-hand)), for
   example `sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml
   /usr/local/lib/ci-accelerator/helm-v4.3.1/helm list -n ci-accelerator`;
   your values are in `/var/lib/ci-accelerator/values.yaml`.
3. **Pick a quiet moment when the daemons restart.** A daemon restart
   waits for the builds running on its node, one node at a time, up to
   `buildkit.terminationGracePeriodSeconds` (300 s). With the default
   `emptyDir` storage a restarted daemon starts with an empty cache; with
   `hostPath` it keeps its cache. What restarts the daemons, and how long
   a rollout takes, is in
   [architecture](architecture.md#rollouts-and-restarts).

## Helm installs

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <new version> \
  -n ci-accelerator --reset-then-reuse-values --wait --timeout 15m
```

- **`--reset-then-reuse-values`** keeps the values you set and takes the
  new version's defaults for everything else. Do not use `--reuse-values`:
  it keeps the old release's defaults too, so a setting the new chart adds
  is missing (a runner cache, for one, then keeps its old configuration).
  Passing your values file again with `-f` works as well.
- **`--timeout`:** a daemon rollout takes up to *nodes × (longest build +
  daemon startup)*. Helm's default of 5 minutes is too short when builds
  run while the daemons restart.
- **Images pinned by digest** (as production installs should be): set the
  new release's digests from its notes in the same upgrade,
  `--set control-plane.image.digest=sha256:...`, and
  `buildkit.diskProbe.image.digest` when the notes name a new probe image.
  A digest belongs to one release: a kept digest of the release before
  would run that release under the new version's name, so the charts
  refuse it and say which version they run. The daemons' BuildKit image is
  pinned by the chart itself.
- **`charts/buildkit` or `charts/control-plane` on their own** upgrade the
  same way, with their own chart name.

## One-machine installs (quickstart.sh)

`quickstart.sh` installed k3s, Helm, cert-manager and the box, and keeps
its own state on the machine:

| What | Where |
|---|---|
| Your values (written once, never changed by the script) | `/var/lib/ci-accelerator/values.yaml` |
| Helm's output for the box, from the last run | `/var/lib/ci-accelerator/install.log` |
| Helm, one directory per version | `/usr/local/lib/ci-accelerator/helm-<version>/helm` |
| The cluster's kubeconfig (root only) | `/etc/rancher/k3s/k3s.yaml` |
| The build cache (unless you chose another `--build-dir`) | `/var/lib/ci-accelerator/buildkit` |

### Upgrade with the new release's script

This is the way to upgrade a one-machine install. The new release's
script brings that release's chart, Helm and cert-manager.

1. Download the new release's script and its checksums, and check the
   script against them:

   ```sh
   v=<new version>
   curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v$v/quickstart.sh
   curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v$v/SHA256SUMS
   grep ' quickstart.sh$' SHA256SUMS | sha256sum -c -
   ```

   The checksums are signed by the release workflow. To check that too
   (cosign 2.6 or newer), before the `sha256sum` line:

   ```sh
   curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v$v/SHA256SUMS.sigstore.json
   cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
     --certificate-identity https://github.com/hoobiboobi/ci-accelerator/.github/workflows/release.yml@refs/tags/v$v \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```
2. Run it with the same GitLab URL and the runner token, as on the first
   install. A token is never read from the command line: put it in a file
   (or `RUNNER_TOKEN=...` in the environment, or type it when asked).

   ```sh
   sudo bash quickstart.sh --gitlab-url https://gitlab.example.com/ --runner-token-file ./runner-token
   ```

   - Add `--yes` when no terminal can answer the question it asks.
   - Do not pass `--build-dir`: the cache stays where `values.yaml` says.
   - A GitLab token for the console that the running install uses stays
     in use (scripts from 0.4.1 on; with an older script, pass
     `--gitlab-token-file` again). To add one now, pass
     `--gitlab-token-file`.
3. The script prints what it does before it starts, and the console's
   address and the next steps when it is done. A step that fails says so
   on the terminal; if the box's own Helm upgrade fails, the script also
   prints `/var/lib/ci-accelerator/install.log`.

What a second run changes, and what it leaves alone:

| | |
|---|---|
| The box (all charts) | upgraded to the script's release, the chart checked against that release's `SHA256SUMS` |
| cert-manager | upgraded to the version the script pins |
| Helm | the script's version installed next to the old one |
| The runner token Secret | written again from the token you give |
| `values.yaml` | kept as it is: your sign-in, cache path, sizes and anything else you added stay |
| k3s | **never upgraded**: it stays at the version the first install put there |
| The daemon's resources | as sized from the machine at the first install (in `values.yaml`) |

Run the new release's script rather than an old script with `--version`:
an old script brings its own, older Helm and cert-manager pins and lacks
the checks of later releases.

**k3s.** The script never upgrades k3s. A release that needs a newer
Kubernetes says so in its Upgrade notes; upgrade k3s with its own
[upgrade procedure](https://docs.k3s.io/upgrades/manual), or reinstall
with `--uninstall` (which removes the build cache too).

### Apply your own changes, or upgrade by hand

To apply a change you made in `values.yaml`, or to upgrade only the box,
use the Helm the script installed (the newest under
`/usr/local/lib/ci-accelerator/`) with the cluster's kubeconfig:

```sh
ls -d /usr/local/lib/ci-accelerator/helm-*/                 # which Helm is there
H=/usr/local/lib/ci-accelerator/helm-v4.3.1/helm           # the newest of them
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml $H upgrade ci-accelerator \
  oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <version> \
  -n ci-accelerator -f /var/lib/ci-accelerator/values.yaml --wait --timeout 15m
```

`--version` is the version you run now (to apply values) or the new one
(to upgrade the box alone; cert-manager and Helm then stay as they are).
A GitLab token the script set with `--set` is lost by such an upgrade
unless `values.yaml` names it: add

```yaml
control-plane:
  gitlab:
    token:
      existingSecret: gitlab-api-token
```

under the existing `control-plane:` key. The script warns when this is the case.

## GitOps (Flux, Argo CD)

Change the chart version in Git: `ref.tag` of the Flux `OCIRepository` or
`targetRevision` of the Argo CD `Application`
([gitops.md](gitops.md#flux)). Change the image digests in the same commit
when you pin them (see [Helm installs](#helm-installs)). Your values live
in Git, so nothing else carries over. Renovate can open these changes for
you.

## Check the upgrade

```sh
helm list -n ci-accelerator                                   # STATUS deployed, the new CHART version
kubectl -n ci-accelerator get pods                            # everything Running and ready
kubectl -n ci-accelerator rollout status ds/ci-accelerator-buildkit --timeout 15m
kubectl -n ci-accelerator get ds ci-accelerator-buildkit -o jsonpath='{.spec.template.spec.containers[0].image}'
```

On the one-machine install, prefix `kubectl` with `sudo k3s` and use the
Helm and kubeconfig above. The console names its version at the bottom of
its sidebar, and each daemon's page on Nodes its BuildKit version. A
pipeline's second build of the same commit should come from the cache
again (with a `hostPath` cache also right after the restart).

## Roll back

```sh
helm history ci-accelerator -n ci-accelerator
helm rollback ci-accelerator <revision> -n ci-accelerator --wait --timeout 15m
```

The Upgrade notes say whether the console's store stays usable by the
release before. A controller refuses a store written by a newer schema
rather than damage it; the notes of a release that changes the schema
say what to do. On the one-machine install, use the script's Helm and
kubeconfig as above. Do not run an older `quickstart.sh` to go back: it
would also move cert-manager back.

## From older releases

### From 0.2.x to 0.3.0 or later: install again

0.3.0 split the console into the control plane: chart `control-plane`
(was `console`), image `ghcr.io/hoobiboobi/ci-accelerator` (was
`ci-accelerator-console`), a controller and a web with new names for every
object (`<release>-controller`, `<release>-web`) and value, and the build
router in front of the daemons. Helm refuses to upgrade a 0.2.x release in
place and says how to move; so does `quickstart.sh` (from 0.4.1 on before
it changes anything on the machine). Install the current release again:

1. Remove the release and wait for its pods:
   ```sh
   helm uninstall ci-accelerator -n ci-accelerator --wait
   kubectl -n ci-accelerator wait --for=delete pod -l app.kubernetes.io/instance=ci-accelerator --timeout=10m
   ```
   The jobs namespace goes with the release (running jobs fail); the
   daemons' cache on a hostPath stays. With cert-manager, then delete the
   daemons' certificate Secret: it outlives the release, and the new one
   names the build router (the daemons read their certificate only at
   start):
   ```sh
   kubectl -n ci-accelerator delete secret ci-accelerator-buildkit-tls
   ```
   Without cert-manager (`buildkit.tls.source=existingSecret`), make the
   daemons' certificate again from the same CA (the same `--out`
   directory, so the clients' certificates stay valid) and replace the
   Secret named in `buildkit.tls.existingSecret` with it:
   ```sh
   scripts/buildkit-certs.sh --name ci-accelerator-buildkit --namespace ci-accelerator \
     --out <your certs directory> --router ci-accelerator
   ```
2. In your values, move everything under `console:` to `control-plane:`.
   The controller's pod settings and volume go under
   `control-plane.controller` (`resources`, `nodeSelector`, `tolerations`,
   `affinity`, `podLabels`, `podAnnotations`, `persistence`), the web's
   under `control-plane.web` (`port`, from `service.port`; the web no
   longer follows the controller's placement). Drop
   `daemons.diskProbePort`: the daemons tell the controller their probe's
   port. An image mirror now mirrors `ghcr.io/hoobiboobi/ci-accelerator`
   (`control-plane.image` and `buildkit.diskProbe.image`). A runner
   cache's `consoleSecretName` is now `controllerSecretName`. A runner of
   your own sets `BUILDKIT_REMOTE_ADDR` to the router,
   `tcp://ci-accelerator-router.ci-accelerator.svc:1234`.
3. Install the current release with those values. Without cert-manager,
   issue the controller's API certificate from the same CA and name its
   Secret in `control-plane.controller.api.tls.existingSecret` (the script
   prints the `kubectl create secret` command):
   ```sh
   scripts/buildkit-certs.sh --name ci-accelerator-buildkit --namespace ci-accelerator \
     --out <your certs directory> --controller ci-accelerator
   ```
4. The new console starts with an empty history. The old one stays on
   `<release>-console-data`; delete it when you no longer need it, with
   the old client certificate Secret:
   ```sh
   kubectl -n ci-accelerator delete pvc ci-accelerator-console-data
   kubectl -n ci-accelerator delete secret ci-accelerator-console-client --ignore-not-found
   ```
   Port-forwards, an Ingress or a `global.console.url` tunnel point at the
   Service `<release>-web` now (it was `<release>-console`).

On the one-machine install, `quickstart.sh` stops with these steps: run
the `helm uninstall` and `kubectl delete secret` it prints, rename
`console:` to `control-plane:` in `/var/lib/ci-accelerator/values.yaml` (or
delete the file to start over) and run the new release's script again.
GitOps: delete the Application or HelmRelease and create it again with the
new values. `charts/buildkit` installed on its own upgrades in place; its
daemons restart once.

### From 0.1.0

0.1.0 installed VictoriaMetrics and VictoriaTraces for metrics and traces.
The upgrade removes them, but their volumes stay. Find them and delete
them:

```sh
kubectl -n ci-accelerator get pvc | grep -e victoria-metrics-single -e vt-single
kubectl -n ci-accelerator delete pvc <name> <name>
```

If the daemons still try to send to the removed Services, the upgrade
reused the old endpoint values: upgrade again with
`--reset-then-reuse-values`, or set `buildkit.otel.metricsEndpoint` and
`tracesEndpoint` to `""` or to your own backend.

## If an upgrade fails

- The charts refuse some upgrades with a message that says what to do (a
  kept digest, values of an older release, a namespace still
  terminating): [the install fails](troubleshooting.md#the-install-fails)
  in troubleshooting lists them, and
  [architecture](architecture.md#install-time-checks) what is checked.
- `helm upgrade --wait` that times out leaves the release `failed` while
  the rollout goes on: check the pods, then run the upgrade again or roll
  back.
- A daemon that crashes on the new configuration stops the rollout on
  its first node; the other daemons keep serving. Roll back, then fix the
  value.
- On the one-machine install, the box's Helm output is in
  `/var/lib/ci-accelerator/install.log`; the steps before it (k3s, Helm,
  cert-manager, the downloads) fail on the terminal.
