<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.png">
    <source media="(prefers-color-scheme: light)" srcset="assets/logo-light.png">
    <img alt="marge" src="assets/logo-light.png" height="160">
  </picture>
</p>
<h1 align="center">marge</h1>
<p align="center">
  The sweep engine for bot pull requests.<br>
  Drains a team's queue of
  <a href="https://docs.renovatebot.com/">Renovate</a>,
  Align files, Herald and
  <a href="https://docs.github.com/en/code-security/dependabot">Dependabot</a>
  pull requests by codified rules, as the person who runs it.
</p>

---

`marge sweep --team <name>` reads the team's repositories from `giantswarm/github`, finds every open bot PR, approves and squash-merges the eligible green ones, labels each PR with its classification, writes its evidence on the PR and prints the outcomes. Nothing it does needs a model. The interactive `marge [query]` command and the MCP server run the same engine.

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

The `marge` Helm chart in the [giantswarm catalog](https://github.com/giantswarm/giantswarm-catalog) runs `marge serve` over the MCP Streamable HTTP transport behind a `ClusterIP` Service. With `muster.register=true` it also registers the server with [muster](https://github.com/giantswarm/muster), which then runs the GitHub sign-in for it and attaches each person's grant to their calls; the server pod holds no GitHub credential of its own. The registration uses the sweep App's **own** OAuth client (`marge.github.app.oauth.*`), never the shared `github-oauth-client`: a person acting through marge is bounded by the intersection of the App's permissions and their own only while the App is the OAuth client on that path. See [helm/marge/README.md](helm/marge/README.md) for every value.

```bash
helm install marge oci://gsoci.azurecr.io/charts/giantswarm/marge --version 0.9.0 \
  --set muster.register=true \
  --set muster.namespace=agent-platform \
  --set marge.github.app.oauth.existingSecret=marge-github-app
```

muster watches MCPServers in its own namespace only, so `muster.namespace` names it when marge runs elsewhere. The OAuth client Secret stays in marge's namespace, and muster's ServiceAccount needs `get` on Secrets there: list the namespace in muster's `rbac.additionalSecretNamespaces` on the platform side.

A freshly reconciled server reports *Auth Required* until the person completes `core_auth_login` in muster, then *Connected*: pinning GitHub's endpoints does not bypass the connect-time probe. `grantScope: subject` files the grant under the person, so every later session reuses it without a second consent, until they sign out of this server.

## Setup

Marge needs a GitHub token and looks for one in this order:

1. the `GITHUB_TOKEN` environment variable
2. the `GH_TOKEN` environment variable
3. the [GitHub CLI](https://cli.github.com/)'s active login (`gh auth token`)

If you are logged in with `gh auth login`, no further setup is needed. Otherwise export a personal access token:

```bash
export GITHUB_TOKEN="ghp_..."
```

A scheduled run uses none of these. It authenticates as the sweep GitHub App and mints an installation token per repository; see [docs/github-app.md](docs/github-app.md).

**Classic token:** needs the `repo` scope.

**Fine-grained token:** select the repositories you want marge to manage, then grant these permissions:

| Permission | Access | Why |
|------------|--------|-----|
| Pull requests | Read & write | Approve and merge PRs |
| Issues | Read & write | Write the `marge/<class>` label and the evidence comments |
| Checks | Read | Read check runs |
| Commit statuses | Read | Read combined commit status |
| Metadata | Read | Required by default |
| Contents | Read & write | Compare a PR with its base (stale classification, marker fingerprints); update a PR branch from its base |
| Administration | Read | Read the base branch's required status checks. Without it marge approves and tries the merge, GitHub enforces the checks, and a refusal for a check reason is reported as `Waiting for checks` |

The token needs read access to the repository that holds the team files and the policy files: `giantswarm/github`, or the `owner/repo` that `MARGE_TEAM_FILE_REPO` names.

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
| `--actions` | | _(all)_ | Comma-separated sweep steps to run, in fixed order: `classify`, `approve`, `merge`, `refresh`, `retry`, `remedy`, `mark` (see [Actions](#actions)) |
| `--org` | | | Limit to repos owned by this org or user |
| `--repos-file` | | | File with `org/repo` entries (one per line; blank lines and `#` comments are ignored) to scan for bot PRs instead of searching GitHub. A query then keeps only the listed repos whose `org/repo` contains it (case-insensitive) |
| `--no-tui` | | `false` | Disable the live table; print plain-text results instead |
| `--merge-auto` | | `false` | Also merge PRs that have auto-merge enabled (by default the merge itself is left to GitHub) |
| `--security-patterns` | | _(built-in)_ | Add to the built-in security check pattern list (see below) |
| `--rules-repo` | | `giantswarm/marge` | Repository the rule catalogue is read from, as `owner/name` |
| `--rules-ref` | | `main` | Branch the rule catalogue is read from |
| `--rules-path` | | | Read the catalogue from this directory instead of the repository |

#### Bot PR kinds and eligibility

marge touches PRs authored by four bots and nothing else: `renovate[bot]` (Renovate), `giantswarm-align-files[bot]` (Align files), `heraldbot[bot]` (Herald, nancy-fixer's security remediation PRs) and `dependabot[bot]`. A PR by a person, the caller's own included, is reported as `Untrusted author` and never approved or merged. There is no flag to widen that set.

A green PR merges when the resolved [sweep policy](#sweep-policy) says its kind and update size are eligible. The company defaults are: Align files and Herald PRs always; Renovate and Dependabot patch, minor, digest, pin and lockfile updates. A major update, or one whose size marge cannot read, is `Held` for a person. The update size comes from the versions Dependabot writes in the title (or, for a group, per dependency in the body) and Renovate writes in the body's *Change* column.

#### Sweep policy

A team declares its own appetite for sweeps in `giantswarm/github`, in files the team owns. marge reads them from the default branch at the start of every sweep, so a change takes effect on the next run.

| File | Holds | Owned by |
|------|-------|----------|
| `bot-prs-sweep/default.yaml` | the company defaults | Bumblebee, in CODEOWNERS |
| `bot-prs-sweep/team-<name>.yaml` | one team's deviations | that team, in CODEOWNERS: a new team file adds its own line, or the directory line leaves it with Bumblebee |
| `botPRsSweep` on a repository entry of `repositories/team-<name>.yaml` | one repository's exception | that team |

The policy files say what may merge, and nothing about when a team is swept. Each team has its own CronJob in the [chart](helm/marge/README.md), which sweeps that team on its own cadence and is suspended on its own. A team without a policy file is swept under the company defaults.

```yaml
# bot-prs-sweep/team-bumblebee.yaml
slackChannel: standup-bumblebee
updateTypes:
  renovate: [patch, minor]
rescue:
  enabled: false
  timeout: 20m       # per rescue
  weekly: 5          # rescues per week
  budget:
    perRescue: 3.00  # US dollars, declared
    weekly: 15.00    # US dollars, declared
  confirm: per-pr
concurrency:
  perTeam: 5
  perRepo: 1
modelConfig: default-model-config
```

Every key is optional and an absent key keeps what the file before it said. `updateTypes` replaces the list of the kinds it names; the known update types are `major`, `minor`, `patch`, `digest`, `pin`, `lockfile` and `none`, and an update whose size marge could not read can never be declared eligible. An empty list is a list: `renovate: []` merges no Renovate PR at all.

marge dispatches no rescue yet, so the whole `rescue` section is declared and none of it is applied: `timeout`, `weekly` and the two `budget` figures alike. Every outcome records `rescues_dispatched: false` and `budget_enforced: false`, and a sweep whose policy switches the rescues on names every bound it does not apply on stderr. The rescue agent enforces `timeout` and `weekly` when it lands; a per-rescue budget then needs the platform to accept a budget on a run, and a weekly budget needs the cost of a finished run to be readable. Enforcement moves under the same file without a team editing anything.

`concurrency` has a ceiling marge owns: `perTeam` reaches 20, `perRepo` reaches 5, and the two multiply to at most 20 PRs in flight. GitHub answers a burst of writes with a secondary rate limit, which a sweep cannot tell apart from a repository it may not touch, so a policy that declares more is an error rather than a sweep that fails halfway. The product is checked on the resolved policy, so `perTeam: 20` in a team file is an error when the company file already raised `perRepo`. `bot-prs-sweep/policy.schema.json` checks each key against its own range; JSON Schema cannot multiply, so the pair is marge's to refuse.

A repository entry deviates under `botPRsSweep`, with three keys that only narrow:

```yaml
# repositories/team-bumblebee.yaml
- name: marge
  componentType: cli
  botPRsSweep:
    enabled: false        # no PR of this repository is touched, not even labelled
- name: muster
  componentType: service
  botPRsSweep:
    updateTypes: [patch]  # intersected with the team's Renovate and Dependabot lists
    rescue: false
```

`updateTypes` here reaches Renovate and Dependabot only. An Align files or Herald PR names no version, so restricting the update sizes of one repository does not stop those two kinds from merging.

An exception that tries to switch the rescues back on where the team switched them off is an error, and so is an update type the team merges for no kind. A widening is refused, never silently narrowed. `none` is refused here too: it belongs to Align files and Herald, which an exception does not reach, so a list that names it would read as a restriction and switch every Renovate and Dependabot merge off. `enabled` has no team-level counterpart: a repository is the only place the sweep itself is switched off.

`giantswarm/github` validates both policy files against `bot-prs-sweep/policy.schema.json` on every pull request, so a misspelled key fails there and not on the next sweep.

A file marge cannot read stops the sweep and names the file and the key: a misspelled key, a second YAML document, an unknown bot PR kind or update type, a timeout that is not a duration, a negative cap, a confirmation that is neither `per-pr` nor `per-sweep`. Falling back to the defaults would sweep a team's repositories under a policy the team never wrote. A file that is simply absent is not an error: the company defaults apply and the outcome names the files that were read.

The policy each PR was decided under is on its outcome entry, with the list of files that produced it, so every decision can be explained after the fact. `--output json` and the MCP `sweep` tool carry it as the `policy` object.

#### Guards

Each guard is enforced by the engine and covered by a scenario test; none has an override flag.

- **Required checks.** The base branch's required status checks are read from its protection. A required context that is pending, or that nobody reported, is a wait (`Waiting for checks`), never a bypass, also when every red check on the head is pre-existing. A merge GitHub refuses for a check reason is a wait too.
- **Red non-required check.** A failing check that is not required blocks the merge when the same check is green on the base head, or never ran there. When it is red on the base head too the failure is pre-existing: the PR merges and the check is named in the detail and in an evidence comment.
- **Security check.** A failing check whose name matches the security pattern list is never merged past, even when it is red on the base head too. The PR gets a `security-blocked` evidence comment; a rescue may still be dispatched on it.
- **Auto-merge.** A PR with GitHub auto-merge enabled is classified, held or approved like any other, and then left to GitHub for the merge itself. GitHub fires auto-merge only once every requirement is met and does nothing to meet one, so the sweep still approves it and still updates a branch behind its base. A failing required check classifies the PR on the check, not on auto-merge. The `auto-merge` outcome is counted apart from `merged`, because nothing merged yet.
- **Review rule.** A green PR that GitHub refuses to merge after marge's approval is `Awaiting approval`. marge never merges as an admin and never touches `enforce_admins`.
- **Strict protection.** A green PR behind its base is brought up to date with *Update branch* and merges on a later sweep, once its checks ran on the new head.

#### Labels and evidence

Every PR the sweep touched carries exactly one `marge/<class>` label, replaced on each sweep: `merged`, `auto-merge`, `eligible`, `pending`, `action-required`, `awaiting-approval`, `security`, `stale`, `conflict`, `ci-unavailable`, `skipped`. Labels are display only; no guard reads them back. A label marge may not write is a note on the entry, never a different outcome. A `bot-prs-sweep/<class>` label from the earlier namespace is removed on the next sweep that touches the PR.

An action marge performed, or a guard decision a person needs to see, is written once as an evidence comment: `update-branch`, `retry`, `merged-past-red-check`, `awaiting-approval`, `security-blocked`. Evidence is an [ai-rescue marker](#rescue-markers-prior-ai-rescue-attempts) with `"kind":"evidence"` and `"tool":"marge"`, so it carries the head SHA and the diff fingerprint. A second sweep on the same change writes nothing: the fingerprint, not the SHA, decides, so a Renovate rebase does not repeat the comment.

#### Actions

`--actions` runs a subset of the sweep steps, always in this order: `classify` (read the PR, its checks and markers, decide the state, write the label; always runs), `approve`, `merge`, `refresh` (update stale branches from their base), `retry` (re-run auto-cancelled CircleCI builds on the same head), `remedy` (apply the catalogue rule that matches the classification; see [Rules](#rules)), `mark` (write markers and evidence comments). `remedy` needs `mark` and is refused without it: the one-attempt-per-change guard reads the evidence marker, so a remedy that writes none repeats on every sweep. `--dry-run` decides every outcome and writes nothing, labels included.

#### Rules

The `remedy` step matches a classified PR against a catalogue of rules and applies the action the matching rule names. Rules live in this repository under `rules/`, one YAML document per file, and are **read at the start of every sweep from the default branch**. The binary does not embed them, so a merged rule is live on the next run without a release. `--rules-ref`, `--rules-repo` and `--rules-path` change where the catalogue is read from; `--rules-path` reads a directory on disk, which is how a rule is tried before it is merged.

A rule carries a detection signal, the name of one action, refusals it adds, and the evidence line written on the PR. The signal is a check-name glob (`match.check.name`), a bounded log-excerpt expression (`match.log`), PR metadata (`match.pr`) -- the title (`titlePattern`), the diff (`files`), or what the base head reported for the failing checks (`baseHead`) -- or the base branch's protection (`match.protection.missingContexts`), globs over the required contexts the head never reported. A `baseHead` condition must hold for every check the rule selected, so a PR carrying one transient failure and one real failure matches neither `green` nor `red`. A glob that matches everything is refused wherever one is accepted: a rule names at least one literal segment, or it is a classification restated rather than a signal.

```yaml
name: circleci-auto-cancel
summary: A build CircleCI itself cancelled established nothing about the code.
source: runbook row 74
match:
  states: [failed]
  check:
    name: "ci/circleci: *"
  log:
    source: circleci
    pattern: 'canceled'
action:
  name: circleci-retry
evidence:
  reason: the auto-cancelled build was retried on the same commit
```

The actions a rule may name are `update-branch`, `rerun-failed`, `circleci-retry`, `close`, `mark-wait`, `dispatch-align-workflow`, `fix-protection-context` and `strict-chain`. A new action is a Go change, reviewed as code.

`strict-chain` is **held**: it is the one action that merges, and no measured sweep has reported a PR blocked by the plain review rule. A rule may name it and validation accepts it, so the catalogue documents it, but the sweep refuses it and says so. Turning it on is a Go change, not a rule merged into the branch the sweep reads at run time.

Documents are decoded strictly, so an unknown key is an error. A rule whose only signal is a check name or a title is refused: a name says which job went red and a title says which dependency moved, neither says why, and `titlePattern: "."` reads every PR of a classification. Such a rule must carry a log signal, or read the PR's state rather than its text through `baseHead`, `files` or `match.protection.missingContexts`. A glob that matches everything is refused wherever one is accepted, so `files: ["**"]` is no way around it. **A rule cannot do what its action forbids**: each action enforces its own guards, a rule may only add refusals, and there is no syntax for removing one. The guard vocabulary is the trusted bot author, no failing security check, the required checks, checks settled on the head, one attempt per change, a log excerpt behind every write, and the generated files a remedy may never hand-edit. Each action carries the subset it needs, not all of them, and `marge rules validate` prints the set an action enforces. Rules are tried most specific first, by how much each one asks of a PR, and by name among equals; the first match wins. A broad rule therefore never shadows a narrow one, and a catalogue decides the same way however it was read.

A failure no rule recognises is grouped by signature in the sweep's JSON output under `unhandled`, most frequent first. `marge rules draft <signature> --from <report>` writes a rule skeleton and a pair of scenarios built from the PRs that carry it, then opens a draft pull request carrying the same files. The skeleton leaves the action blank, so it does not validate until a person names one; promoting a pattern is editing a draft rather than authoring one.

The pull request is opened through the API -- a tree, a commit, a branch and the pull request -- so marge needs no git workspace, no push credential and no language toolchain, and an agent running the sweep opens it with the token the sweep already uses. That token needs `contents: write` and `pull-requests: write` on the repository; a read-only sweep token is not enough. The branch is `rule/<name>`, one per rule, so a second draft of the same pattern reports the pull request already standing rather than opening another. `--repo` and `--base` say where it lands, and `--no-pr` writes the files and stops, printing the git and `gh` commands instead.

`marge rules validate` decodes and validates every document; `marge rules test` replays the recorded scenarios under `rules/testdata` against the catalogue. Both run in CI as ordinary Go tests, so a rule without one scenario that matches it and one that refuses it cannot merge. [docs/patterns.md](docs/patterns.md) carries the failure patterns the engine does not act on and the hazards behind the guards. [skills/marge-rescue](skills/marge-rescue/SKILL.md) condenses those hazards and the known-pattern hints for the model that runs when no rule matched. The AgentTemplate that loads it as a commit-pinned git source is not in this repository yet.

A rule that fails to validate is skipped and named on stderr and in the JSON `rules` object; the rest of the catalogue still runs. A catalogue that cannot be read at all refuses every remedy for that run and leaves classification, approval and merging unchanged. Every sweep reports the catalogue digest it ran, and every evidence comment names the rule and that digest.

#### Security check patterns

When a PR's CI fails, marge classifies the failure as security-related if any failing check's name contains one of the configured substrings (case-insensitive). Security failures are surfaced separately so they are not mistaken for ordinary build/test flakiness.

The built-in list contains: `security`, `govulncheck`, `trivy`, `codeql`, `snyk`, `gosec`, `gitleaks`, `semgrep`, `checkov`, `kics`, `vulnerability`, `vulnerabilities`, `sast`, `dast`, `dependency-review`, `dependency review`.

Pass `--security-patterns "Analyze"` to add to the list; it cannot be narrowed. The github/codeql-action template uses a job name like `Analyze (<lang>)` that the `codeql` substring will not match, so add `Analyze` if you rely on that template.

#### CI unavailable (Actions budget)

Sometimes a check reports `failure` not because the code is broken but because GitHub never started the job -- a personal account or organization has exhausted its Actions budget. GitHub surfaces this as a job that never reached the runner:

- `The job was not started because an Actions budget is preventing further use.`

Verified against the live GitHub API, such a block shows up as a check run with conclusion `failure`, empty `output`, and a single failure-level **annotation** whose `message` carries the text above (the message is in the annotation, not the output fields). marge therefore inspects each failed check run's annotations and matches that message -- which never appears for a genuine test, build, or lint failure.

When **every** failing check on a PR is such a block, marge classifies the PR as `CI unavailable (budget)` rather than `Failed`. It is counted separately, surfaced under its own section, and kept out of any rescue path -- the fix is to raise or await the Actions budget, not to touch the code. If a PR has a mix of a genuine failure and a budget block, it is still reported as `Failed`.

> Only this API-verified message is matched; any unrecognized block degrades to the normal `Failed` path rather than risking a real failure being hidden.

#### CI unavailable (no verdict)

A check also reports `failure` when it established nothing about the code. Two shapes are recognised, and each carries its own remedy in the detail:

- **The job was cancelled.** The check run concluded `cancelled`. This needs no message and costs no extra request: a job that was killed built, tested and scanned nothing. Rerun it.
- **CircleCI refuses the pipeline.** A repository whose `.circleci/config.yml` uses a setup workflow while the project setting that allows it is off gets `Use of setup workflows must be enabled in project settings (Project settings > Advanced -> Dynamic config using setup workflows)`. CircleCI reports it in the commit status description and, for the CircleCI GitHub app, in the check run output. No branch fix and no rerun resolves it: somebody has to change the setting once per repository.

A `timed_out` conclusion is deliberately **not** in this bucket. GitHub sets it when a job exceeds its time limit, which usually means the job ran and hung, and a dependency bump that deadlocks a test produces exactly that shape. It stays on the `Failed` path where the regression is visible.

When **every** failing check on a PR is such a check, marge classifies the PR as `CI unavailable (no verdict)` rather than `Failed`, with a detail naming each check and why: `CI unavailable (no verdict) (checks produced no verdict: publish / gitleaks (cancelled before it finished))`. It is counted separately, surfaced under its own section, and kept out of any rescue path. A PR with a mix of a genuine failure and such a check is still reported as `Failed`, and the check without a verdict is left out of the failure list.

This matters most for a security check. `Failed (security)` is the one category an operator is trained never to wave through, so a gitleaks job that was cancelled before it scanned anything would draw manual attention and discourage the rerun that actually fixes it. A security check that established nothing is therefore never reported as a security failure; a scan that ran and found a secret still is.

#### Obsolete bot PRs

Renovate leaves two open PRs for one dependency whenever a major lands while a minor is still open, and it opens a PR for a pinned action even when upstream moved only the tag. A failing or conflicted PR of either shape looks like work and is not. marge reports it as `Obsolete` and keeps it out of the action-required list. Two rules produce that verdict, and the report says which one fired:

- **Superseded.** Two or more open bot PRs in the same repository and on the same base branch whose titles name the same dependency: the one with the highest target version wins and every other one is reported as `Obsolete (superseded by #30 (v7.0.1))`. The comparison is semver on the titles, and a PR is only superseded on positive evidence -- both titles must name a dependency and a target version, both versions must parse, and equal versions supersede nothing. It is decided from the sweep's PR list alone, so it costs no request.
- **No-op.** A PR whose diff changes nothing that executes. The rule is narrow on purpose: every changed file must be a `.github/workflows` or `.github/actions` YAML file, and every changed line must be a `uses:` line pinned to a full commit SHA whose pin is unchanged and whose trailing version comment is all that moved. Anything else -- a moved SHA, a tag ref, a re-indented line, a file elsewhere, a patch GitHub truncated -- keeps the PR as it was. The diff is fetched only on the paths that would otherwise report work, and only when the supersession rule did not already fire.

```
-      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5
+      - uses: actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09 # v5.1.0
```

Neither rule suppresses a merge: a green PR still merges, whatever its siblings carry. The sibling can be a major that CI rejects, and the safe lower bump must still land. The verdict only replaces a failure or a conflict, where the remedy is to close the PR rather than to rescue it.

marge never closes a PR itself. Two PRs can legitimately carry the same dependency name in one repository, for instance across the manifests of a monorepo, so the decision stays with a human. PRs found through a GitHub issue search carry no base branch, because the search API does not report one; those group as if they shared a branch. Sweeps driven by `--repos-file` or `repos` know the base branch and group by it.

#### Conflicts a sweep caused itself

Two bot PRs of one repository often edit the same file. The PRs of a repository are processed one at a time, so the first merge makes the second one `dirty`. The conflict is real, but it is not work for anybody: the bot rebases its own PR within about a minute, and the PR is mergeable again.

marge reports such a PR as `Awaiting rebase` (label `marge/pending`), never as a conflict, and names the sibling that caused it: `conflicted by #6, merged in this run; the bot rebases it`. No rule of the catalogue acts on that state, so no rescue is started.

The sweep then looks at the PR again in a second pass, after the rest of the run. It waits for the bot to rebase, up to three minutes for the whole pass, and processes each rebased PR as usual: green and eligible, it merges in the same run. Without that pass the PR would wait for the next sweep, which is a day later, and each further PR of the repository would wait behind it. A PR the bot has not rebased when the wait runs out keeps `Awaiting rebase` and the next sweep decides.

A conflict on a PR of a repository this run did not merge into is unchanged: it is reported as `Conflict` and counted as action-required. A PR with GitHub auto-merge enabled needs none of this, because GitHub merges it when the rebase lands.

#### Stale failures (already fixed on the base branch)

A dependency PR is built once and then sits in the queue while the base branch moves on. When a fleet-wide fix lands on `main` -- a CVE bump that turned every open Go PR's vulnerability scan red, a linter pin, a CI infra repair -- the PR's last build stays red although the failure no longer exists. Without help, an operator (or a rescue agent) spends time diagnosing a failure that a branch refresh would have cleared.

marge recognises this case. When a PR's checks fail, it:

1. compares the PR head with its base branch (`GET /repos/{owner}/{repo}/compare/{base}...{head}`) and continues only if the head is **behind** (`behind_by > 0`);
2. looks up the latest run of **every failing check** on the base branch head (commit statuses and check runs);
3. classifies the PR as **`Stale`** instead of `Failed` when all of them are green there, e.g. `Stale (go-build green on main since 2026-09-05 10:57 UTC, 12 behind)`.

A PR that is behind but whose failing check is also red on the base branch stays `Failed` -- the failure is real on `main` too. So does a PR whose failing check does not exist on the base branch at all (a PR-only workflow cannot be proven green), and a PR that is not behind. Any lookup error keeps the `Failed` classification; a stale verdict is only ever reached on positive evidence. The check runs before the security split, so a stale govulncheck or Trivy failure is recognised like any other.

Stale PRs are listed in their own **Stale** section, counted separately from failures, and kept out of the action-required list -- the remedy is a refresh, not a rescue. With the **`refresh`** action (selected by default; `--actions` narrows it) marge performs that refresh itself: it calls `PUT /repos/{owner}/{repo}/pulls/{number}/update-branch` (the same merge commit as GitHub's "Update branch" button) and reports the PR as **`Refreshed (re-checking; ...)`**. CI re-runs against current code and the next sweep merges the PR if it is green -- or reports it as `Failed`, because it is no longer behind.

Two guards apply to the refresh:

- `--dry-run` prints the `Stale` classification but never calls update-branch.
- A PR carrying a **non-stale [rescue marker](#rescue-markers-prior-ai-rescue-attempts)** is not refreshed (`Stale (...; refresh skipped: fresh rescue marker)`). The marker pins the change an automated rescue already failed on -- whether the branch was rebased since or not -- so re-running CI against a newer base cannot help. The marker is shown on the entry so the operator can decide. A stale marker (the PR content changed since the attempt) does not block the refresh.

The heuristic is deliberately cheap: `main` being green does not prove the bump itself is innocent (a major bump can be red for its own reasons while `main` is fine). The cost of a wrong `Stale` verdict is one branch refresh and one CI run, after which the PR is no longer behind and is classified on its own merits.

#### Cancelled builds (CircleCI auto-cancel)

CircleCI cancels a running build on its own when a newer pipeline starts on the same branch (Renovate pushed a rebase or a new version while the previous build ran) or when it detects a redundant workflow. The cancelled build still posts `failure` -- "Your tests failed on CircleCI" -- to the commit it was building, and the status description carries no hint of the cancellation. Read at face value, the PR looks like a real failure and costs an operator (or a rescue agent) a diagnosis that ends in "nothing is wrong, just retry".

marge looks behind the status. When **every** failing check of a PR is a commit status whose `target_url` points at a CircleCI build (`https://circleci.com/gh/<owner>/<repo>/<build>`), it fetches that build from the v1.1 API (`GET /api/v1.1/project/github/<owner>/<repo>/<build>`) and inspects its steps. Verified against the live API: an auto-cancelled build reports `status: failed` and `canceled: false` at the top level, exactly like a real failure -- but its steps are all `success` up to the cancellation and only `canceled` from there on, whereas a real failure has a step with `status: failed`. A build cancelled before any step ran reports `status: canceled` instead. Both shapes classify the PR as **`Cancelled`** rather than `Failed`:

- **On the current head** -- `Cancelled (build 1263 auto-cancelled; retry needed)`: the commit has no verdict yet. With the **`retry`** action (selected by default; `--actions` narrows it) marge calls `POST /api/v2/workflow/<workflow_id>/rerun` with `{"from_failed": true}` on the workflow the build belongs to (its `workflows.workflow_id`, part of the v1.1 build JSON), and reports the PR as **`Retried (re-checking; workflow build rerun from failed)`**. The next sweep reads the real result. The workflow rerun also releases the jobs the cancel left `blocked` or `not_run`; the v1.1 single-build retry does not, so on a repository whose branch protection requires those downstream contexts the PR would go green on the retried job and stay unmergeable. Each distinct workflow is rerun once, however many of its jobs were cancelled. CircleCI reruns from a failed job, so two cases have none -- a build outside a workflow, and a workflow cancelled before any job failed, which the endpoint turns down with `400` -- and both fall back to `POST /api/v1.1/project/github/<owner>/<repo>/<build>/retry`, reported as `Retried (re-checking; build 1263 retried as 1272)`. A rerun refused for any other reason, an expired token for instance, leaves the PR `Cancelled` with the error: the retry would meet the same wall.
- **Behind a newer head** -- `Cancelled (build 1244 (2c0ce64) auto-cancelled; head is now 605a2d6, its build is the verdict)`: the branch moved on and the new head's own build decides. Nothing is retried.

Cancelled PRs are listed in their own **Cancelled** section (and **Retried** once retried), counted separately from failures and kept out of the action-required list. The lookup is lazy -- nothing is fetched unless a failing status points at CircleCI -- and it is decided before the stale and security classifications because it rests on positive evidence about the very build that failed. A PR with a mix of a cancelled build and a genuine failure (a failing GitHub Actions check run, a CircleCI build with a failed step) stays `Failed`. `--dry-run` classifies but never retries.

Public CircleCI projects can be inspected without credentials. Private projects and every rerun endpoint need a CircleCI API token (see [Setup](#setup)). Without one, a private project's build cannot be inspected and the PR keeps today's `Failed` classification, annotated: `Failed (checks failed: ci/circleci: go-build; ci/circleci: go-build: build 1263 could not be inspected (HTTP 404: Build not found; no CircleCI token configured (private project?)))`. A cancelled build on the current head with the `retry` action but no token is reported as `Cancelled (...; retry skipped: no CircleCI token configured)`.

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

### `marge sweep (--team <name>... | --query <text>) [flags]`

Sweeps one scope without interactive grouping. Exactly one scope is given:

- `--team <name>` reads the team's repositories from `repositories/team-<name>.yaml` in the team-file repository (`giantswarm/github` unless `MARGE_TEAM_FILE_REPO` names another `owner/repo`; each entry's `name` is read, and its `botPRsSweep` key when it has one) and sweeps their open bot PRs under the team's [policy](#sweep-policy). It takes several names, repeated or comma-separated: each team is swept under its own repositories and its own policy, and the teams are reported one after the other. One team's unreadable policy fails that team alone; every other team still runs, and the command exits non-zero at the end. Naming several teams refuses `--prs`, which narrows a single scope.
- `--query <text>` runs marge's GitHub search the way `marge [query]` does, for personal repositories and organisations without a team file; `--org` and `--repos-file` belong to this scope. The query scope has no team, so the company defaults apply on their own.

A sweep does not wait for pending checks: a `Waiting for checks` PR is reported and the next sweep decides. `--check-timeout` opts into a wait.

The live table shows every PR's outcome, including the failure reason and any ai-rescue marker, followed by a one-line summary. With `--no-tui` the results are printed as plain-text groups: **Merged**, **Security failures**, **Failed**, **Stale** and **Refreshed**, **Cancelled** and **Retried**, **Obsolete** (bot PRs a higher-version sibling replaces, and bot PRs whose diff changes nothing that executes, see [Obsolete bot PRs](#obsolete-bot-prs)), **Waiting for checks**, **CI unavailable (Actions budget)**, **CI unavailable (no verdict)** (PRs whose failing checks established nothing about the code, see [CI unavailable (no verdict)](#ci-unavailable-no-verdict)), and **Skipped**. With `--output json` the same structure the MCP `sweep` tool returns is printed, including `repositories_failed` for repositories whose PRs could not be listed.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--team` | | | Team whose repositories are swept (team scope); repeatable, or comma-separated, for several teams |
| `--post-summary` | | `false` | Post each swept team's summary to the Slack channel its policy names; a scheduled run sets it |
| `--query` | | | GitHub search text (query scope) |
| `--actions` | | _(all)_ | Comma-separated sweep steps to run, in fixed order: `classify`, `approve`, `merge`, `refresh`, `retry`, `remedy`, `mark` (see [Actions](#actions)) |
| `--dry-run` | | `false` | Decide every outcome, write nothing |
| `--check-timeout` | | `0` | How long to wait for one PR's pending checks; zero reports the PR as waiting |
| `--rules-repo` | | `giantswarm/marge` | Repository the rule catalogue is read from, as `owner/name` |
| `--rules-ref` | | `main` | Branch the rule catalogue is read from |
| `--rules-path` | | | Read the catalogue from this directory instead of the repository |
| `--watch` | `-w` | `false` | Keep polling for new PRs every 60 seconds |
| `--org` | | | Limit to repos owned by this org or user (query scope) |
| `--repos-file` | | | File with `org/repo` entries (one per line; blank lines and `#` comments are ignored) to scan instead of searching GitHub (query scope) |
| `--prs` | | | Sweep only these pull requests of the scope, each a PR URL or `owner/repo#number` (repeatable, or comma-separated). It is a scope of its own: given alone, the repositories of the listed PRs are read |
| `--no-tui` | | `false` | Disable the live table; print plain-text results instead |
| `--output` | | `table` | `table` or `json` |
| `--merge-auto` | | `false` | Also merge PRs that have auto-merge enabled (by default the merge itself is left to GitHub) |
| `--security-patterns` | | _(built-in)_ | Add to the built-in security check pattern list (see [Security check patterns](#security-check-patterns)) |

### `marge mark <pr-url> [flags]`

Records a failed AI rescue attempt on a PR by posting an [ai-rescue marker](#rescue-markers-prior-ai-rescue-attempts) comment. The marker captures the PR's current head SHA plus a fingerprint of its diff (`patch_id`) and, for dependency PRs, of its title (`change_id`), so it goes stale when the PR content changes but survives a Renovate rebase that leaves the diff unchanged. The confirmation line lists what was pinned, e.g. `Marked my-org/my-repo#42: rescue blocked (head 1be1ed9d, patch_id 5ad45e13d66acc2b, change_id typescript@v7)`.

| Flag | Default | Description |
|------|---------|-------------|
| `--outcome` | `failed` | Rescue outcome: `failed` (attempted, could not fix) or `blocked` (fix known but waits on something external) |
| `--reason` | | Short explanation of why the rescue did not succeed |
| `--tool` | `ai` | Name of the tool/agent that attempted the rescue (e.g. `klaus`) |
| `--dry-run` | `false` | Show the marker that would be written, head SHA and fingerprint included, and post nothing |

```bash
marge mark https://github.com/my-org/my-repo/pull/42 \
  --tool klaus --reason "nock v14 is ESM-only, needs Jest ESM migration"
```

Requires the token to have **Issues: Read & write** (comment) permission in addition to the permissions listed under [Setup](#setup).

### `marge serve [flags]`

Starts an MCP server exposing four tools: `list`, `sweep`, `remedy` and `mark`. Each one is an adapter over the same engine call the CLI makes, with the same guards, so a tool call and the equivalent `marge sweep` invocation do the same thing. Every tool that can change a PR takes `dry_run` and carries the MCP read-only and destructive annotations, so a client knows what a call costs before making it.

- **`list`** -- read-only. The open bot PRs of a scope with the classification of each. The result has the same shape as `sweep`'s, so "what is waiting for us" and "what would a sweep do" are one question. `refresh` chooses where the classification comes from, and the two readings cost very different amounts:
  - `refresh: false` (the default) reads back the classification the last sweep stored in the PR's `marge/<class>` label, which the PR search already carries. The whole call costs the search, whatever the number of PRs. Several states share one label, so the `status` is the class's representative; the evidence, `update_type`, `policy` and `rescue` each need the PR itself and are left out. A PR no sweep has labelled is reported under `unclassified` rather than guessed at.
  - `refresh: true` classifies every PR again: the `classify` step alone on a dry run, with the evidence, the update type, the resolved policy and the prior-rescue state of each. Nothing is approved, merged, refreshed, retried, remedied or labelled. It costs a check read per PR.
- **`sweep`** -- mirrors `marge sweep`, returning structured JSON (`rules`, `unhandled`, `summary`, `merged`, `security_failures`, `action_required`, `stale`, `refreshed`, `cancelled`, `retried`, `obsolete`, `waiting`, `ci_unavailable`, `ci_no_verdict`, `skipped`, `eligible`, `unclassified`, `repositories_failed`). Each `obsolete` entry carries a `reason` of `superseded` or `no_op`. `team` selects the team scope; `query`, `org`, `repos` (a list of `org/repo` entries) and `repos_file` (a file in the `--repos-file` format) belong to the query scope and are refused together with `team`. `actions` selects the sweep steps like `--actions`, and `prs` narrows the run to the listed PRs (each a PR URL or `owner/repo#number`) like `--prs`; the scope still decides which repositories are read and under which policy, so a PR outside it is refused rather than swept. Each PR entry includes `policy` (the resolved policy the PR was decided under, with the `sources` that produced it), `kind`, `update_type`, `label` (the label on the PR after the sweep; absent when nothing was written, as in `dry_run`), `created_at`, `age_days`, and -- when a prior rescue attempt was found -- a `rescue` object (`tool`, `outcome`, `reason`, `at`, `stale`, `rebased`). `rebased: true` means the PR head moved since the attempt but the diff did not (a Renovate rebase); such a marker is still valid and `stale` is `false`. Agent orchestrators should dispatch on `action_required` only, skip entries whose rescue is not `stale` (rebased or not), and escalate those to a human.
- **`remedy`** -- classifies one PR and applies the catalogue rule that matches it, through the action that rule names and that action's own guards. It is `classify,remedy,mark` on a single PR, so the refusals and the evidence comment are the sweep's. `rule` narrows the catalogue to one rule, which must still match the PR: naming a rule removes the others from the contest, it never forces an action onto a PR. `team` decides the PR under that team's policy.
- **`mark`** -- mirrors `marge mark`, so rescue agents can record their own failed attempts. The result echoes what was pinned: `head_sha` plus `patch_id` and `change_id` when they could be computed.

`rescue`, the fifth tool of the plan, waits for the engine's rescue step in [roadmap#4360](https://github.com/giantswarm/roadmap/issues/4360); there is nothing to adapt until it exists.

| Flag | Default | Description |
|------|---------|-------------|
| `--transport` | `stdio` | `stdio` (JSON-RPC over stdin/stdout, for an MCP client that starts marge itself) or `streamable-http` (the MCP Streamable HTTP transport, what the Helm chart runs) |
| `--http-addr` | `:8080` | Listen address for `streamable-http` |

Over `streamable-http` the MCP endpoint is `/mcp`; `/healthz` and `/readyz` answer the Kubernetes probes. The server stops on `SIGTERM`/`SIGINT` after draining in-flight requests for up to ten seconds.

**The two transports act as different people, because they serve a different number of them.** A `stdio` server is started by the person using it and serves that one person, so it acts with their own credential: the App when the environment carries one, otherwise `GITHUB_TOKEN`, `GH_TOKEN` or `gh auth token`. A `streamable-http` server serves everyone who reaches it, so only a per-request credential can be right: it acts with the token the caller presented as a `Bearer`, which is what muster attaches from the person's GitHub grant. A call that carries no token is answered with a sign-in error and never with a credential the process happens to hold -- a shared server that fell back would attribute one person's merges to whoever configured it.

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

Preview a team's sweep, then apply it:

```bash
marge sweep --team bumblebee --dry-run
marge sweep --team bumblebee
```

Sweep a handful of teams in one run and read one report:

```bash
marge sweep --team bumblebee,atlas --team phoenix
```

Relabel a team's queue without approving or merging anything:

```bash
marge sweep --team bumblebee --actions classify
```

Sweep a personal organisation, including PRs with auto-merge enabled:

```bash
marge sweep --query "" --org my-org --merge-auto
```

Print the sweep result as JSON:

```bash
marge sweep --team bumblebee --output json
```

## How it works

1. Resolves the scope and the [policy](#sweep-policy): with `--team`, the repositories of the team file in the team-file repository, the company default file and the team's own; otherwise the GitHub search for open PRs by the four bots that request your review or live in your repositories, or the repositories of `--repos-file`, under the company defaults. A repository whose PRs cannot be listed is reported, never silently dropped.
2. In interactive mode, groups results by repository (or dependency) and presents a selector.
3. For each PR, in parallel (the policy's `concurrency`, by default 5 repositories at a time and one PR per repository):
   - Reads the PR, its kind and update size, its checks, the base branch's required status checks and its markers.
   - Applies the [guards](#guards) in order: repository swept at all, trusted author, auto-merge, security check, required checks, red non-required checks, eligibility.
   - Approves the PR if not already approved, then squash-merges it; a PR behind its base is brought up to date instead.
   - On a failure, looks behind failing CircleCI statuses (auto-cancelled builds are `Cancelled` and retried by the `retry` action) and compares the failing checks with the base head (fixed there already is `Stale` and refreshed by the `refresh` action).
   - Writes the classification label and, where it acted, an evidence comment.
4. Displays a live-updating table with columns for repository, dependency, version, age, author, and status. Failing entries are annotated with any prior AI rescue attempt found on the PR. Use `--no-tui` for plain-text output or `--output json` for the structured result.

## Development

```bash
make build          # Build the binary
make test           # Run tests
make lint           # Run golangci-lint
make help           # Show all available targets
```

## License

MIT -- see [LICENSE](LICENSE) for details.
