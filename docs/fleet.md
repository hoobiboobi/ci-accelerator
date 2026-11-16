# A fleet: several boxes under one console

> Experimental. The values under `fleet`, the management API under
> `/api/v1/fleet/` and the protocol between a hub and its members may
> change between releases without the usual deprecation cycle.

One box serves one cluster. With several clusters, install a **hub** once
and let each box join it as a **member**: the hub is one console for all
of them, with one address and one sign-in.

- Each member keeps its own builds, daemons, router and controller. The
  hub asks a member while you look at its pages; nothing is copied.
- A member dials **out** to the hub's console address (HTTPS, a WebSocket
  through your Ingress). It needs no inbound address: it works behind NAT.
- Hub and member prove themselves with keys each side pins (Ed25519 or
  ECDSA P-256/P-384); no certificate authority is involved.
- A member's own web is not installed: its console is the hub's
  `/m/<name>/`.

## 1. Keys

Each member and the hub need a key pair. Standard tools make them:

```sh
openssl genpkey -algorithm ed25519 -out hub.key
openssl pkey -in hub.key -pubout -out hub.pub
```

(`ci-accelerator fleet-keygen hub.key` writes `hub.key` and
`hub.key.pub`, and refuses to write over either.) The private key goes
into a Secret; the public key is not a secret.

## 2. The hub

The hub is a release of the control-plane chart, without daemons, in any
cluster your browsers and your members' clusters can reach, always a new
release in a namespace of its own: the chart refuses to turn a box's
release into a hub or back, and the box chart refuses
`control-plane.fleet.role=hub`. Its console address (`global.console.url`,
or `signIn.url`) is required and is the root of its host (no path).

```sh
openssl rand -hex 24 > hub-token          # the bootstrap API token: section 3 uses it
openssl rand -base64 32 > hub-secrets.key # the secrets key: keep it apart from the backups

kubectl create namespace fleet
kubectl -n fleet create secret generic hub-key --from-literal=key="$(cat hub.key)"
kubectl -n fleet create secret generic hub-token --from-literal=token="$(cat hub-token)"
kubectl -n fleet create secret generic hub-secrets --from-literal=key="$(cat hub-secrets.key)"

helm upgrade --install hub oci://ghcr.io/hoobiboobi/charts/control-plane -n fleet \
  --set fleet.role=hub \
  --set fleet.key.existingSecret=hub-key \
  --set fleet.apiToken.existingSecret=hub-token \
  --set fleet.secretsKey.existingSecret=hub-secrets \
  --set global.console.url=https://ci.example.com
```

- `fleet.apiToken`: a bootstrap token for the management API, role owner,
  read at every use: valid while its Secret holds it (a change reaches the
  hub's pod within about a minute, no restart needed). Terraform or a
  script can start with it and issue its own token; then delete the
  Secret, or unset `fleet.apiToken.existingSecret` and upgrade. A hub
  needs this token or sign-in: the chart refuses one with neither. The
  API does not list the bootstrap token and cannot revoke it (409): change
  or remove it in its Secret.
- `fleet.secretsKey`: the key the hub seals the secrets it keeps for its
  clusters with (runner tokens, GitLab tokens, S3 keys entered in "Add a
  cluster"). Make it random (`openssl rand -base64 32`); it is not
  stretched. Keep it where it survives the hub: the backups do not open
  without it. Changing it makes every kept secret unreadable: a cluster's
  settings then mark those secrets "stale", the others are still
  delivered, and each stale one must be entered again.
- Without `fleet.secretsKey` the hub keeps no secret and asks for none.
  Create in each box's namespace, yourself, the Secrets its values name:
  `fleet-runner-token` (keys `runner-token`, and
  `runner-registration-token` empty), `fleet-gitlab-token` (`token`) if
  you give the box a GitLab token, `fleet-s3-cache` (`accesskey`,
  `secretkey`) with S3.
- `fleet.hub.caCert`: the CA of the hub's address when its certificate is
  not from a public CA; the hub writes it into the values it generates.

Put your Ingress in front of the Service `hub-web` (port 8080), on one
host, with TLS. The members connect to `/fleet/v1/connect` (and trade join
tokens at `/fleet/v1/join`) on the same host, so the Ingress must pass
WebSockets: Traefik does by default; the NGINX Ingress Controller
(nginx.org) needs the annotation `nginx.org/websocket-services: hub-web`.
Idle connections are kept alive every 15 seconds, well inside the usual
60-second proxy timeouts.

**Sign-in** is set up as on a box (the GitLab OAuth application:
docs/console.md, "Level 3"), with these differences on a hub:

```yaml
signIn:
  enabled: true
  sessionKey:
    existingSecret: hub-session       # openssl rand -base64 32
  gitlab:
    - url: https://gitlab.example.com/ # required on a hub
      existingSecret: hub-oauth       # client-id and client-secret
      owners: ["id:1234567"]          # GitLab user ids; not admins
```

- `signIn.gitlab[].owners` names the first owners (`admins` is refused).
  They are owners always and are changed only in the values.
- The hub needs the GitLab URL: give each `signIn.gitlab` entry its `url`,
  or set `gitlab.urls` (a hub has no runners to take it from).
- The redirect URI is `<console address>/auth/callback/<id>`; the hub's
  install notes print it (`helm get notes hub -n fleet | grep
  auth/callback`), and so does its log (`kubectl -n fleet logs
  deploy/hub-web | grep redirect_uri`).

Without sign-in the hub shows every cluster's pages, and its Members,
Access and Audit pages, to whoever reaches it, and changes come only
through the API with a token. Set `global.console.url` so the hub answers
only its own host name; otherwise a page on another site can read them
through DNS rebinding.

`networkPolicy.enabled` gives the hub's pages the policy the web has on a
box (docs/console.md): when the hub shares a cluster with a box, add that
box's jobs namespace to `networkPolicy.deniedNamespaces`.

## 3. Add a cluster

**From the console** (operators and owners): Members, **Add a cluster**.
The form asks for the cluster's name and its settings from a short,
curated list: how it joins (a join token, or a public key you made), the
build cache disk (a path on the build nodes and the cache limit), the
runner (GitLab, its token, concurrent jobs, a GitLab read token for job
times) and an optional S3 cache for the runner. The hub then shows:

- the commands that install the box, with a **join token**: it works once,
  for this name only, for an hour by default (at most 7 days); the hub
  keeps only its hash and shows it this once;
- the box's values file, generated from the settings (save it as
  `<name>.values.yaml`).

The generated commands install release `ci-accelerator` in namespace
`ci-accelerator` with the hub's chart version; for a box installed under
other names, or to keep its version, change the `helm` line. The box's
prerequisites (docs/install.md steps 1 to 3: the build nodes' label and
taint, cert-manager, the namespace with its Pod Security label) come
first.

Tokens and keys entered in the form are never in the values: the hub keeps
them sealed and sends them to the cluster over its link once it is
connected; the box writes them into the Secrets its values name
(`fleet-runner-token`, `fleet-gitlab-token`, `fleet-s3-cache`). The
runner starts once its Secret is filled, shortly after the cluster
connected. The forms and the API never show a kept secret again.

A cluster's page (Members, **Settings**) shows its settings, which secrets
are kept and when they were delivered, its join tokens, and:
**Change settings** (new values for `helm upgrade`; changed secrets go to
the cluster at once), **New join token** (while it has not joined),
**Remove from the fleet**.

**Through the API** (`/api/v1/fleet/`, a token or a session; machines and
Terraform). Either register the cluster by its public key:

```sh
TOKEN=$(cat hub-token)
H="Authorization: Bearer $TOKEN"
openssl genpkey -algorithm ed25519 -out prod-eu.key
openssl pkey -in prod-eu.key -pubout -out prod-eu.pub
curl -fsS -X POST https://ci.example.com/api/v1/fleet/members -H "$H" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg k "$(cat prod-eu.pub)" '{name: "prod-eu", public_key: $k}')"
```

or issue a join token for the name (the answer's `secret`, shown once):

```sh
curl -fsS -X POST https://ci.example.com/api/v1/fleet/members/prod-eu/join-tokens -H "$H" \
  -H 'Content-Type: application/json' -d '{"expires_in_minutes": 60}'
```

Then the settings (secrets write-only) and the generated values:

```sh
curl -fsS -X PUT https://ci.example.com/api/v1/fleet/members/prod-eu/settings -H "$H" -H 'Content-Type: application/json' \
  -d '{"values": {"gitlab_url": "https://gitlab.example.com/", "runner_token": "glrt-...", "storage": "hostPath", "host_path": "/var/mnt/nvme/buildkit"}}'
curl -fsS https://ci.example.com/api/v1/fleet/members/prod-eu/values -H "$H" > prod-eu.values.yaml
```

A cluster registered by key gets values that name the key's Secret
`fleet-key` (`control-plane.fleet.key.existingSecret`) whatever its
settings say; one that joins with a token gets `fleet-join`
(`control-plane.fleet.join.existingSecret`). `joins`, `token_hours` and
`public_key` in the settings are what the console's form uses; a
`public_key` other than the registered one is refused. Create the box's
Secret before installing it:

```sh
kubectl create namespace ci-accelerator
# registered by key:
kubectl -n ci-accelerator create secret generic fleet-key --from-literal=key="$(cat prod-eu.key)"
# or with a join token:
kubectl -n ci-accelerator create secret generic fleet-join --from-literal=token='cia_join_...'
```

A kept optional secret is dropped with `PUT .../settings` and
`{"forget": ["gitlab_token"]}`. Names are lower-case letters, digits and
dashes (at most 40; `new` is reserved).
docs/api/openapi.yaml describes every call.

**Removing a cluster** (the console, or `DELETE
/api/v1/fleet/members/<name>`) closes its link at once, revokes its
unused join tokens and forgets its settings and kept secrets; a cluster
later registered under the name starts without them. Its record is kept
for `fleet.keepRemovedDays` (the audit and the Removed list show it); its
name can be registered again at once, and its record then starts anew.
A removed cluster's key can be registered again, under its name or
another one (a box that joined with a token keeps its key in the Secret
`<release>-controller-fleet-key` and joins with it again). A Secret the
hub wrote into the cluster stays there until you delete it
(`kubectl -n <namespace> delete secret fleet-runner-token
fleet-gitlab-token fleet-s3-cache`); the same holds for a secret dropped
at the hub (S3 turned off, or `forget`).

## 4. Join a box

Install or upgrade the box with the generated values (or set them by
hand):

```yaml
control-plane:
  fleet:
    role: member
    name: prod-eu
    join:
      existingSecret: fleet-join     # a join token; or key.existingSecret: a private key registered by its public key
    hub:
      url: https://ci.example.com
      publicKeys: |
        -----BEGIN PUBLIC KEY-----
        ...
        -----END PUBLIC KEY-----
      caCert: ""                      # the CA of the hub's address, when it is not a public one
global:
  console:
    url: https://ci.example.com      # job logs link to the hub
```

- `fleet.hub.url` is https (plain http only to an address inside the
  cluster, `*.svc`, when joining with a token), at the root of the hub's
  host, without a user or password. `fleet.key` and `fleet.join` exclude
  each other.
- With a join token the controller makes the member's key, keeps it first
  in the Secret `<release>-controller-fleet-key` of its namespace, then
  trades the token. The chart creates that Secret empty, with the Secrets
  the hub delivers (`fleet-runner-token`, `fleet-gitlab-token`,
  `fleet-s3-cache`); the controller may read and change only those, and
  create no Secret. A restart in between loses nothing: the key is kept,
  and trading again for a key the hub knows under that name is done
  already. The token's Secret may go once the cluster joined.
- The token is sent only over https (http only to localhost or a Service
  of the cluster, `*.svc`). If the hub is away, the controller keeps
  trading the token until the hub answers (and reads a new token put into
  the Secret); the box collects its builds meanwhile.
- A box's `signIn` values are ignored once it is a member: people sign in
  at the hub, and the box's own sessions end.
- If the hub keeps no secrets (no `fleet.secretsKey`) and you create the
  delivered Secrets yourself, create them before the install (the chart
  then leaves them as they are), or fill the empty ones with `kubectl
  create secret generic <name> ... --dry-run=client -o yaml | kubectl
  apply -f -`.
- A fleet member runs in Kubernetes: it writes the Secrets its hub
  delivers there. A delivery sets only the keys the hub keeps and leaves
  other keys of the Secret alone.

The cluster shows on the hub's Members page; its own web goes away. The
job links the runners print (`<console>/jobs/<gitlab>/<job>`) open at the
hub, which finds the cluster that ran the job. If people still have links
to the box's old address, point that host in your Ingress at the hub.

A member whose key the hub does not know (not registered, removed, or
registered under another name) logs `the hub refused the key SHA256:...
for "<name>"` and tries again, at most 5 minutes apart; register the
key at the hub or join with a token. On the box, the link's state
appears only in the controller's log.

### What a member trusts its hub with

A member serves its hub what it serves its own web: over the link the hub
reads every project's builds and step output on that cluster, and asks
GitLab with the member's token (the installation's, or a person's at
sign-in) for job logs and commit comparisons of any project that token
can read, not only the jobs that ran there (at most 16 GitLab reads at
once). It also writes the Secrets `fleet-runner-token`,
`fleet-gitlab-token` and `fleet-s3-cache` (only the keys the values
read), so it can point the cluster's runner at another runner
registration. The hub never sends a person's session to the cluster,
writes nothing into the cluster's build history, and cannot use the
cluster's sign-in. Join a box only to a hub run by people you would give
those, and give the box a GitLab token scoped to the projects that build
there.

## 5. Who may do what

| Role | May |
|---|---|
| viewer (everyone signed in, no binding) | each cluster's console, with the projects GitLab lets them read |
| operator | also every project, Members, adding, changing and removing clusters, join tokens, the audit |
| owner | also roles and API tokens |

A viewer also sees the names of the fleet's clusters (in the cluster
switcher), and a cluster's build nodes (its Nodes pages and search) only
when they may read at least one project of that cluster. The hub decides
itself what a person may read; a cluster is trusted for what its own
pages show and for which jobs ran in it. When several clusters claim the
same job, the hub lets the person choose.

An operator can register a key for a cluster (adding it by its key, or
with a join token), and the hub sends a cluster's kept secrets to the key
registered under its name: give the operator role only to people you
would trust with the runner tokens, S3 keys and GitLab tokens entered for
your clusters.

**Access** (owners change, operators read): people with a role, by GitLab
user id (the number on the profile page; usernames are not taken, GitLab
gives a freed one to another account), and **API tokens**: a name, a role
and an expiry (90 days unless you choose another, or never), the secret
shown once, `cia_pat_...`. `PUT`/`DELETE
/api/v1/fleet/role-bindings/gitlab/<host>/<id>` and `/api/v1/fleet/tokens`
do the same.

- A person is written exactly `gitlab:<host>/<user id>`: the host as in
  the GitLab URL people sign in with (lower case, with a port only when
  that URL has one), no spaces.
- The owners from the values are listed as locked. A person bound in the
  console and then named an owner in the values becomes a values owner;
  when the values drop them, they have no role (a viewer), not the role
  they had before: grant it again.
- The hub's only owner cannot remove themselves or take a lesser role in
  the console or the API; make someone else an owner first. `ci-accelerator
  hub grant` inside the hub's pod still can.

**Audit**: who changed what (a person, a token, the values, a command in
the hub's pod), filtered by who and by target, kept a year. Secrets are
recorded without their values.

## 6. Backups and a lost owner

The hub's configuration is its `fleet.db` (members, settings and sealed
secrets, roles, token hashes, the audit). The commands run in the hub's
pod by their path, `/ci-accelerator`. The image has no shell and no
`tar`, so a file leaves or enters the pod through a debug container that
mounts the hub's volume as the hub's user (`kubectl debug --custom` needs
kubectl 1.31 or later; the container stays listed in the pod until it
restarts, so give each one a new name):

```sh
POD=$(kubectl -n fleet get pod -l app.kubernetes.io/instance=hub,app.kubernetes.io/component=hub \
  -o jsonpath='{.items[0].metadata.name}')
cat > files.json <<'EOF'
{"securityContext": {"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
  "allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]},
  "seccompProfile": {"type": "RuntimeDefault"}},
 "volumeMounts": [{"name": "data", "mountPath": "/data"}]}
EOF
C=files-$(date +%s)
kubectl -n fleet debug "$POD" -c "$C" --image=busybox:1.37 --profile=restricted --custom=files.json -- sleep 3600
until kubectl -n fleet get pod "$POD" \
  -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"$C\")].state.running.startedAt}" | grep -q .; do sleep 2; done

# backup: written in the pod, copied out, deleted in the pod
kubectl -n fleet exec "$POD" -c hub -- /ci-accelerator hub backup /data/backup.db
kubectl -n fleet cp -c "$C" "$POD:/data/backup.db" ./hub-backup-$(date +%F).db
kubectl -n fleet exec "$POD" -c "$C" -- rm /data/backup.db

# restore: copied in, staged, applied at the next start
kubectl -n fleet cp -c "$C" ./hub-backup-2026-11-15.db "$POD:/data/restore.db"
kubectl -n fleet exec "$POD" -c hub -- /ci-accelerator hub restore /data/restore.db
kubectl -n fleet exec "$POD" -c "$C" -- rm /data/restore.db
kubectl -n fleet rollout restart deploy/hub-web

# nobody left who may sign in as owner
kubectl -n fleet exec deploy/hub-web -c hub -- /ci-accelerator hub grant owner gitlab:gitlab.example.com/1234567
```

- The backup file is readable by the hub's user only. `hub backup`
  refuses a file that exists: delete it in the pod once copied out, as
  above. Once a restore is checked, delete the `fleet.db.before-restore-*`
  file the hub's start left in `/data` too (through a debug container in
  the new pod). A backup is trusted input: store it like the hub's
  secrets key. A file holding triggers or views is refused.
- A restore brings the whole registry back as it was when the backup was
  taken: clusters removed since are members again and their keys
  connect, roles taken away since are back, and each cluster's settings
  and secrets are those of the backup, delivered at its first connection
  after the start, over what the cluster holds (a runner token or S3 key
  rotated since is overwritten). Clusters added since must join again.
  Only API and join tokens are voided, because the backup would bring
  back tokens revoked since it was taken; the bootstrap token from the
  values is not in the backup and stays valid while its Secret holds it.
  After a restore, review Members and Access, and set again any secret
  changed since the backup.
- `hub grant` is authorised by Kubernetes (who may exec into the pod) and
  recorded as `local`; the hub sees it within 15 seconds.

## 7. Upgrades, keys, leaving

**Upgrade the hub first.** A hub accepts members that speak its link
protocol or the one before it; a member newer than its hub is refused
(409, with both protocol numbers in the member's log) and tries again,
at most 5 minutes apart, until the hub is upgraded.

**Replacing the hub's key** (it is read at the hub's start):

1. Make the new pair. Add the new public key to every member's
   `fleet.hub.publicKeys`, after the old one (the value takes several PEM
   blocks), and upgrade each member.
2. Put the new private key into the hub's key Secret and restart the hub
   (`kubectl -n fleet rollout restart deploy/hub-web`). The values the hub
   generates from then on carry the new key. A member that does not have
   it yet is refused by its own pin, logs it, and tries again, at most 5
   minutes apart.
3. Drop the old public key from the members' values.

**Replacing a member's key** is removing the cluster and adding it again
(the settings and kept secrets go with the removal: enter them again).
For a box that joined with a token, delete its Secret
`<release>-controller-fleet-key` before the upgrade with the new join
token: the chart makes it again, empty, and the controller a new key. A
cluster's name cannot change the same way: remove it and add it under the
new name.

**Leaving the fleet:** remove the cluster at the hub, then upgrade the box
without `control-plane.fleet` (and with its own `global.console.url` and
`signIn` values again): its web comes back. The chart keeps the Secrets
`fleet-runner-token`, `fleet-gitlab-token`, `fleet-s3-cache` and
`<release>-controller-fleet-key`; point `gitlab-runner.runners.secret`
back at your own runner Secret, then delete them.

## What you see

- **Members** (operators and owners): every cluster, connected or not,
  its version, last contact, how it joined and its key's fingerprint;
  clusters added and waiting to join; removed ones while their record is
  kept. When a cluster's store is replaced (its controller's volume was
  replaced), the Members page says so; addresses of the old store's builds
  (`/m/<name>/builds/<id>`) then open the new store's builds of the same
  number.
- **A cluster** (`/m/<name>/`): the console you know, with a cluster
  switcher above the project switcher. A removed cluster's addresses
  answer 410, an unknown name's 404.
- **A pipeline whose jobs ran in several clusters**: its jobs grouped by
  cluster, each linking to that cluster's pages.
- **Access** and **Audit**, as above.

## Limits of this stage

- A member that is not connected shows as not connected. A page of it
  that you opened before is shown as it was then, dated, for at most a
  day, and only to the same person with the same role; other pages wait
  until it reconnects (its builds are kept in the member). Those pages
  are kept in the hub's memory (at most 2000 pages, 64 MiB) and are gone
  when the hub restarts; they are shown without asking GitLab again.
- GitLab is read by each member with its own token
  (`control-plane.gitlab.token`), not with the person's token at the hub.
  A cluster's pages at the hub ask GitLab about the 500 most recently
  built projects of that cluster per page; older projects do not show
  there.
- Lists are per cluster: there is no merged list of builds or pipelines
  across clusters, and Insights and Nodes are per cluster.
- A box that joins a fleet has no web of its own, and the hub does not
  serve the box's read-only JSON API (`/api/v1/builds`, its one operation) per
  cluster: scripts that read a box's API stop working when it joins.
- The settings list is short on purpose (one row and its test per new
  setting); everything else is set in the values by hand.
- Roles are given to people one by one, not to GitLab groups.
- The install page, a new join token and a new API token show their
  secret once, as the answer to the form. Reloading such a page repeats
  the action: a reload of the New join token page issues another token
  and revokes the one shown.
- `/fleet/v1/join` answers whoever reaches the hub and is not
  rate-limited; refused tokens are logged at the hub.
- The management API (alpha) has no update of a member (a key change is
  removing and registering again), no labels, no builds per member, and
  unpaged lists of members, tokens, bindings and join tokens (the audit
  pages by `before`); a member's spec and status are one object.
- The hub prepares its pages once for every cluster: a connected member
  costs it its link and what its open pages hold. A job's address at the
  hub waits a moment for the other clusters once the one that ran the job
  answered; a pipeline's page waits up to 5 seconds for a cluster that
  does not answer, then names it.
