# 0031. Console packaging: one pod, its own certificate, the runner's URL once

Date: 2026-10-20

## Context

The console (ADR-0029, ADR-0030) has to run next to the daemons:
- with access to their history over mutual TLS;
- with read access to the CI job pods;
- with a SQLite store that allows one writer.

In the box it must not ask for anything the box already knows, the GitLab URL above all. Its trace-id prefix is a hash of the runner's `gitlabUrl` string, so the console has to use exactly that string.

## Decision

- **Its own chart:** `charts/console`, standalone, and a subchart of the box under `console.enabled`, off by default while it is a preview.
- **One pod:** `replicas: 1` with `Recreate`, the store on a `ReadWriteOnce` claim that is kept when the release goes. The pod runs non-root, with a read-only root file system and no capabilities.
- **Its own client certificate.** The console chart issues it with cert-manager from the daemons' CA (`tls.certManager.issuerRef`, else `global.tls.issuerRef`), or takes an existing Secret. In the box, the issuer is the chart's CA or the global issuer. With daemons on an existing Secret, the console needs its own Secret, and the render says so.
- **Pods read through a Role in the jobs namespace only** (get, list, watch pods). The service account token is mounted only for that.
- **The GitLab URL is set once.** The box writes the runner's `gitlabUrl` into a ConfigMap, and the console reads it as an environment variable.
- **Wired with tpl:** daemons, jobs namespace and issuer are rendered with `tpl` from the box's names.
- **Network access:** the daemons' NetworkPolicy admits console pods of the release namespace.
- **The API is a ClusterIP Service** without sign-in until sign-in through GitLab exists.
- **The image:** a static Go build on distroless static, both pinned by digest. It is not published until the console leaves preview; Go goes to the newest point release that is two weeks old before that.

## Consequences

- One `helm install` of the box gives a working console with `console.enabled=true`. The e2e `box` scenario checks it end to end.
- No high availability (ADR-0030). A rollout stops the console for a few seconds. The daemons keep their history, and the console reads it again.
- The release check expects the console chart's version too.
