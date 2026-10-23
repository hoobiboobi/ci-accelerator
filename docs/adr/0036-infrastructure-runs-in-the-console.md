# 0036. Terraform, OpenTofu and Terragrunt runs in the console

Date: 2026-10-22

Status: superseded by [ADR-0037](0037-console-builds-only.md): the console shows image builds only.

## Context

The box runs deploys too, often Terraform, OpenTofu or Terragrunt. Their job logs are long and the answer people look for is short: what will change, what changed, what failed and why, and whether the manual apply waiting in a pipeline is still safe to press. GitLab shows the log and, with its HTTP backend, a list of states; it does not connect the two.

## Decision

- **From the job's log** (read once, after the job, with the viewer's credential; kept as a versioned digest): plan and apply counts per Terragrunt unit, the resources each touched with their action, drift, errors with their file and line, lock errors, Terragrunt's run summary and the job's exit code. Only the tools' summary lines are read; values never leave the log. Applying a saved plan prints no plan, so the action of each resource comes from what completed. Checked against whole GitLab logs of Terragrunt 1.1.6 with Terraform 1.16.5 on the box's Kubernetes executor.
- **Exit code 2** of a plan with `-detailed-exitcode` reads as "changes found", not as a failure, wherever the job appears.
- **GitLab-managed states** through GraphQL (`read_api`, GET): each state's lock and its last ten versions with the job and pipeline that wrote them, at most once a minute per viewer and project. The state's content is never downloaded: it can hold secrets.
- **Stale plan:** a manual job of a pipeline that wrote a state before (in any pipeline) is warned about when that state was written by another pipeline after this pipeline's plan finished. Terraform would refuse the saved plan; the warning comes before the click.

## Consequences

- The digest depends on the tools' human output (no `-json`): new versions need a look at their summary lines; fixtures from real runs catch changes.
- A state is matched to a manual job by the job's name in earlier versions: the first apply of a new state gets no stale warning.
- Viewers below Developer see no states (GitLab hides them); the console says nothing then.
