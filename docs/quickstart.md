# Try it on one machine

`quickstart.sh` installs everything on one clean Linux machine: a
one-node Kubernetes (k3s), cert-manager, and the box (the `ci-accelerator`
chart: a BuildKit daemon, the build router, the console and a GitLab
runner). It is for a first look before a real deployment, not for
production: one node, no high availability, the console reachable only
through an SSH tunnel. This page goes from an empty machine to the
console, then to a first build, a GitLab token, sign-in, changes,
upgrades and removal. The install itself takes five minutes.

## Before you start

- **A clean machine:** Ubuntu 24.04 or Debian 13 (any Linux with systemd
  and cgroup v2 works), amd64 or arm64, 4 CPUs, 8 GB of memory and 40 GB
  of free disk (the script refuses less than 2 CPUs, 4 GB and 20 GB).
  Nothing else on it: the script stops if it finds k3s, kubeadm or a
  running Docker (`--allow-docker` overrides the last one at your own
  risk; Docker's firewall rules can break the pods' network).
- **Outbound HTTPS** from the machine to `github.com`,
  `raw.githubusercontent.com`, `get.helm.sh`, `quay.io`, `ghcr.io`,
  `docker.io`, `registry.gitlab.com` and your GitLab. Nothing needs to
  reach the machine: the runner polls GitLab, and you reach the console
  through SSH. Open only port 22 inbound. With `ufw` active, the script warns and goes on: before answering `Go
  on?`, in another terminal, `sudo ufw allow from 10.42.0.0/16 to any &&
  sudo ufw allow from 10.43.0.0/16 to any` (the pod and service
  networks), or the pods cannot reach the API.
- **A runner in GitLab** and its token. Create it as
  [install.md](install.md#4-create-the-runner-in-gitlab) describes: in
  the project you will build, Settings > CI/CD > Runners > Create project
  runner, tag `ci-accelerator`, no untagged jobs; copy the `glrt-...`
  token GitLab shows once.
- **The Maintainer role** on that project, and `sudo` on the machine.

## 1. Put the token in a file

On the machine, as the user you log in with, in your home directory
(steps 2 and 3 run from the same directory):

```sh
install -m 600 /dev/null runner-token
nano runner-token        # paste the glrt-... token as the only line, save
```

The script reads the token from this file (`--runner-token-file`), so it
never appears on a command line or in the shell history. Keep the file:
every later run of the script, for a GitLab token or an upgrade, asks
for the runner's token again, and GitLab does not show it a second time
(if it is lost: the runner's page in GitLab, **Reset authentication
token**, and the new token into the file).

Without the file the script asks for the token on the terminal and
hides what you type: paste, press Enter.

## 2. Download the script and check it

```sh
curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.6.0/quickstart.sh
curl -fsSL https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.6.0/SHA256SUMS | grep quickstart.sh | sha256sum -c -
```

The second command prints `quickstart.sh: OK`. Anything else (`FAILED`):
download again, do not run it.

## 3. Run it

```sh
sudo bash quickstart.sh --gitlab-url https://gitlab.com/ --runner-token-file ./runner-token
```

`--gitlab-url` is your GitLab's address: `https://gitlab.com/`, or your
own `https://gitlab.example.com/`. The script first checks the machine,
then prints what it is going to install and asks `Go on? [y/N]`. Answer
`y`. Unattended (cloud-init, `ssh host 'sudo bash ...'`), add `--yes`.
What follows, with the time each step takes on a small cloud VM:

```
==> Installing k3s v1.36.5+k3s1          (about a minute)
==> Waiting for Kubernetes
==> Installing Helm v4.3.1
==> Installing cert-manager v1.21.3      (half a minute)
==> Installing ci-accelerator 0.5.2      (one to three minutes: the images)

Done. ci-accelerator 0.5.2 runs on this machine.
```

Then the install notes (the commands of the next steps and the `include:`
block of the first build, hash filled in), the tunnel to the console,
and where the files are. If a step fails, the script says which; fix the
cause and run the same command again: it keeps what it installed and
continues. If the last step, the box, fails or times out, the Helm output
is in `/var/lib/ci-accelerator/install.log`, and `sudo k3s kubectl -n
ci-accelerator get pods` shows which pod is not ready
([troubleshooting.md](troubleshooting.md#the-install-fails)).

What is now on the machine:

| What | Where |
|---|---|
| Your values (written once, never changed by the script) | `/var/lib/ci-accelerator/values.yaml` |
| Helm's output for the box, from the last run | `/var/lib/ci-accelerator/install.log` |
| Helm, one directory per version | `/usr/local/lib/ci-accelerator/helm-<version>/helm` |
| The cluster's kubeconfig (root only) | `/etc/rancher/k3s/k3s.yaml` |
| The build cache (a `hostPath`, so it survives restarts) | `/var/lib/ci-accelerator/buildkit` (or `--build-dir`, first run only) |

`values.yaml` holds the daemon's size, taken from the machine at the
first install (half its CPUs and a quarter of its memory requested, three
quarters of the memory at most), the cache path, the GitLab URL and
`global.console.url: http://localhost:8080` for the job logs' links.

**On this machine, `kubectl` is `sudo k3s kubectl`**, and Helm is the
script's: `sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/lib/ci-accelerator/helm-v4.3.1/helm`. The install notes print
bare `kubectl`: add the prefix.

## 4. Check it

```sh
sudo k3s kubectl -n ci-accelerator get pods
```

Six pods `Running`: `ci-accelerator-buildkit-...` (`2/2`),
`ci-accelerator-controller-...`, `ci-accelerator-gitlab-runner-...`,
`ci-accelerator-router-...` and two `ci-accelerator-web-...` (each
`1/1`). A pod that is not `Running` or not ready: `sudo k3s kubectl -n
ci-accelerator describe pod <name>`, and
[troubleshooting.md](troubleshooting.md#the-install-fails). Then the
runner:

```sh
sudo k3s kubectl -n ci-accelerator logs deploy/ci-accelerator-gitlab-runner | grep -i -E 'registered|valid|error'
```

`Runner registered successfully`; in GitLab, under Settings > CI/CD >
Runners, the runner's row shows a green dot and **Online**. If it says
`is not valid`, the token in the file is wrong: fix the file, run the
script again with the same options, then `sudo k3s kubectl -n
ci-accelerator rollout restart deploy/ci-accelerator-gitlab-runner` (the
runner reads the token at start) and check the log again.

## 5. Open the console

The console is not exposed on the machine's address: no HTTPS, no
domain, no ingress (the script turns k3s's Traefik off). You reach it
through an SSH tunnel. Two terminals:

1. On the machine, and keep it running:

   ```sh
   sudo k3s kubectl -n ci-accelerator port-forward svc/ci-accelerator-web 8080:8080
   ```

2. On your computer, with the user and address you use for SSH:

   ```sh
   ssh -N -L 8080:localhost:8080 <user>@<the machine's address>
   ```

Then open <http://localhost:8080/>. The pages are empty until the first
build. Both commands stop when their terminal closes; start them again
when you need the console. The script prints the tunnel line with the
machine's own hostname, which your computer may not resolve: use the
address you log in with.

## 6. A first build

[first-build.md](first-build.md): a `.gitlab-ci.yml` that includes the
GitLab CI component (one `include:` block, printed by the install notes,
that adds the `build-image` job), the tag you gave the runner, and what
the first and the second run look like. On this machine, prefix its `kubectl` commands with `sudo k3s`,
and the console's port-forward is the tunnel above. Every job's log links
to its page in the console at `http://localhost:8080/...`; the links work
while the tunnel is up.

## 7. Add a GitLab token

With a token the console also shows each job's result and how long jobs
waited for a runner, and settles "files or node" for rebuilds by
comparing commits in GitLab.

1. In GitLab, select your avatar > **Edit profile** > **Access** >
   **Personal access tokens** > **Generate token** (on some versions
   **Add new token**): a name, an expiry date, and the scope `read_api`.
   Copy the token (`glpat-...`); GitLab shows it once. A group or project
   access token (Settings > Access tokens, role Reporter) works the same
   way.
2. On the machine, put it into a file like the runner's token:
   `install -m 600 /dev/null gitlab-token && nano gitlab-token`.
3. Run the script again with both files:

   ```sh
   sudo bash quickstart.sh --gitlab-url https://gitlab.com/ --runner-token-file ./runner-token --gitlab-token-file ./gitlab-token
   ```

   It keeps `values.yaml`, writes the token into the Secret
   `gitlab-api-token` and applies it. It also warns that the token is applied with `--set`: your own `helm
   upgrade -f values.yaml` (step 9) would drop it. So make the
   `control-plane:` block of `/var/lib/ci-accelerator/values.yaml` read
   as follows (the first two lines are there already; add the three
   under them):

   ```yaml
   control-plane:
     enabled: true
     gitlab:
       token:
         existingSecret: gitlab-api-token
   ```

The console's job pages now show results and waits. The token expires on
the date you chose: make a new one then, put it into the file, run the
same command again and restart the controller, which reads the token
only at start: `sudo k3s kubectl -n ci-accelerator rollout restart
deploy/ci-accelerator-controller`.

## 8. Sign-in through GitLab

Sign-in replaces the shared token: each person signs in with their GitLab
account and sees the projects they may read. On this machine it works
for the person using the tunnel only, because the console's address is
`http://localhost:8080` (plain `http` is accepted on `localhost` alone).
For a team, use a cluster install behind HTTPS
([console.md](console.md#setting-up-the-console)).

1. In GitLab, select your avatar > **Edit profile** > **Access** >
   **Applications** > **Add new application**. Name: anything. Redirect
   URI: `http://localhost:8080/auth/callback/<id>`, where `<id>` is the
   six characters the install notes printed after `redirect URI is` (or,
   once step 7 gave the console a token, after `redirect URI would be`)
   (print them again: `sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml
   /usr/local/lib/ci-accelerator/helm-v4.3.1/helm get notes ci-accelerator
   -n ci-accelerator`; for `https://gitlab.com/` it is `993196`). Tick
   **Confidential**. Scopes: `openid` and `read_api`. Save. GitLab shows
   the **Application ID** and the **Secret** (`gloas-...`); copy both now.
2. Your GitLab user id: your profile page shows it under your name
   (`User ID: 1234567`).
3. On the machine, the two Secrets:

   ```sh
   sudo k3s kubectl -n ci-accelerator create secret generic console-gitlab-oauth \
     --from-literal=client-id=<Application ID> --from-literal=client-secret=gloas-...
   sudo k3s kubectl -n ci-accelerator create secret generic console-session \
     --from-literal=key=$(openssl rand -base64 32)
   ```

4. In `/var/lib/ci-accelerator/values.yaml`, make the `control-plane:`
   block this (a shared token and sign-in exclude each other: remove
   `gitlab: token:` if step 7 put it there):

   ```yaml
   control-plane:
     enabled: true
     signIn:
       enabled: true
       sessionKey: { existingSecret: console-session }
       gitlab:
         - existingSecret: console-gitlab-oauth
           admins: ["id:1234567"]      # your user id: admins see every project
   ```

5. Apply it with the `helm upgrade` command of step 9, then open <http://localhost:8080/> through the
   tunnel: GitLab asks you to authorize the application, and the console
   shows your projects. If GitLab says the redirect URI does not match,
   compare it with the notes' character by character.

## 9. Change the values

Edit `/var/lib/ci-accelerator/values.yaml`, then apply it with the
script's Helm and the version you run now (`--version` stays the same;
the newest `helm-*` directory under `/usr/local/lib/ci-accelerator/` is
the Helm to use):

```sh
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/lib/ci-accelerator/helm-v4.3.1/helm upgrade ci-accelerator \
  oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.6.0 \
  -n ci-accelerator -f /var/lib/ci-accelerator/values.yaml --wait --timeout 15m
```

The script prints this command, with the right versions, at the end of
every run. A change under `buildkit:` restarts the daemon (its cache on
the `hostPath` stays); the console keeps its history.

## 10. Upgrade

Run the next release's `quickstart.sh` the same way as the first time
(download, check, the same `--gitlab-url` and `--runner-token-file`). It
brings that release's chart, Helm and cert-manager, keeps `values.yaml`
and the cache, and never upgrades k3s. Step by step, with the checks and
the rollback: [upgrading.md](upgrading.md#one-machine-installs-quickstartsh).

## 11. Remove it

```sh
sudo bash quickstart.sh --uninstall
```

It asks `Remove k3s, everything running in it, /var/lib/ci-accelerator and
the build cache ...? [y/N]` (`--yes` to skip), then removes k3s with
everything that ran in it, the box's files, Helm and the build cache. A
cache directory that existed before the first install (`--build-dir` on
a disk of your own) is left in place. In GitLab, delete the runner
(Settings > CI/CD > Runners) and the `include:` from `.gitlab-ci.yml`
yourself.

## What this install does not do

- No HTTPS, no domain, no ingress: the console lives behind the SSH
  tunnel, and sign-in works for the person with the tunnel.
- One node: the machine is the build node, and the daemon, the runner,
  the console and the CI jobs share it.
- No high availability, no upgrades of k3s.

For a team, install on a cluster: [install.md](install.md).
