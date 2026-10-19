# 0027. In-place upgrades, short-lived branches, releases when they matter

Date: 2026-10-19

## Context

Users install the box once and upgrade it in place, often through Argo CD or Flux. A chart that cannot upgrade from the previous release forces a reinstall. With emptyDir that loses the cache; with the bundled storage it may lose the metrics and traces. Semantic versioning allows anything to break in 0.x versions, which is no help to users who already run 0.1.

[ADR-0023](0023-values-compatibility.md) covers values keys added after 0.1.0. This record covers releases as a whole, and how changes reach `main`.

## Decision

**Upgrades.** Every release upgrades in place from the previous one. 0.x minors are treated like majors that must not break:

- **Values.** A key that is renamed or removed keeps working for at least one minor and prints a deprecation warning in the install notes. After that, the chart refuses it with a message that names the new key. Keys added later are read with defaults, as in ADR-0023.
- **Names and selectors.** Names and selectors of the objects users and runners refer to stay the same: Services, Secrets, the DaemonSet and its selector, the jobs namespace. A pod template change that restarts the daemons is allowed only on purpose, and the upgrade notes say so.
- **Data.** The cache on hostPath and the storage volumes survive an upgrade. Stored data of later components (the console) uses forward-only, versioned schema migrations. A version that finds a newer schema refuses to start instead of guessing.
- **Requirements.** The minimum Kubernetes and Helm versions rise only in a minor release, and only with an upgrade note.
- **Changelog.** Every release has an entry in `CHANGELOG.md` with an "Upgrade notes" section ("None" is a valid entry). The release fails without one, and the section goes into the GitHub release.
- **Testing.** The e2e scenario `upgrade` installs the last published release of both charts from the registry and upgrades them to the tree under test, with `--reuse-values` and then `--reset-then-reuse-values`. Both charts are covered because `--reuse-values` keeps the previous defaults of the chart being upgraded, but not of its bundled subcharts: a key added without a default breaks the standalone `buildkit` chart, and the umbrella only through its own values. It checks that CI jobs keep working, that the cache is still there, that the daemons are not restarted unless the tree says so (`UPGRADE_ROLLOUT_EXPECTED=1`), and that `helm rollback` works. Each release bumps the version the scenario starts from.

**Branches.** Development is trunk-based:

- `main` is protected. Changes arrive through short-lived branches and pull requests, with the checks and the end-to-end tests green. History is linear.
- Unfinished features are merged behind a switch that is off by default, not kept on a long-lived branch.
- A `release-0.N` branch is created only when a patch release for an older minor is needed. The fix lands in `main` first and is cherry-picked. Release tags `v*` cannot be moved or deleted.

**Releases.** Merging and releasing are separate. `main` gets small changes often; a release is cut from `main` when users gain something from it:

- **Patch** (`0.1.1`): fixes only, no new features and no behaviour changes. Typical reasons: a bug, a security fix, a dependency bump that fixes one.
- **Minor** (`0.2.0`): a meaningful new capability, ready and on by default. It still upgrades in place from the previous minor.
- **Pre-release** (`0.2.0-rc.1`): a rehearsal of a minor, published as a GitHub pre-release.
- Every change adds its line to the **Unreleased** section of `CHANGELOG.md`, including its upgrade notes. At release time the section is renamed to the version, so a release holds no surprises.
- A patch is released from `main` as long as unfinished work there is switched off. Only when `main` has changed what users would get does the patch come from a `release-0.N` branch.
- Users install releases from the registry, never `main`.

The steps are in [docs/releasing.md](../releasing.md).

## Consequences

- Breaking changes cost a deprecation cycle. Renames should be rare.
- Every pull request runs one more e2e scenario (a few minutes) and depends on the published chart in ghcr.io.
- The changelog is written with the change, not at release time.
- A large feature reaches `main` in several pull requests behind a switch; enabling it by default is a change of its own.
