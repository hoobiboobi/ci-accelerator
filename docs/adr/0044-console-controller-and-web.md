# 0044. The console as a controller and a web

Date: 2026-10-26

## Context

The console was one pod that collected the daemons' history and the job
pods, owned SQLite and the sign-in sessions, held every GitLab token, and
served the pages. Its logic had grown well past a web page, and the next
features (routing builds to the daemon with a warm cache, the cache each
project holds, clearing it) need a component that keeps running while the
pages are rolled, and a place for their state that is not the web server.
Every pod restart also cut every open page.

## Decision

- One binary and image (`console`), roles as subcommands: `controller`
  (the store, the collector, the job-pod watch, the sessions, every GitLab
  token), `web` (the pages; keeps nothing), `disk-probe`, and the default
  `console` with controller and web in one process (development).
- The chart keeps its name and the Deployment `<release>-console` (its
  volume, its `Recreate` strategy) for the controller; the web is the new
  Deployment `<release>-console-web` (two pods, rolling). The Service
  `<release>-console` selects the web, so addresses users have keep
  working; renaming the chart or the image would have bought nothing and
  cost new Artifact Hub and registry entries.
- The web reads the controller over an internal HTTP API
  (`<release>-console-api`, HTTPS): the pages' reads by name from an
  explicit list, the few writes they cause (GitLab's answers, settled
  reasons, sessions), a change stream for live pages, and the
  controller's setup. It is not a public API: both sides come from one
  release. Sign-in moves whole into the controller (the key that seals the
  sign-in state and the tokens); the web only sets cookies.
- The API's certificate is issued from the daemons' CA with server
  authentication only. The daemons require client certificates (the client
  authentication usage), so this one can never drive them; no second CA.
- The web proves who it is with a ServiceAccount token projected for the
  controller's audience; the controller checks it with a TokenReview
  (cached for minutes). No secret to generate or rotate, nothing that
  drifts under GitOps; a static token from a file outside a cluster.
  Rejected: client certificates from a second CA (three more certificates
  per installation without cert-manager, no reload).
- The controller's NetworkPolicy admits the web's pods only. A page whose
  controller does not answer is a 502; the web stays ready.

## Consequences

- The chart needs a ClusterRole (TokenReview only) and its binding.
- Without cert-manager the API needs a provided certificate
  (`api.tls.existingSecret`; `scripts/buildkit-certs.sh --console-api`).
- The pages are tested twice: with the controller in the process and
  through its API (`make go-test`, `make ui-test`).
- Values of 0.2.x keep their meaning; the upgrade restarts the console
  once and no daemon.
