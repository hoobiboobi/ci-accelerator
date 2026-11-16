# 0064. Fleet administration: roles and tokens in the hub, join tokens, secrets delivered over the link

Date: 2026-11-15

## Context

ADR-0063 left a fleet managed by the owners in the values and one
bootstrap token. Organisations want to add clusters from the console, to
give people roles without a release, to drive the hub from Terraform, and
not to hand around private keys and runner tokens.

## Decision

- Roles viewer, operator and owner are bindings in the hub's store,
  checked by the name of a permission. `signIn.gitlab[].owners` are owners
  of origin "values", locked in the console and the API; a person bound
  by id, never by username. Operators and owners see every project.
- API tokens are issued by owners with a role and an expiry chosen at
  issue (90 days by default, "never" only when chosen), kept as a hash,
  shown once. The values' bootstrap token is read from its Secret at
  every use: valid while the Secret holds it.
- "Add a cluster" takes a short, versioned list of settings, one entry and
  its check per setting, and generates the box's values and install
  commands; installing stays Helm. A cluster joins with a join token
  (hashed, single use, one name, an hour by default, a week at most) or
  with a public key. A joining box makes its key, keeps it in a Secret of
  its namespace before it trades the token, and trades until the hub
  answers, again at every start while the token is there: the hub answers
  a known key as done. The token goes only over https (plain http only to
  an address inside the cluster).
- Secrets entered at the hub are sealed (AES-256-GCM, a key from a
  user-made Secret), never written into values, pages, answers or the
  audit, and delivered over the member's link; the member writes only the
  Secrets its generated values name. The controller reads its GitLab token
  file at every use, so a token delivered after its start counts.
- Backups are a copy of the store taken in the hub's pod; a restore is
  applied at the next start and voids every API and join token it holds
  (the bootstrap token is not in the store). A command in the pod grants
  a role when no owner is left.

## Consequences

- One API serves the console and a later Terraform provider (alpha).
- The hub holds the clusters' runner tokens and S3 keys: its secrets key
  and backups need the care of those credentials.
- A member writes Secrets in its own namespace: the chart creates them
  empty, and its controller may read and change only those (the three it
  is delivered, and its key's when it joins with a token), and create
  none.
- A member trusts its hub like its own web: the hub reads its builds,
  asks GitLab with its token, and writes its runner's Secrets.
