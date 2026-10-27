# 0044. The control plane: a controller and a web

Date: 2026-10-26 (names settled 2026-10-27)

## Context

The console was one pod that collected the daemons' history and the job
pods, owned SQLite and the sign-in sessions, held every GitLab token, and
served the pages. Its logic had grown well past a web page, and the next
features (routing builds to the daemon with a warm cache, the cache each
project holds, clearing it) need a component that keeps running while the
pages are rolled, and a place for their state that is not the web server.
Every pod restart also cut every open page.

## Decision

- One binary and image, `ci-accelerator`, with roles as subcommands:
  `controller` (the store, the collector, the job-pod watch, the sessions,
  every GitLab token), `web` (the console's pages; keeps nothing),
  `disk-probe`, and `all` (the default: controller and web in one process,
  for development). The router will be another role of the same image.
- One chart, `control-plane`, with a Deployment per role named after it:
  `<release>-controller` (one pod, `Recreate`, the volume
  `<release>-controller-data`, the API's Service) and `<release>-web` (two
  pods, rolling, the Service users open). Values follow the same split:
  shared settings at the top, `controller.*` and `web.*` for each role's
  pods. Labels: `app.kubernetes.io/component` and
  `ci-accelerator.io/component` are `controller` or `web`; the daemons'
  NetworkPolicy admits the controller only. "The console" stays the name
  of what people use: the pages and their address
  (`global.console.url`).
- The web reads the controller over an internal HTTP API (Service
  `<release>-controller`, HTTPS): the pages' reads by name from an
  explicit list, the few writes they cause (GitLab's answers, settled
  reasons, sessions), a change stream for live pages, and the
  controller's setup. It is not a public API: both sides come from one
  release. Sign-in moves whole into the controller (the key that seals the
  sign-in state and the tokens); the web only sets cookies.
- The API's certificate is issued from the daemons' CA with server
  authentication only. The daemons require client certificates (the client
  authentication usage), so this one can never drive them; no second CA.
- The web proves who it is with a ServiceAccount token projected for the
  audience `ci-accelerator-controller`; the controller checks it with a
  TokenReview (cached for minutes). No secret to generate or rotate,
  nothing that drifts under GitOps; a static token from a file outside a
  cluster. Rejected: client certificates from a second CA (three more
  certificates per installation without cert-manager, no reload).
- The controller's NetworkPolicy admits the web's pods only. A page whose
  controller does not answer is a 502; the web stays ready.
- No in-place upgrade from 0.2.x (the author's choice: no installations to
  carry over yet). Keeping the console's names would have left a
  Deployment called console running the controller and a Service called
  console pointing at the web. Both charts refuse to install over a
  release that still runs the 0.2.x console, and the box refuses values
  under `console:`, each with the way forward: uninstall, wait for the
  pods, install again (the hostPath cache stays; the old console's volume
  stays until deleted). An exception to [ADR-0027](0027-upgrades-and-branches.md),
  like [ADR-0039](0039-no-bundled-observability.md); from 0.3.0 on releases
  upgrade in place again.

## Consequences

- The chart needs a ClusterRole (TokenReview only) and its binding.
- Without cert-manager the API needs a provided certificate
  (`controller.api.tls.existingSecret`; `scripts/buildkit-certs.sh
  --controller <release>`).
- The pages are tested twice: with the controller in the process and
  through its API (`make go-test`, `make ui-test`).
- 0.2.x installations start a new console history; new registry packages
  (`charts/control-plane`, the `ci-accelerator` image) and a new Artifact
  Hub repository; the old packages stay published for 0.2.x.
- The e2e upgrade scenario checks the refusal and the reinstall from
  0.2.x, and upgrades in place from 0.3.0 on.
