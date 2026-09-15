<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.png">
    <source media="(prefers-color-scheme: light)" srcset="assets/logo-light.png">
    <img alt="marge" src="assets/logo-light.png" height="160">
  </picture>
</p>
<h1 align="center">marge</h1>
<p align="center">
  A housekeeping tool for dependency update PRs.<br>
  Automates the approve-and-merge workflow for
  <a href="https://docs.renovatebot.com/">Renovate</a> and
  <a href="https://docs.github.com/en/code-security/dependabot">Dependabot</a>
  pull requests that request your review.
</p>

---

It searches GitHub for open bot PRs, groups them interactively, waits for CI checks to pass, approves, and merges them -- with a live terminal table showing progress.

## Install

### From GitHub releases

Download the binary for your platform (Linux, macOS, Windows; amd64 and arm64) from the [releases page](https://github.com/giantswarm/marge/releases): the assets are named `marge-<os>-<arch>`, each next to its cosign signature bundle (`marge-<os>-<arch>.bundle`). Releases are built and signed by the repository's CircleCI pipeline; `marge self-update` installs a newer release only after that bundle verifies.

> marge moved here from a personal namespace, and its release pipeline moved from GitHub Actions to CircleCI. Binaries up to v0.8.1 verify release signatures against the former pipeline and refuse releases built here, so `marge self-update` cannot carry them across. Install once from the releases page above; from then on `self-update` works again.

### From source

```bash
go install github.com/giantswarm/marge@latest
```

Or clone and build locally (`make build` stamps the version with [gitsemver](https://github.com/giantswarm/gitsemver); `make install` puts the binary into `$(go env GOPATH)/bin`):

```bash
git clone https://github.com/giantswarm/marge.git
cd marge
make install
```

### On Kubernetes

The `marge` Helm chart in the [giantswarm catalog](https://github.com/giantswarm/giantswarm-catalog) runs `marge serve` over the MCP Streamable HTTP transport behind a `ClusterIP` Service, ready to be registered as a streamable-http MCP server in [muster](https://github.com/giantswarm/muster). It takes the GitHub token from `marge.github.token` or an existing Secret (`marge.github.existingSecret`); see [helm/marge/README.md](helm/marge/README.md) for every value.

```bash
helm install marge oci://gsoci.azurecr.io/charts/giantswarm/marge --version 0.9.0 \
  --set marge.github.existingSecret=marge-github-token
```

## Setup

Marge needs a GitHub token and looks for one in this order:

1. the `GITHUB_TOKEN` environment variable
2. the `GH_TOKEN` environment variable
3. the [GitHub CLI](https://cli.github.com/)'s active login (`gh auth token`)

If you are logged in with `gh auth login`, no further setup is needed. Otherwise export a personal access token:

```bash
export GITHUB_TOKEN="ghp_..."
```

**Classic token:** needs the `repo` scope.

**Fine-grained token:** select the repositories you want marge to manage, then grant these permissions:

| Permission | Access | Why |
|------------|--------|-----|
| Pull requests | Read & write | Approve and merge PRs |
| Checks | Read | Wait for CI status |
| Commit statuses | Read | Read combined commit status |
| Metadata | Read | Required by default |
| Contents | Read (write only with `--refresh-stale`) | Compare a PR with its base (stale classification, rescue marker fingerprints); with `--refresh-stale`: update a stale PR branch from its base |

Optionally, a CircleCI API token lets marge inspect builds of **private** CircleCI projects and retry auto-cancelled builds (see [Cancelled builds](#cancelled-builds-circleci-auto-cancel)). marge reads `CIRCLECI_CLI_TOKEN` or the CircleCI CLI's own config, `~/.circleci/cli.yml`, and sends it as the `Circle-Token` header. Public projects need no token.

## Usage

### `marge [query] [flags]` (default command)

When run without a query, marge enters **interactive mode**: it fetches all open bot PRs requesting your review and lets you pick a group to process.

When run with a query (e.g. a repo name or dependency), it filters PRs directly and processes them.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--dry-run` | | `false` | Show what would be done without making changes |
| `--watch` | `-w` | `false` | Keep polling for new PRs every 60 seconds |
| `--grouping` | | `repo` | Group by `repo` or `dependency` |
| `--author` | | `all` | Filter by PR author: `renovate`, `dependabot`, or `all` |
| `--org` | | | Limit to repos owned by this org or user |
| `--repos-file` | | | File with `org/repo` entries (one per line; blank lines and `#` comments are ignored) to scan for bot PRs instead of searching GitHub. A query then keeps only the listed repos whose `org/repo` contains it (case-insensitive) |
| `--no-tui` | | `false` | Disable the live table; print plain-text results instead |
| `--merge-auto` | | `false` | Also merge PRs that have auto-merge enabled (by default these are skipped) |
| `--refresh-stale` | | `false` | Update the branch of stale PRs from their base so CI re-runs (see [Stale failures](#stale-failures-already-fixed-on-the-base-branch)) |
| `--retry-cancelled` | | `false` | Retry CircleCI builds that CircleCI auto-cancelled on the PR head (see [Cancelled builds](#cancelled-builds-circleci-auto-cancel)) |
| `--trusted-authors` | | `renovate[bot],dependabot[bot]` | Comma-separated list of trusted PR author logins |
| `--security-patterns` | | _(built-in)_ | Override the built-in security check pattern list (see below) |

#### Security check patterns

When a PR's CI fails, marge classifies the failure as security-related if any failing check's name contains one of the configured substrings (case-insensitive). Security failures are surfaced separately so they are not mistaken for ordinary build/test flakiness.

The built-in list contains: `security`, `govulncheck`, `trivy`, `codeql`, `snyk`, `gosec`, `gitleaks`, `semgrep`, `checkov`, `kics`, `vulnerability`, `vulnerabilities`, `sast`, `dast`, `dependency-review`, `dependency review`.

Pass `--security-patterns "Trivy,Govulncheck,CodeQL,Analyze"` to replace the list. The github/codeql-action template uses a job name like `Analyze (<lang>)` that the `codeql` substring will not match, so add `Analyze` if you rely on that template.

#### CI unavailable (Actions budget)

Sometimes a check reports `failure` not because the code is broken but because GitHub never started the job -- a personal account or organization has exhausted its Actions budget. GitHub surfaces this as a job that never reached the runner:

- `The job was not started because an Actions budget is preventing further use.`

Verified against the live GitHub API, such a block shows up as a check run with conclusion `failure`, empty `output`, and a single failure-level **annotation** whose `message` carries the text above (the message is in the annotation, not the output fields). marge therefore inspects each failed check run's annotations and matches that message -- which never appears for a genuine test, build, or lint failure.

When **every** failing check on a PR is such a block, marge classifies the PR as `CI unavailable (budget)` rather than `Failed`. It is counted separately, surfaced under its own section, and kept out of any rescue path -- the fix is to raise or await the Actions budget, not to touch the code. If a PR has a mix of a genuine failure and a budget block, it is still reported as `Failed`.

> Only this API-verified message is matched; any unrecognized block degrades to the normal `Failed` path rather than risking a real failure being hidden.

#### Stale failures (already fixed on the base branch)

A dependency PR is built once and then sits in the queue while the base branch moves on. When a fleet-wide fix lands on `main` -- a CVE bump that turned every open Go PR's vulnerability scan red, a linter pin, a CI infra repair -- the PR's last build stays red although the failure no longer exists. Without help, an operator (or a rescue agent) spends time diagnosing a failure that a branch refresh would have cleared.

marge recognises this case. When a PR's checks fail, it:

1. compares the PR head with its base branch (`GET /repos/{owner}/{repo}/compare/{base}...{head}`) and continues only if the head is **behind** (`behind_by > 0`);
2. looks up the latest run of **every failing check** on the base branch head (commit statuses and check runs);
3. classifies the PR as **`Stale`** instead of `Failed` when all of them are green there, e.g. `Stale (go-build green on main since 2026-09-05 10:57 UTC, 12 behind)`.

A PR that is behind but whose failing check is also red on the base branch stays `Failed` -- the failure is real on `main` too. So does a PR whose failing check does not exist on the base branch at all (a PR-only workflow cannot be proven green), and a PR that is not behind. Any lookup error keeps the `Failed` classification; a stale verdict is only ever reached on positive evidence. The check runs before the security split, so a stale govulncheck or Trivy failure is recognised like any other.

Stale PRs are listed in their own **Stale** section, counted separately from failures, and kept out of the action-required list -- the remedy is a refresh, not a rescue. With **`--refresh-stale`** (MCP: `refresh_stale: true`) marge performs that refresh itself: it calls `PUT /repos/{owner}/{repo}/pulls/{number}/update-branch` (the same merge commit as GitHub's "Update branch" button) and reports the PR as **`Refreshed (re-checking; ...)`**. CI re-runs against current code and the next sweep merges the PR if it is green -- or reports it as `Failed`, because it is no longer behind.

Two guards apply to the refresh:

- `--dry-run` prints the `Stale` classification but never calls update-branch.
- A PR carrying a **non-stale [rescue marker](#rescue-markers-prior-ai-rescue-attempts)** is not refreshed (`Stale (...; refresh skipped: fresh rescue marker)`). The marker pins the change an automated rescue already failed on -- whether the branch was rebased since or not -- so re-running CI against a newer base cannot help. The marker is shown on the entry so the operator can decide. A stale marker (the PR content changed since the attempt) does not block the refresh.

The heuristic is deliberately cheap: `main` being green does not prove the bump itself is innocent (a major bump can be red for its own reasons while `main` is fine). The cost of a wrong `Stale` verdict is one branch refresh and one CI run, after which the PR is no longer behind and is classified on its own merits.

#### Cancelled builds (CircleCI auto-cancel)

CircleCI cancels a running build on its own when a newer pipeline starts on the same branch (Renovate pushed a rebase or a new version while the previous build ran) or when it detects a redundant workflow. The cancelled build still posts `failure` -- "Your tests failed on CircleCI" -- to the commit it was building, and the status description carries no hint of the cancellation. Read at face value, the PR looks like a real failure and costs an operator (or a rescue agent) a diagnosis that ends in "nothing is wrong, just retry".

marge looks behind the status. When **every** failing check of a PR is a commit status whose `target_url` points at a CircleCI build (`https://circleci.com/gh/<owner>/<repo>/<build>`), it fetches that build from the v1.1 API (`GET /api/v1.1/project/github/<owner>/<repo>/<build>`) and inspects its steps. Verified against the live API: an auto-cancelled build reports `status: failed` and `canceled: false` at the top level, exactly like a real failure -- but its steps are all `success` up to the cancellation and only `canceled` from there on, whereas a real failure has a step with `status: failed`. A build cancelled before any step ran reports `status: canceled` instead. Both shapes classify the PR as **`Cancelled`** rather than `Failed`:

- **On the current head** -- `Cancelled (build 1263 auto-cancelled; retry needed)`: the commit has no verdict yet. With **`--retry-cancelled`** (MCP: `retry_cancelled: true`) marge calls `POST /api/v2/workflow/<workflow_id>/rerun` with `{"from_failed": true}` on the workflow the build belongs to (its `workflows.workflow_id`, part of the v1.1 build JSON), and reports the PR as **`Retried (re-checking; workflow build rerun from failed)`**. The next sweep reads the real result. The workflow rerun also releases the jobs the cancel left `blocked` or `not_run`; the v1.1 single-build retry does not, so on a repository whose branch protection requires those downstream contexts the PR would go green on the retried job and stay unmergeable. Each distinct workflow is rerun once, however many of its jobs were cancelled. A build that carries no workflow id falls back to `POST /api/v1.1/project/github/<owner>/<repo>/<build>/retry` and is reported as `Retried (re-checking; build 1263 retried as 1272)`.
- **Behind a newer head** -- `Cancelled (build 1244 (2c0ce64) auto-cancelled; head is now 605a2d6, its build is the verdict)`: the branch moved on and the new head's own build decides. Nothing is retried.

Cancelled PRs are listed in their own **Cancelled** section (and **Retried** once retried), counted separately from failures and kept out of the action-required list. The lookup is lazy -- nothing is fetched unless a failing status points at CircleCI -- and it is decided before the stale and security classifications because it rests on positive evidence about the very build that failed. A PR with a mix of a cancelled build and a genuine failure (a failing GitHub Actions check run, a CircleCI build with a failed step) stays `Failed`. `--dry-run` classifies but never retries.

Public CircleCI projects can be inspected without credentials. Private projects and every rerun endpoint need a CircleCI API token (see [Setup](#setup)). Without one, a private project's build cannot be inspected and the PR keeps today's `Failed` classification, annotated: `Failed (checks failed: ci/circleci: go-build; ci/circleci: go-build: build 1263 could not be inspected (HTTP 404: Build not found; no CircleCI token configured (private project?)))`. A cancelled build on the current head with `--retry-cancelled` but no token is reported as `Cancelled (...; retry skipped: no CircleCI token configured)`.

#### PR age highlighting

Every table and report includes an **Age** column showing how long the PR has been open (`5h`, `3d`, `2w`). PRs older than 3 days are highlighted yellow, older than 7 days red -- an old dependency PR has already survived several sweeps and is the most likely to need manual work. Failure sections are sorted oldest-first for the same reason.

#### Rescue markers (prior AI rescue attempts)

When an automated rescue (a coding agent, a CI bot, a human with a script) tries to fix a failing dependency PR and gives up, it can record that attempt as a machine-readable **ai-rescue marker** inside an ordinary PR comment:

```markdown
**AI rescue failed** (klaus): nock v14 is ESM-only and breaks Jest CJS resolution.

<!-- ai-rescue: {"tool":"klaus","outcome":"failed","reason":"ESM-only breaks Jest CJS","head_sha":"d9f00bf2","at":"2026-06-09T18:40:00Z","patch_id":"5ad45e13d66acc2b","change_id":"nock@v14"} -->
```

On every sweep, marge reads the comments of each failing PR and annotates its entry with the most recent marker, e.g. `[rescue failed 1d ago (klaus): ESM-only breaks Jest CJS]`. The marker records what it was attempted against, and the sweep decides from that whether the attempt still describes the current PR:

- **`head_sha`** -- the PR head at the time. Same head: the marker is fresh.
- **`patch_id`** (optional) -- a fingerprint of the PR's diff. When the head moved, marge recomputes it for the current head from `GET /repos/{owner}/{repo}/compare/{base}...{head}`. Same fingerprint: the branch was only rebased (Renovate does this whenever the base moves) and the marker stays fresh, annotated `[rescue blocked 5d ago (klaus), rebased since: same change: ...]`. Different fingerprint (a new version, a pushed fix): the marker is **stale** (`[rescue failed 3d ago (klaus), stale: new commits since]`) and the PR is fair game for another rescue.
- **`change_id`** (optional) -- the cheap fallback for dependency PRs: `<dependency>@<target version>` parsed from the PR title (`typescript@v7`). It decides only when no `patch_id` can be compared, e.g. when the diff is too large for the compare API. A rebase never changes it and a new version always does, but a version update that keeps the title (7.0.1 -> 7.0.2 under "to v7") is invisible to it, which is why the diff fingerprint is preferred whenever it is available.

Markers without a fingerprint (written before it existed) age out with the head SHA alone, as before.

The `patch_id` is the first 16 hex characters of a SHA-256 over the compare response's files, sorted by path: per file its status, previous path and path, then every `+`/`-` line of the patch in order -- hunk headers and context lines are skipped, since both shift when the base changes around the PR's lines. A file without a patch (binary, pure rename) contributes its blob SHA. The fingerprint is left out when GitHub truncates the response (more than 300 files, or a file whose diff is too large to include a patch), so a partial diff is never mistaken for the whole change.

This makes the daily triage call obvious at a glance:

- **failing + fresh failed rescue** (rebased since or not) -> automation already lost; a human is needed
- **failing + stale or no marker** -> dispatch (another) automated rescue

Use [`marge mark`](#marge-mark-pr-url-flags) to write markers without knowing the format. Any tool that can comment on a PR can participate -- there is no coupling to a specific agent framework.

### `marge sweep [flags]`

Processes all matching PRs without interactive grouping. The live table shows every PR's outcome, including the failure reason and any ai-rescue marker, followed by a one-line summary; nothing is repeated below the table. With `--no-tui` the results are printed as plain-text groups instead: **Merged**, **Security failures**, **Failed** (PRs that failed, have conflicts, or came from untrusted authors), **Stale** (PRs whose failing checks are already green on their base branch; **Refreshed** once `--refresh-stale` has updated them), **Cancelled** (PRs whose failing CircleCI builds were auto-cancelled by CircleCI; **Retried** once `--retry-cancelled` has retried them), **CI unavailable (Actions budget)** (PRs whose checks never ran because an Actions spending limit was exhausted), and **Skipped**. Security failures (e.g. govulncheck, Trivy, CodeQL) are separated so they are not mistaken for ordinary CI flakiness; stale, cancelled and budget-blocked PRs are separated so they are not mistaken for genuine failures (see [Stale failures](#stale-failures-already-fixed-on-the-base-branch), [Cancelled builds](#cancelled-builds-circleci-auto-cancel) and [CI unavailable (Actions budget)](#ci-unavailable-actions-budget)).

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--dry-run` | | `false` | Show what would be done without making changes |
| `--watch` | `-w` | `false` | Keep polling for new PRs every 60 seconds |
| `--author` | | `all` | Filter by PR author: `renovate`, `dependabot`, or `all` |
| `--org` | | | Limit to repos owned by this org or user |
| `--repos-file` | | | File with `org/repo` entries (one per line; blank lines and `#` comments are ignored) to scan for bot PRs instead of searching GitHub |
| `--no-tui` | | `false` | Disable the live table; print plain-text results instead |
| `--merge-auto` | | `false` | Also merge PRs that have auto-merge enabled (by default these are skipped) |
| `--refresh-stale` | | `false` | Update the branch of stale PRs from their base so CI re-runs (see [Stale failures](#stale-failures-already-fixed-on-the-base-branch)) |
| `--retry-cancelled` | | `false` | Retry CircleCI builds that CircleCI auto-cancelled on the PR head (see [Cancelled builds](#cancelled-builds-circleci-auto-cancel)) |
| `--trusted-authors` | | `renovate[bot],dependabot[bot]` | Comma-separated list of trusted PR author logins |
| `--security-patterns` | | _(built-in)_ | Override the built-in security check pattern list (see [Security check patterns](#security-check-patterns)) |

### `marge mark <pr-url> [flags]`

Records a failed AI rescue attempt on a PR by posting an [ai-rescue marker](#rescue-markers-prior-ai-rescue-attempts) comment. The marker captures the PR's current head SHA plus a fingerprint of its diff (`patch_id`) and, for dependency PRs, of its title (`change_id`), so it goes stale when the PR content changes but survives a Renovate rebase that leaves the diff unchanged. The confirmation line lists what was pinned, e.g. `Marked my-org/my-repo#42: rescue blocked (head 1be1ed9d, patch_id 5ad45e13d66acc2b, change_id typescript@v7)`.

| Flag | Default | Description |
|------|---------|-------------|
| `--outcome` | `failed` | Rescue outcome: `failed` (attempted, could not fix) or `blocked` (fix known but waits on something external) |
| `--reason` | | Short explanation of why the rescue did not succeed |
| `--tool` | `ai` | Name of the tool/agent that attempted the rescue (e.g. `klaus`) |

```bash
marge mark https://github.com/my-org/my-repo/pull/42 \
  --tool klaus --reason "nock v14 is ESM-only, needs Jest ESM migration"
```

Requires the token to have **Issues: Read & write** (comment) permission in addition to the permissions listed under [Setup](#setup).

### `marge serve [flags]`

Starts an MCP server exposing two tools:

- **`sweep`** -- mirrors `marge sweep`, returning structured JSON (`summary`, `merged`, `security_failures`, `action_required`, `stale`, `refreshed`, `cancelled`, `retried`, `ci_unavailable`, `skipped`). `query` narrows the sweep the way `marge [query]` does: a dependency name, a repo name, or GitHub search qualifiers such as `repo:my-org/my-repo`. `repos` (a list of `org/repo` entries) and `repos_file` (a file in the `--repos-file` format) scan exactly the listed repositories instead of searching GitHub; given together they are merged without duplicates, and `query` then filters the listed repositories by name. Each PR entry includes `created_at`, `age_days`, and -- when a prior rescue attempt was found -- a `rescue` object (`tool`, `outcome`, `reason`, `at`, `stale`, `rebased`). `rebased: true` means the PR head moved since the attempt but the diff did not (a Renovate rebase); such a marker is still valid and `stale` is `false`. Pass `refresh_stale: true` to update stale branches from their base (they then appear under `refreshed`) and `retry_cancelled: true` to retry auto-cancelled CircleCI builds on the PR head (they then appear under `retried`); `dry_run: true` still classifies them under `stale` and `cancelled`. Agent orchestrators should dispatch on `action_required` only, skip entries whose rescue is not `stale` (rebased or not), and escalate those to a human.
- **`mark`** -- mirrors `marge mark`, so rescue agents can record their own failed attempts. The result echoes what was pinned: `head_sha` plus `patch_id` and `change_id` when they could be computed.

| Flag | Default | Description |
|------|---------|-------------|
| `--transport` | `stdio` | `stdio` (JSON-RPC over stdin/stdout, for an MCP client that starts marge itself) or `streamable-http` (the MCP Streamable HTTP transport, what the Helm chart runs) |
| `--http-addr` | `:8080` | Listen address for `streamable-http` |

Over `streamable-http` the MCP endpoint is `/mcp`; `/healthz` and `/readyz` answer the Kubernetes probes. The server stops on `SIGTERM`/`SIGINT` after draining in-flight requests for up to ten seconds. The GitHub token is read per tool call, so the server starts without one and reports the missing token on the first `sweep` or `mark`.

```bash
marge serve                                        # stdio, for a local MCP client
marge serve --transport streamable-http --http-addr :8080
```

### Other commands

```bash
marge version         # Print the current version
marge self-update     # Update to the latest release
```

### Examples

Process all bot PRs interactively, grouped by repository:

```bash
marge
```

Process only Renovate PRs:

```bash
marge --author renovate
```

Filter PRs matching a query and keep watching:

```bash
marge "my-org/my-repo" --watch
```

Dry run to preview what would happen:

```bash
marge --dry-run
```

Group by dependency instead of repository:

```bash
marge --grouping dependency
```

Sweep all PRs in a specific org:

```bash
marge sweep --org my-org
```

Sweep including PRs with auto-merge enabled:

```bash
marge sweep --merge-auto
```

Sweep and refresh stale PRs (behind their base, failing checks already green there) so CI re-runs:

```bash
marge sweep --refresh-stale
```

Sweep and retry CircleCI builds that CircleCI auto-cancelled on the PR head, so the same commit gets a real verdict:

```bash
marge sweep --retry-cancelled
```

## How it works

1. Searches GitHub for open PRs authored by `app/renovate` or `app/dependabot` that are either requesting your review or in your own repositories. Self-authored dependency-update PRs (e.g. from self-hosted Renovate) in your repos are also included. With `--repos-file`, the open bot PRs of the listed repositories are collected instead of running the search.
2. In interactive mode, groups results by repository (or dependency) and presents a selector.
3. Validates each PR's author against a trusted allow-list (`renovate[bot]`, `dependabot[bot]`, and the authenticated user by default). PRs from untrusted authors are refused with a clear status message. You can extend the allow-list with `--trusted-authors`.
4. For each selected PR, processes it in parallel (up to 5 concurrent):
   - Checks combined commit status and check runs; polls every 15 seconds for up to 5 minutes if pending.
   - On failure, looks behind failing CircleCI statuses: a build that CircleCI itself auto-cancelled makes the PR `Cancelled`, not `Failed`, and `--retry-cancelled` retries it on the same commit.
   - Otherwise checks whether the PR is behind its base branch with every failing check green on the base head; if so it is `Stale`, not `Failed`, and `--refresh-stale` updates the branch so CI re-runs.
   - Approves the PR if not already approved.
   - If auto-merge is enabled, lets the merge queue handle it (unless `--merge-auto` is set).
   - Otherwise merges via squash.
5. Displays a live-updating table with columns for repository, dependency, version, age, author, and status. Failing entries are annotated with any prior AI rescue attempt found on the PR. Use `--no-tui` for plain-text output.

## Development

```bash
make build          # Build the binary
make test           # Run tests
make lint           # Run golangci-lint
make help           # Show all available targets
```

## License

MIT -- see [LICENSE](LICENSE) for details.
