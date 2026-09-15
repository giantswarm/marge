# Failure patterns

This document carries the sweep's knowledge of bot PR failures. Rules under
`rules/` hold the patterns the engine acts on by itself; this page holds the
ones a person or an agent still decides, and the hazards that make a wrong
decision easy.

Every row cites the runbook row it came from, so the runbook can be retired
once every row lives here or in a rule.

## Patterns the engine acts on

These are rules. The file under `rules/` is the specification; this table is
the index.

| Pattern | Rule | Action | Runbook row |
|---|---|---|---|
| A CircleCI build the platform cancelled | `circleci-auto-cancel` | `circleci-retry` | 74 |
| cosign transparency-log conflict | `cosign-transparency-log-conflict` | `circleci-retry` | 32 |
| A release asset 404 moments after publication | `release-asset-404-race` | `rerun-failed` | 33 |
| An action or tool download that failed | `actions-runner-download-error` | `rerun-failed` | transient CI |
| A failure already green on the base head | `stale-failure-green-on-base` | `update-branch` | 68 |
| nancy guide-API error on an old orb | `nancy-guide-api-orb-delta` | `update-branch` | 37 |
| A tag older than the pseudo-version in use | `renovate-downgrade-to-older-tag` | `close` | 16 |
| A self-replace rewritten to a wrong major | `mangled-replace-major` | `close` | 38 |
| A required context nobody reported | `required-check-name-drift` | `fix-protection-context` | 21, 44 |
| A bump waiting on an upstream release | `ecosystem-not-ready` | `mark-wait` | 7, 9, 15, 31 |
| gosec run without the repository configuration | `upstream-orb-gosec-fixtures` | `mark-wait` | 63 |

## Patterns that need a person

A remedy here is mechanical but the engine has no action for it yet, or the
decision is not mechanical at all.

### Waiting on a branch-writing remedy

These rows need a git workspace, a push credential and a language toolchain,
which the sweep does not have. They become rules when those actions exist.

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
| Transitive CVEs with a clean upgrade path | `go get` the parents, tidy, push | 23 |
| A CVE with no fixed release | A time-boxed `.nancy-ignore` entry with a justification | 51 |

### Needing a person or an agent

| Pattern | Why it is not mechanical | Runbook row |
|---|---|---|
| A breaking Go API across a major bump | The migration is a code change, spread over call sites | 3, 52, 61, 67, 72 |
| An ESM-only major with a CJS consumer | The fix is usually a `patch-package` shim, chosen case by case | 11 |
| A lint rule that tightened | Mechanical but spread over files, and sometimes a configuration decision | 10, 17 |
| Entangled bumps that only pass together | The remedy is to fold several PRs into one branch | 40, 65, 66 |
| A chart validator that newly rejects a chart | The fix is a CI values file, never a weakened guard | 22, 27 |
| An alignment migration that renamed a built binary | The Dockerfile needs a per-architecture selection | 43 |

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

## Hazards

These are the ways a reasonable-looking decision goes wrong. They are the
reason several guards exist.

- **A check name is not a diagnosis, and neither is a title.** Read the
  failing step's log. Never classify from a check name, a title, a
  repository, or a previous sweep's table. Validation refuses a rule whose
  only signal is one of those: a rule needs a log signal, or a `baseHead`,
  `files` or `match.protection.missingContexts` signal that reads the PR's
  state. A glob that matches everything is refused wherever one is
  accepted, so `files: ["**"]` is no way past it.
- **Absent is not green.** A base branch that never ran a check has not
  passed it. A job filtered off the default branch makes "green on main"
  meaningless.
- **A green security check is not evidence that a scan ran.** When a scanner
  is broken fleet-wide, a green result may mean it audited nothing.
- **A missing CircleCI context is a stop.** A build can fail on the exact
  head without ever posting its status. Cross-check the build's
  `vcs_revision`; never merge past the gap.
- **Twice red on the same commit is real.** One rerun or retry per change.
  That is the `once-per-change` guard.
- **The branch name is part of the branch.** A failure that reproduces only
  on one branch is not always the dependency: a long branch name has broken
  chart labels before.
- **Teammate work comes first.** Before a migration, read the repository's
  open PRs and recent commits. An open teammate PR on the same subsystem is
  a stop, not a merge conflict to route around.
- **A merged rule is live on the next sweep.** Nothing gates the catalogue
  but review on this repository: there is no signature and no release. That
  is the point, and it is why `strict-chain`, the one action that merges, is
  **held** in the registry until a measured run shows PRs that need it. A
  rule may name it and validation accepts it; the sweep refuses it and says
  so, so the gate is code rather than the absence of a rule.
- **A context that has not reported is not a context nobody posts.** A
  queued workflow reports nothing, which is what drift looks like. The
  `checks-settled` guard waits for the head to finish reporting, and a
  drift rule names the context shapes it diagnoses so the protection write
  touches those and no others.
- **A generated file is never hand-edited.** `zz_*`, the generated
  `renovate.json5`, the align-managed `.pre-commit-config.yaml` and
  `.circleci/workflows.yml`. `Makefile.custom.mk` and `Chart.yaml` are the
  repository's own and may be written.
