# 0058. Cache-mount hints

Date: 2026-11-03

## Context

A cache mount keeps a package manager's downloads between builds even
when its layer is rebuilt. Many Dockerfiles have none, and many that
have one cannot use it. Of 123 public Dockerfiles found on GitHub (code
search for common install commands, 2026-11-03), 337 of 654 `RUN` steps
installed packages without one. Several had a mount that did nothing:
a pnpm store mount without `PNPM_HOME`, a Gradle mount on
`/home/gradle/.gradle` while the image runs Gradle as root, an apk
mount together with `--no-cache`, and apt mounts with the base image's
`docker-clean` still in place. The console already knows each step's
command, environment, user, working folder and mounts from BuildKit's
records, and what the step cost each time it ran.

## Decision

- The console reads each step's LLB operation (its command, its
  environment, which includes the base image's variables, its user, its
  working folder and its mounts), never the Dockerfile text, and
  matches the commands of about thirty package managers, such as apt,
  apk, dnf, pip, uv, npm, Go, Cargo, Maven, Gradle and NuGet.
- A step gets a hint when no cache mount covers the manager's cache and
  builds of the image executed the step in the last 30 days. It also
  gets one, whether the step ran or not, when a cache mount cannot work:
  - a flag, a variable or a command switches the cache off or empties it;
  - the mount is not where the manager writes;
  - the mount is on something the image needs;
  - apt, apk, dnf or zypper run without `sharing=locked`;
  - the step runs as a user other than root and the mount sets no owner
    or mode.
- Paths are computed from the operation, never guessed. When the cache
  folder depends on something the record does not show, such as a
  non-root user's home or the Yarn version, the hint shows no path and
  names the manager's own command for it (`npm config get cache`,
  `yarn cache dir`, ...).
- Figures are measured only: how often the step ran in 30 days, the time
  together, the median. The console makes no estimate of savings.
- Each hint carries a fragment that replaces the step. The fragment
  keeps the step's other options, and adds or fixes only cache mounts:
  `sharing=locked`, `uid`/`mode` for non-root steps, removing
  `docker-clean`, `keepcache`, and dropping `--no-cache-dir`.
- Pages:
  - the build page gets a Speed-ups card above the steps, ranked by time,
    and a numbered marker on each step;
  - the Dockerfile tab highlights the line and puts a note beside it;
  - the step's panel gets a Speed-up tab next to its output.
- `GET /api/v1/builds` returns the hints of each build. Hints cannot be
  dismissed; a hint goes away once the step has a cache mount that works.

## Consequences

- Checked against real builds, one per manager, on kind. The cache
  folder the hints place is the one the manager itself reports in the
  same image, for 20 of 22 managers. Yarn 1 as root and npm as a named
  user are left unplaced on purpose. The suggested fragment made the
  repeated step faster, for example Maven 36.8 s to 2.2 s, zypper 39 s
  to 5 s and Go 5.1 s to 0.9 s. Cargo did not get faster: compiling
  dominates its step.
- Found while testing on BuildKit v0.34.2:
  - a build with `--no-cache` gets its cache mounts empty;
  - a cache mount without `uid`/`mode` is root's, mode 0755, so a
    non-root step cannot write to it;
  - `uid`, `gid` and `mode` make a mount of their own, separate from the
    same id without them.
- No new store columns: hints are worked out from the step definitions
  the store already keeps, when a page or the API asks for them.
