# 0050. The CI component is tested in a real GitLab

Date: 2026-10-28

## Context

0.3.1 shipped a GitLab CI component that GitLab refused: its
`spec:description` had 257 characters, and GitLab allows 256 ("header:spec
config description is too long"). Every pipeline that included it failed
without jobs. The component's tests ran only its job script against a fake
buildctl; the file itself was checked by hand through gitlab.com's CI lint
when it was written. Reading GitLab's source afterwards showed more: the
`description` key exists only from GitLab 18.10 on, and older versions
refuse the whole header, so since 0.2.3 the component did not work on the
GitLab 17.9-18.9 that its README and ADR-0042 promised.

## Decision

- The component carries no `spec:description` (a comment says what it
  does): it works from GitLab 17.9 on again.
- `ci/gitlab/test/gitlab.sh` boots GitLab CE in Docker, pinned by digest,
  at the oldest version the component claims (17.9.8) and at the runner's
  minor (19.4.3, a security patch, ADR-0041). GitLab itself reads pipelines
  that include the component (CI lint with the pipeline simulation): the
  job it builds (stage, tags, image, variables, an untouched script) for
  every kind of input, and the inputs, hashes and headers it must refuse
  with their messages, among them a description of 257 characters.
- It runs only when the component's own files (`ci/gitlab/`) change, both
  versions side by side, since every GitLab boots for minutes; and in the
  release workflow, when the component changed since the previous release,
  on the file at the tag on GitHub under the integrity the notes print,
  before anything is published. Every release checks that the file at its
  tag is the one whose integrity the notes print.
- The e2e scenario `component` (its own group, under the same condition)
  runs real pipelines in that GitLab on the box's runner: checkout, build through the router, push to GitLab's
  registry with the job token, the digest in a later job, the cache on the
  second pipeline, a failing build, the console's pages for those jobs.

## Consequences

- A component GitLab refuses cannot be released, and the oldest version
  the README names is the oldest one tested.
- Each GitLab boot costs several minutes and about 1.5 GB of image; the
  e2e group `component` and the two checks jobs run in parallel with the
  rest.
- The GitLab versions are pins like any other: bumped when the runner's
  minor moves or a security patch appears, and the minimum only together
  with the README.
