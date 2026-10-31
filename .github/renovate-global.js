// Renovate's global configuration: what only the runner may set, read by
// .github/workflows/renovate.yml (the repository's own rules are in
// renovate.json5). Named so that Renovate does not take it for a
// repository config file.
module.exports = {
  platform: "github",
  repositories: ["hoobiboobi/ci-accelerator"],
  // The repository config is in the tree already: no onboarding PR.
  onboarding: false,
  requireConfig: "required",
  // Commits through GitHub's API: made and signed as the GitHub App the
  // workflow runs as (its installation token), so they verify.
  platformCommit: "enabled",
  branchPrefix: "renovate/",
  // The one command a repository rule may run after a bump: the checksums
  // next to the version, from where each project publishes them.
  allowedCommands: ["^scripts/update-pins\\.sh( [a-z0-9-]+)+( sha256:[0-9a-f]{64})?$"],
  // Run by hand (workflow_dispatch): a dry run, or outside the schedule.
  ...(process.env.RUN_DRY_RUN ? { dryRun: process.env.RUN_DRY_RUN } : {}),
  ...(process.env.RUN_IGNORE_SCHEDULE === "true" ? { force: { schedule: ["at any time"] } } : {}),
};
