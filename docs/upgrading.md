# Upgrading

Every release upgrades in place from the release before it, and you may
go straight to the newest one: read the notes of every release in
between, set only the newest one's digests. The one exception is 0.3.0,
which a 0.2.x install reaches by installing again
([below](#from-02x-to-030-or-later-install-again)). Before every change
to the charts or the code is merged, the end-to-end tests install the
previous published release, upgrade it to the change and roll it back
again ([ADR-0027](adr/0027-upgrades-and-branches.md)).

Three procedures, one per kind of install. If you inherited the box and
do not know which: a file `/var/lib/ci-accelerator/.quickstart` on the
machine means [quickstart.sh](#one-machine-installs-quickstartsh); a
`HelmRelease` or an Argo CD `Application` named `ci-accelerator` means
[GitOps](#gitops-flux-argo-cd); otherwise [Helm](#helm-installs).

## Before you upgrade

1. **Read the release's Upgrade notes**, in [CHANGELOG.md](../CHANGELOG.md)
   or on the release page
   (<https://github.com/hoobiboobi/ci-accelerator/releases>). They say
   what restarts, what to change in your values, and whether a rollback
   keeps working. The release page also lists the **Digests** for an
   install that pins its images.
2. **Know what runs now:**

   ```sh
   helm list -n ci-accelerator
   ```

   ```
   NAME            NAMESPACE       REVISION  STATUS    CHART                 APP VERSION
   ci-accelerator  ci-accelerator  1         deployed  ci-accelerator-0.5.1  0.5.1
   ```

   The CHART column is your version (nothing listed: `helm list -A`
   finds the release under its namespace). Then save the values you set,
   as a record and for a rollback by hand:

   ```sh
   helm get values ci-accelerator -n ci-accelerator -o yaml > values-before-upgrade.yaml
   ```

   This is everything you passed at install and since, with `-f` or
   `--set`. If it contains `digest:` lines, your install pins its images
   and the upgrade must set the new digests (below).
3. **Pick the moment.** What restarts is in the notes. When the daemons
   restart (an image, their configuration, their resources changed),
   they go one node at a time, each waiting for its running builds
   first, up to `buildkit.terminationGracePeriodSeconds` (300 s): an
   upgrade then takes up to nodes × (longest build + half a minute),
   and with the default `emptyDir` cache every restarted daemon starts
   cold; a `hostPath` cache stays. When only the control plane
   restarts, the console is away for a few seconds and running builds
   go on; a router restart lets its open builds finish for
   `control-plane.router.drainSeconds` (300 s) and then cuts them. A
   job running on the box's runner pod when that pod is replaced may
   fail and need a retry (the official runner chart's stop is not
   tested here). For 0.5.1 to 0.5.2: the controller, the web, the routers and the runner
   pod restart once; the daemons, the store and the cache stay.

## Helm installs

1. The command, with the version from the release page (without the
   `v`):

   ```sh
   helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
     -n ci-accelerator --reset-then-reuse-values --wait --timeout 15m
   ```

   `--reset-then-reuse-values` keeps every value you set (what `helm get
   values` printed) and takes the new version's defaults for everything
   else. Do not use `--reuse-values`: it keeps the old release's defaults
   too, so a setting the new chart adds is missing. Passing your values
   file again with `-f values.yaml` works as well, as long as the file
   has everything you ever set (a `--set` of the install is forgotten
   with `-f` alone). Secrets named in the values (the runner token, a
   GitLab token) are never touched.
2. **Images pinned by digest:** the new release's digests go in the same
   command, from the release page's **Digests** paragraph ("To pin the
   box's images by digest"):

   ```sh
   helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
     -n ci-accelerator --reset-then-reuse-values \
     --set control-plane.image.digest=sha256:<the ci-accelerator image's digest> \
     --wait --timeout 15m
   ```

   Add `--set buildkit.diskProbe.image.digest=sha256:...` only when the
   paragraph names another probe digest than the one you have (the probe
   is `v0.3.3` since 0.3.3). A kept digest of the release before is
   refused with `image.digest ... is the digest of the v0.5.1 image, but
   this chart runs v0.5.2`: the message names the right one. The daemons'
   BuildKit image is pinned by the chart itself.
3. **How long:** a few minutes when the daemons stay; when they roll,
   nodes × (longest build + half a minute), so set `--timeout` to that.
   The command returns with `STATUS: deployed`, `REVISION` one higher,
   and the install notes of the new version.
4. [Check the upgrade](#check-the-upgrade).
5. If it fails: [below](#if-an-upgrade-fails).

**`charts/buildkit` and `charts/control-plane` on their own** upgrade
the same way, each release with its own name; the two charts of one
release belong together: bring both to the same version (recommended:
the daemons first; the end-to-end tests cover the box, not the
standalone order):

```sh
helm upgrade <buildkit release> oci://ghcr.io/hoobiboobi/charts/buildkit --version 0.5.2 \
  -n <namespace> --reset-then-reuse-values --wait --timeout 15m
helm upgrade <control-plane release> oci://ghcr.io/hoobiboobi/charts/control-plane --version 0.5.2 \
  -n <namespace> --reset-then-reuse-values --set image.digest=sha256:... --wait --timeout 15m
```

(In the standalone control-plane chart the digest key is `image.digest`,
and the probe's `diskProbe.image.digest` in the standalone buildkit
chart.)

**cert-manager** is yours: the box needs only its `cert-manager.io/v1`
API (v1.16 or newer), and the version the quickstart pins is the tested
one. Upgrade it with its own procedure (`helm upgrade cert-manager
oci://quay.io/jetstack/charts/cert-manager --version v1.21.3 -n
cert-manager --set crds.enabled=true`, after cert-manager's upgrade
notes); its pods restart, the box's certificates and daemons stay.

**The runner inside the box** is part of the chart: its version changes
only with a box release, and the Upgrade notes say when. A runner of
your own ([install-existing-cluster.md](install-existing-cluster.md#the-cluster-has-its-own-gitlab-runner))
is upgraded by you with the gitlab-runner chart.

### Roll back

```sh
helm history ci-accelerator -n ci-accelerator
```

```
REVISION  UPDATED                   STATUS      CHART                 APP VERSION  DESCRIPTION
1         Fri Nov 13 22:36:15 2026  superseded  ci-accelerator-0.5.1  0.5.1        Install complete
2         Fri Nov 13 23:10:02 2026  deployed    ci-accelerator-0.5.2  0.5.2        Upgrade complete
```

The revision to go back to is the last `superseded` one before the
current (1 here):

```sh
helm rollback ci-accelerator 1 -n ci-accelerator --wait --timeout 15m
```

The rollback brings back that revision's chart and values, your digests
included, and restarts what the upgrade restarted (a daemon rollout
again, if there was one). The Upgrade notes say whether the console's
store stays usable by the release before; a controller refuses a store
written by a newer schema rather than damage it, and the notes of a
release that changes the schema say what to do. 0.5.2 to 0.5.1: the
store is unchanged, the rollback works.

## One-machine installs (quickstart.sh)

`quickstart.sh` installed k3s, Helm, cert-manager and the box, and keeps
its own state on the machine ([quickstart.md](quickstart.md#3-run-it)
lists where). The way to upgrade is to run the new release's script: it
brings that release's chart, Helm and cert-manager, keeps `values.yaml`
and the build cache, and never upgrades k3s. Everything below is typed
on the machine.

1. **What runs now.** The script's Helm with the cluster's kubeconfig;
   define it once, with the newest Helm the scripts installed:

   ```sh
   H=$(ls -d /usr/local/lib/ci-accelerator/helm-*/ | sort -V | tail -1)helm
   sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml $H list -n ci-accelerator
   ```

   The CHART column is the version; the console names it at the bottom
   of its sidebar too. Kubernetes stays at what the first install put
   there (`sudo k3s kubectl version`); a release that needs a newer one
   says so in its notes.
2. **The runner's token file** from the first install
   ([quickstart.md](quickstart.md#1-put-the-token-in-a-file)). If you no
   longer have it, take it from the Secret:

   ```sh
   install -m 600 /dev/null runner-token
   sudo k3s kubectl -n ci-accelerator get secret gitlab-runner-token -o jsonpath='{.data.runner-token}' | base64 -d > runner-token
   ```

   Giving the script another runner's token would register that runner
   instead.
3. **Download the new release's script and check it:**

   ```sh
   v=0.5.2
   curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v$v/quickstart.sh
   curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v$v/SHA256SUMS
   grep ' quickstart.sh$' SHA256SUMS | sha256sum -c -
   ```

   `quickstart.sh: OK`. The checksums are signed by the release workflow;
   to check that too (cosign 2.6 or newer), before the `sha256sum` line:

   ```sh
   curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v$v/SHA256SUMS.sigstore.json
   cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
     --certificate-identity https://github.com/hoobiboobi/ci-accelerator/.github/workflows/release.yml@refs/tags/v$v \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   ```
4. **Run it** with the same GitLab URL and the token file as at the
   first install:

   ```sh
   sudo bash quickstart.sh --gitlab-url https://gitlab.com/ --runner-token-file ./runner-token
   ```

   It prints what it will do (`k3s: kept as installed`, the new Helm,
   cert-manager and box versions) and asks `Go on? [y/N]`; `--yes`
   answers it. Do not pass `--build-dir`: the cache stays where
   `values.yaml` says. A GitLab token the running install uses stays in
   use; to add one now, pass `--gitlab-token-file`. The run takes a few
   minutes: cert-manager is brought to the version the script pins (its
   pods restart only when that pin moved since your last run; the box's
   certificates and the daemon are untouched), then the box upgrades
   like a Helm install. It
   ends with `Done. ci-accelerator 0.5.2 runs on this machine.` and the
   notes.
5. **If the script warns** `the GitLab token is applied with --set`, add
   to `/var/lib/ci-accelerator/values.yaml`, under the existing
   `control-plane:` key next to `enabled: true`:

   ```yaml
   control-plane:
     enabled: true
     gitlab:
       token:
         existingSecret: gitlab-api-token
   ```

   Otherwise a later `helm upgrade -f values.yaml` by hand drops the
   token, and the console silently stops showing job results and runner
   waits.
6. **Check:**

   ```sh
   sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml $H list -n ci-accelerator     # CHART ci-accelerator-0.5.2
   sudo k3s kubectl -n ci-accelerator get pods                              # all Running
   ```

   The console through the tunnel
   ([quickstart.md](quickstart.md#5-open-the-console)) names the new
   version at the bottom of its sidebar, and a pipeline's next build of
   the same commit comes from the cache again.

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
the checks of later releases. A release that needs a newer Kubernetes
says so in its notes; upgrade k3s with its own
[procedure](https://docs.k3s.io/upgrades/manual), or reinstall with
`--uninstall` (which removes the build cache too).

### Roll back a one-machine install

With `$H`, the script's Helm, as defined in step 1 above (a new shell
needs it again):

```sh
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml $H history ci-accelerator -n ci-accelerator
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml $H rollback ci-accelerator <revision> -n ci-accelerator --wait --timeout 15m
```

The revision is the last `superseded` one. cert-manager and Helm stay as
the newer script left them, which is fine. Do not run an older
`quickstart.sh` to go back: it would install its older chart as a new
revision, not a rollback, and lacks the checks of later releases.

### Apply your own changes, or upgrade only the box

To apply a change you made in `values.yaml`, or to upgrade the box alone
(cert-manager and Helm then stay as they are), with `$H` from step 1:

```sh
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml $H upgrade ci-accelerator \
  oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <version> \
  -n ci-accelerator -f /var/lib/ci-accelerator/values.yaml --wait --timeout 15m
```

`--version` is the version you run now (to apply values) or the new one.
A GitLab token the script applied with `--set` is lost by this command
unless `values.yaml` names it (step 5 above).

## GitOps (Flux, Argo CD)

Your values live in Git, so the upgrade is one commit:

1. Read the release's notes (above).
2. Once, before the first upgrade that rolls the daemons: `spec.timeout:
   15m` on the `HelmRelease` (Flux's default is 5 minutes) and `upgrade:
   { remediation: { retries: 1 } }`, so a timed-out upgrade is retried
   once instead of staying failed; for Argo CD a sync timeout to match
   ([gitops.md](gitops.md) shows both objects).
3. In one commit: the chart version, `ref.tag` of the Flux
   `OCIRepository` or `targetRevision` of the Argo CD `Application`, and,
   when you pin them, the image digests from the release page:

   ```diff
   -  ref: { tag: 0.5.1 }
   +  ref: { tag: 0.5.2 }
   ...
        control-plane:
   -      image: { digest: "sha256:<0.5.1's>" }
   +      image: { digest: "sha256:<0.5.2's>" }
   ```

   Renovate can open this commit for you.
4. Watch it: `flux get helmrelease -n ci-accelerator` or `argocd app get
   ci-accelerator`, then [Check the upgrade](#check-the-upgrade). The
   chart's checks that read the live cluster do not run under Argo CD
   ([gitops.md](gitops.md#argo-cd)), so check the pods yourself.
5. Roll back by reverting the commit. A `helm rollback` by hand is
   undone by the next sync.

## Check the upgrade

```sh
helm list -n ci-accelerator                                   # STATUS deployed, the new CHART version
kubectl -n ci-accelerator get pods -o custom-columns=NAME:.metadata.name,READY:.status.containerStatuses[*].ready,IMAGE:.spec.containers[0].image
```

Every pod ready; the controller, the web and the router pods run
`ghcr.io/hoobiboobi/ci-accelerator:v0.5.2`, the daemons
`moby/buildkit:v0.34.2@sha256:...` (the release's BuildKit). When the
notes said the daemons roll, wait for the rollout:

```sh
kubectl -n ci-accelerator rollout status ds/ci-accelerator-buildkit --timeout 15m
```

Then the console: `kubectl -n ci-accelerator port-forward
svc/ci-accelerator-web 8080`, <http://localhost:8080/>; the version is at
the bottom of the sidebar, and each daemon's page on Nodes names its
BuildKit version. A pipeline's next build of the same commit should come
from the cache again (with a `hostPath` cache also right after a daemon
restart). On the one-machine install, prefix `kubectl` with `sudo k3s`
and use the script's Helm.

## If an upgrade fails

- **The chart refused it** with a message that names the fix (a kept
  digest, values of an older release, a namespace still terminating):
  [troubleshooting.md](troubleshooting.md#the-install-fails) lists them.
  Nothing changed on the cluster; fix the value and run the upgrade again.
- **`--wait` timed out.** The release is `failed`, but the rollout goes
  on: `kubectl -n ci-accelerator get pods`. Pods still starting: wait,
  then run the same upgrade again (Helm sees no change and marks it
  deployed). A pod in `Pending`, `CrashLoopBackOff` or
  `ImagePullBackOff`: `kubectl describe pod` and `kubectl logs` say why;
  roll back, then fix the cause.
- **A daemon crashes on the new configuration.** The rollout stops on
  its first node; the other daemons keep serving. Roll back, then fix
  the value.
- **The release is `pending-upgrade`** (a killed `helm`): roll back to
  the last deployed revision first; another upgrade is refused until
  then.
- **On the one-machine install**, the box's Helm output is in
  `/var/lib/ci-accelerator/install.log`; the steps before it (k3s, Helm,
  cert-manager, the downloads) fail on the terminal, and the same
  command runs again from where it stopped.

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
   daemons' certificate again from the same CA (`scripts/buildkit-certs.sh`
   is in the repository: `git clone --branch v0.5.2
   https://github.com/hoobiboobi/ci-accelerator`; `--router` and
   `--controller` take the release name) (the same `--out`
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
   the controller needs two Secrets from the same CA: its client
   certificate for the daemons (`scripts/buildkit-certs.sh --name
   ci-accelerator-buildkit --no-server --client controller`, named in
   `control-plane.tls.existingSecret`) and its API's server certificate
   (below, named in `control-plane.controller.api.tls.existingSecret`);
   the script prints the `kubectl create secret` command for each:
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
