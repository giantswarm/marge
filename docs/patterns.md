# Failure patterns

This document carries the sweep's knowledge of bot PR failures. Rules under
`rules/` hold the patterns the engine acts on by itself; this page holds the
ones a person or an agent still decides, and the hazards that make a wrong
decision easy.

The condensed form a model reads when no rule matched is
[`skills/marge/references/hazards.md`](../skills/marge/references/hazards.md).
A pattern promoted to a rule loses its hint there in the same pull request.

Every row of the runbook extraction is a rule under `rules/`, a row on this
page citing its runbook row, or one of the thirteen named below. That is the
condition for retiring the runbook.

Rows 78, 80, 82, 83, 84, 85, 95, 102, 106 and 108 to 111 are **deliberately
not carried**. They describe hazards of driving the sweep from a shell: a
CircleCI token read from a local file, `set -- $var` not word-splitting under
zsh, `commit.gpgsign` dropping a fix commit, `gh` losing its token mid-run,
scratch clones on a tmpfs, and the rescue runtime's own parameter names. The
engine has none of them. It makes no local commit, runs no shell and reads
no local credential file, so writing them down here would document a
workflow this repository exists to replace. The one with an engine-side
answer, row 93, is in the Hazards section under `checks-settled`.

## Patterns the engine acts on

These are rules. The file under `rules/` is the specification; this table is
the index.

| Pattern | Rule | Action | Runbook row |
|---|---|---|---|
| A CircleCI build the platform cancelled | `circleci-auto-cancel` | `circleci-retry` | 74 |
| cosign transparency-log conflict | `cosign-transparency-log-conflict` | `circleci-retry` | 32 |
| A release asset 404 moments after publication | `release-asset-404-race` | `rerun-failed` | 33 |
| An action or tool download that failed | `actions-runner-download-error` | `rerun-failed` | 86 |
| A dropped Go module-proxy connection | `go-module-proxy-stream-error` | `rerun-failed` | observed 2026-09-14 |
| A failure already green on the base head | `stale-failure-green-on-base` | `update-branch` | 68 |
| nancy guide-API error on an old orb | `nancy-guide-api-orb-delta` | `update-branch` | 37 |
| A nancy finding the base head carries too | `cve-fix-on-base` | `dispatch-cve-workflow` | 23, 51, 81, 98 |
| A tag older than the pseudo-version in use | `renovate-downgrade-to-older-tag` | `close` | 16 |
| A self-replace rewritten to a wrong major | `mangled-replace-major` | `close` | 38 |
| A required context nobody reported | `required-check-name-drift` | `fix-protection-context` | 21, 44 |
| A bump waiting on an upstream release | `ecosystem-not-ready` | `mark-wait` | 7, 9, 15, 31 |
| gosec run without the repository configuration | `upstream-orb-gosec-fixtures` | `mark-wait` | 63 |

Rows 5, 6, 86, 89 and 90 of the extraction state these patterns in their
general form -- a stale branch, a transient failure, a job that never ran, a
failure already fixed on the base, the release-asset race. The rules above
are those rows with a log signal attached, which is what the engine needs to
act on one.

A failure no rule recognises is grouped by signature in the sweep report
under `unhandled`, and left on the PR as a marker carrying that signature.
`marge rules signatures` counts the markers by signature, and `marge rules
draft <signature>` writes a rule skeleton and a pair of scenarios from the
PRs that carry it, then opens a draft pull request on `rule/<name>`. The
skeleton leaves the action blank, so it does not validate until a person
names one.

## Patterns that need a person

A remedy here is mechanical but the engine has no action for it yet, or the
decision is not mechanical at all.

### Written on the branch by a person or an agent

These rows change a file on the PR branch, so each one needs a git workspace,
a push credential and a language toolchain. The engine has none of the three
and does not get them. Over the sweeps of 2026-09-18 to 2026-09-21, one pull
request reached one row of this table and eleven rows reached none, which does
not justify that capability class. The remedy is `skills/marge` in Claude
Code, where the workspace, the toolchain and the credential are the person's
own. PRD decision 31 carries the counts.

| Pattern | Remedy | Runbook row |
|---|---|---|
| A stale `go.sum` Renovate did not tidy | `go mod tidy`, commit, push | 1 |
| A pre-commit hook that modified files (`go-mod-tidy`, `helm-docs`, `helm-schema-<chart>`, `end-of-file-fixer`) | Run the hook with its exact arguments, commit the result | 25, 41, 54, 64 |
| A generated Dockerfile that drifted | Run the repository's generate target on the branch | 35 |
| A vendir bump without the repository's post-sync step | Run the repository's sync target with mikefarah `yq` | 71 |
| The architect v9 `multiarch` argument | Delete the argument from every `push-to-registries` invocation | 19 |
| A `TEAM-NAME` placeholder in `Chart.yaml` | Replace it with the team from `CODEOWNERS` | 42 |
| A staticcheck SA1019 deprecation after a bump | Rename the call site | 62 |
| `github.Ptr` rejected by the `inline` analyzer | Replace it with the `new` builtin | 73 |
| A custom Makefile target referencing a file the alignment migration deleted | Drop the file from the target on `teams-alignment-branch`; `Makefile.custom.mk` is the repository's own | 24 |
| A chart icon the `abs` validator rejects (`C0004: IconDomainIsValid`) | `giantswarm-validator-ignored-checks: C0004` in `.abs/main.yaml` | 28 |
| ATS refusing an unknown cluster type in its pre-run | Add a minimal `.ats/main.yaml` | 50 |
| A placeholder PEM header in vendored chart docs tripping `gitleaks` | An allowlist regex in the repository's `.github/.gitleaks.toml` | 70 |
| A consuming chart pinned outside the range its HelmRelease follows | Add the entry the test names to `testdata/consumption/charts.yaml` | observed 2026-09-20 |
| A golden render that drifted from the default branch | Run the repository's own verify target and commit what it rewrites | observed 2026-09-18 |

The last two rows come from the sweeps themselves and from no runbook row.
Both are a repository's own generate target rather than a shared hook, and
both reached more pull requests than any row above them.

The CVE rows are gone from this table. `cve-fix-on-base` dispatches the
repository's own `Fix Go vulnerabilities` workflow on the base branch;
nancy-fixer then performs the bump, the replace pin and the time-boxed
`.nancy-ignore` entry, and the shared workflow opens the PR under the Herald
App. marge sweeps those PRs as a bot PR kind and never re-implements the
remedy (PRD decision 3).

### Fixed on the default branch first

The PR is blocked by a defect in the repository that the bump merely
exposes. The fix is a PR on the default branch; the blocked PRs then need
`update-branch`, which the engine does apply.

| Pattern | Where the fix goes | Runbook row |
|---|---|---|
| architect v9 multi-arch default breaking a Dockerfile that is not arm64-ready | Pin `platforms: linux/amd64` on the default branch | 29 |
| `check-values-schema` rejecting an empty `values.yaml` | Make the values file `{}` on the default branch | 34 |
| A required check the Align files PR has not delivered yet (`pre-commit`, `semantic-pull-request`) | Land the align PR, then refresh each blocked PR | 36, 46 |

### Needing a person or an agent

| Pattern | Why it is not mechanical | Runbook row |
|---|---|---|
| A breaking Go API across a major bump | The migration is a code change, spread over call sites | 3, 52, 61, 67, 72 |
| An ESM-only major with a CJS consumer | The fix is usually a `patch-package` shim, chosen case by case | 11 |
| A lint rule that tightened | Mechanical but spread over files, and sometimes a configuration decision | 10, 17 |
| Entangled bumps that only pass together | The remedy is to fold several PRs into one branch | 40, 65, 66 |
| A chart validator that newly rejects a chart | The fix is a CI values file, never a weakened guard | 22, 27 |
| An alignment migration that renamed a built binary | The Dockerfile needs a per-architecture selection | 43 |
| A security CVE in a transitive dependency with no clean path | The upgrade is a judgement about the dependency tree | 2 |
| A CI configuration defect unrelated to the bump | Diagnosis comes before any remedy | 4 |
| OCM bundle-wiring drift after several per-app releases | One combined catch-up PR, and usually a second round | 13, 103 |
| A chart test failing in the apptestctl bootstrap (`no matches for kind "PodMonitor"`) | Merging past it is a decision, and only after the log confirms the bootstrap error | 18 |
| An OCI artifact a component references but that was never pushed | The reference and the version must move in lockstep; the check is a repository defect | 45, 92 |
| A major bump whose module system changed (`js-yaml` v5, ESM default export) | A small code change, spread over call sites | 53 |
| A chart test that deploys and then goes silent until it times out | Merging past it needs the hang shape confirmed in the build log | 58 |
| `helm dependencies update` failing on a vendir-vendored subchart | The vendor target needs a person; the PR is marked and left | 60 |
| A test environment whose DOM implementation changed (`jsdom` v30) | A shared test setup file, written case by case | 77 |
| An Align files PR blocked by the repository's own content | The per-repo rescue prompt, not the alignment payload | 107 |

### Held by a team decision

Nothing is wrong with the PR. Someone has to decide, and the sweep must not
decide for them.

| Pattern | Why the sweep leaves it | Runbook row |
|---|---|---|
| Upstream removed an API the consumer should stop using | The consumer's own change has to land first | 56 |
| A vendored upstream chart going MAJOR | The repository owner owns that upgrade | 57 |
| Renovate autoclosing a bump as a no-op after a sibling merged | The autoclose is correct; read the timeline actor before restoring anything | 75, 91 |
| A dependency held by `allowedVersions` | The hold is the decision; the sweep reports and skips | 55 |

### Belonging upstream

The remedy is a change in another repository. The sweep marks the PR and
leaves it; fixing the consumer would be undone on the next generation.

| Pattern | Where the fix belongs | Runbook row |
|---|---|---|
| A shared `zz_generated` template defect | `giantswarm/github` | 12, 49 |
| An orb defect (`nancy` stdin limit, gosec `--no-config`, cold-cache timeouts) | `giantswarm/architect-orb` | 26, 47, 63 |
| A required check that is path-filtered and can never report | `devctl` | 20 |
| A generic eslint hook on a bespoke repository | `repositories/override/<repo>/` in `giantswarm/github` | 49 |
| A vendored file tripping a pre-commit hook | `repositories/override/<repo>/` in `giantswarm/github` | 59 |
| Renovate managing a vendored subtree | `renovate-custom.json5` in the repository | 39 |
| A deprecated repository still receiving bumps | `lifecycle: deprecated` on the repository entry | 79 |
| Two Renovate managers matching one image line | Delete the redundant repo-local regex manager | 76 |
| Many pin PRs from a curate-generated chart | Renovate grouping, so one PR carries every pin | 69 |
| A toolchain-version wave stalling the fleet on the linter | `devctl`, then `dispatch-align-workflow` per repository | 96 |
| The same check failing across unrelated repositories in one sweep | Shared CI in `giantswarm/github` or an action it calls | 100, 104 |
| A CircleCI project setting refusing the whole pipeline | The project setting; the sweep reports it as no verdict | 101 |
| A central template change waiting to reach the repositories | The Align files workflow dispatch | 105 |

## Mechanical, and out of the engine's reach

The runbook calls these mechanical, and they are. The engine still cannot
act on them, for a reason that is a property of the engine rather than of
the pattern. Each says which.

| Pattern | Why no rule expresses it | Runbook row |
|---|---|---|
| An archived repository still carrying open bot PRs | The sweep never sees them. Discovery searches `is:pr is:open archived:false`, so the PRs are filtered out before classification, and an archived repository is read-only in any case | 8 |
| An architect-orb bump on a devctl-managed repository | The signal is the repository's own CircleCI config, not anything on the PR. A rule reads the diff, the checks and the logs, never a file on the default branch. The runbook's remedy also ends in "coordinate with the operator" | 30 |
| A transitive-only `/v2` un-pin leaving a vulnerable v1 path | The signal is `go mod why` reporting that the main module does not need the package, which the engine cannot run. The nancy line alone does not separate a pin floor that still holds from a real finding, and the action would be `close` | 48 |

## Hazards

These are the ways a reasonable-looking decision goes wrong. They are the
reason several guards exist. Each cites the runbook row it comes from.

- **A check name is not a diagnosis, and neither is a title** (rows 99,
  §2). Read the
  failing step's log. Never classify from a check name, a title, a
  repository, or a previous sweep's table. Validation refuses a rule whose
  only signal is one of those: a rule needs a log signal, or a `baseHead`,
  `files` or `match.protection.missingContexts` signal that reads the PR's
  state. A glob that matches everything is refused wherever one is
  accepted, so `files: ["**"]` is no way past it.
- **Absent is not green** (row 89). A base branch that never ran a check has not
  passed it. A job filtered off the default branch makes "green on main"
  meaningless.
- **A green security check is not evidence that a scan ran** (rows 81, 97). When a scanner
  is broken fleet-wide, a green result may mean it audited nothing.
- **A missing CircleCI context is a stop** (row 87). A build can fail on the exact
  head without ever posting its status. Cross-check the build's
  `vcs_revision`; never merge past the gap.
- **Twice red on the same commit is real** (row 86, §2). One rerun or retry per change.
  That is the `once-per-change` guard.
- **The branch name is part of the branch** (row 88). A failure that reproduces only
  on one branch is not always the dependency: a long branch name has broken
  chart labels before.
- **Teammate work comes first** (§2). Before a migration, read the repository's
  open PRs and recent commits. An open teammate PR on the same subsystem is
  a stop, not a merge conflict to route around.
- **A merged rule is live on the next sweep.** Nothing gates the catalogue
  but review on this repository: there is no signature and no release. That
  is the point, and it is why `strict-chain`, the one action that merges, is
  **held** in the registry until a measured run shows PRs that need it. A
  rule may name it and validation accepts it; the sweep refuses it and says
  so, so the gate is code rather than the absence of a rule.
- **A context that has not reported is not a context nobody posts** (rows
  36, 46, 93, 94). A
  queued workflow reports nothing, which is what drift looks like. The
  `checks-settled` guard waits for the head to finish reporting, and a
  drift rule names the context shapes it diagnoses so the protection write
  touches those and no others.
- **A generated file is never hand-edited** (§2). `zz_*`, the generated
  `renovate.json5`, the align-managed `.pre-commit-config.yaml` and
  `.circleci/workflows.yml`. `Makefile.custom.mk` and `Chart.yaml` are the
  repository's own and may be written.
