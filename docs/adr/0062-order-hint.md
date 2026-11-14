# 0062. Dependencies installed again only because of the step order

Date: 2026-11-14

## Context

The console calls a rebuild "your change" when GitLab's comparison of the
commits shows that a file the first rebuilt step copies changed. That is
true, but it says nothing about what the change cost. A Dockerfile that
copies the whole build context (`COPY . .`) before installing the
dependencies installs every dependency again on a one-line code change. In
the lab run of 101 popular projects (2026-11-10), TradingAgents spent
30.4 s of a 36.8 s rebuild in `pip install .` after an edit of `main.py`;
graphify 19 s of 21 s. Static reading of the 101 Dockerfiles found an
install after a whole-context copy in 11 of them, and in 5 more a narrow
copy (`vendor/`, `patches/`) that the install legitimately reads.

## Decision

- A rebuild gets an "order" hint when all of these hold: its reason is
  files-changed and was settled by GitLab's comparison (the changed files
  are known, the list not cut short); the first rebuilt step copies the
  whole context; a later step of the same stage that ran again installs
  dependencies (pip, uv, Poetry, npm, Yarn, pnpm, Bun, Go modules,
  Bundler, Composer, Mix, Cargo) and took at least 1 s; none of the
  changed files is one that manager reads (by name, anywhere in the
  context); and no step before the copy already installs with the same
  manager (the right order, as browser-use and scrapling have it).
  Installs of folders of the context (`pip install ./libs/x`) are not
  judged.
- The figure is the step's measured time in this build and in the image's
  builds of the last 30 days (`builds.order_ms`, written when the reason
  is settled), never an estimate of what the fix saves.
- The Speed-ups card offers a fragment only where it was built in the lab
  (2026-11-14, BuildKit v0.34.2, ten real projects: cold, a one-line code
  edit, a smoke test of the image): copy the files the install reads
  (`COPY --parents **/package.json` for the JavaScript managers, so
  workspaces work; it needs Dockerfile syntax 1.20 or the daemon's own
  frontend), install the dependencies alone (`uv sync
  --no-install-project`, `poetry install --no-root`, `composer install
  --no-scripts --no-autoloader`, pip from pyproject.toml through
  `tomllib` or from the requirement file without its local lines), copy
  the code, and run the original command again, which finds the
  dependencies installed. Bundler (a gem's Gemfile reads its gemspec, which
  reads the code: the fragment failed on kamal) and Cargo get the hint in
  words; so does a step that is not right after the copy or does more
  than "a && b" in its script.
- A later rebuild of the image whose install before the copy came from
  the cache says the order was fixed, with the build times before and
  after. Insights lists the time these installs took; the API returns
  them as `order`.

## Consequences

- No hint without GitLab's comparison (a token or sign-in), as for every
  verified "your change".
- The fragment can still miss a file the install reads (a local package,
  a patch, a config): the caveat says so, and the original command after
  the copy keeps the image correct for pip, uv, Poetry and Composer.
- The store gains a column the previous release ignores: a rollback keeps
  working.
