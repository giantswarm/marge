---
name: marge-rescue
description: Hazards, guards and known-pattern hints for fixing a bot PR that no marge rule matched. Load it inside the weekly rescue run, before reading a failure or writing a change.
---

# Rescuing a bot PR

marge handed you this PR because no rule matched its failure. Everything a
rule already fixes is fixed before you start, so treat every PR you receive as
a pattern nobody has expressed yet.

The rules are in `rules/` of this repository. `docs/patterns.md` is the full
catalogue, including the patterns the engine deliberately leaves to a person.
This page carries only what a model needs when the rules ran out: the ways a
reasonable decision goes wrong, and where known fixes live.

## Before you change anything

**Read the repository's open PRs and recent commits first.** An open teammate
PR or a fresh commit on the same module is a hard stop. Mark the PR blocked
with the reference and move to the next repository. The dependency graph says
what broke. Only the team's own work says whether it should be fixed at all.

**Never touch a PR a person authored.** You work on bot PRs. A teammate's PR
that blocks the one you hold is a stop, not a conflict to route around.

**Confirm the PR is still open and still mergeable** immediately before you
work on it. A sweep's handoff is minutes old, and Renovate rebases.

**Work one repository at a time**, and inside a repository one PR at a time.

## Reading the failure

**A check name is not a diagnosis, and neither is a title.** Read the failing
step's log. Never classify from a check name, a PR title, a repository, or a
previous sweep's table.

**Absent is not green.** A base branch that never ran a check has not passed
it. Before you trust "green on the base", confirm the check actually ran
there. A job filtered off the default branch makes "green on main"
meaningless.

**A green base does not prove the bump is innocent.** A major bump can be red
for its own reasons while the base is fine.

**The branch name is part of the branch.** A failure that reproduces only on
one branch is not always the dependency. A long branch name has broken chart
labels before.

**Twice red on the same commit is real.** One rerun or one retry per change.
A job that passes on a rerun of the same commit is proof of a flake. Another
PR being green is not.

**A green security check is not evidence that a scan ran.** When a scanner is
broken across the fleet, a green result can mean it audited nothing. Read one
step's output before you believe the conclusion.

**A missing CircleCI context is a stop.** A build can fail on the exact head
and never post its status. Cross-check the build's `vcs_revision`. Never
merge past the gap.

**A context that has not reported is not a context nobody posts.** A queued
workflow reports nothing, which is what required-check drift looks like. Wait
for the head to finish reporting before you call it drift.

## Writing the change

**Never hand-edit a generated file.** `zz_*`, the devctl-generated
`renovate.json5`, the align-managed `.pre-commit-config.yaml` and
`.circleci/workflows.yml`. If a generated file is the cause, the fix is a PR
on the repository that generates it. `Makefile.custom.mk`, `Chart.yaml` and
`.golangci.yml` are the repository's own and you may write them.

**Make the smallest fix that turns the check green.** Do not rewrite CI, do
not enable stricter lint rules on the way past, and do not refactor around the
failure. A rescue that grew three times larger than the bump cost $20.76 and
still needed a second round.

**Never silence a finding per line.** No `nolint` comments, no `nosec`, no
globally disabled rule. Extract the constant, or tune the rule in the
repository's own configuration with the reason written down.

**Never suppress a vulnerability.** A red security scan is fixed, not
ignored. You do not write a `.nancy-ignore` entry: nancy-fixer owns the bump
and the time-boxed ignore. If an ignore is the only remedy left, mark the PR
`blocked` and name nancy-fixer.

**Ask for a review before you push**, with whatever review subagent the run
declares. If it declares none, read your diff against the failing log once
more before you push.

## Merging

**A required check that is pending or missing is a wait, never a bypass.**
`--admin` exists to bypass the review requirement on a bot PR. It also
bypasses status checks, which has put real regressions on default branches.
Confirm every required context reports `success` on the head commit before
you merge.

**A repository with no required checks is not the same as a repository whose
checks are green.** When the protection API answers 404, there are no
required checks, and the non-required ones still have to settle.

**A red non-required check on a dependency PR is not ignorable.** Decide it
with the base-head comparison above.

**Never self-approve your own fix PR.** An author cannot approve their own
PR. If branch protection blocks on `REVIEW_REQUIRED`, stop and report.

**If you lift `enforce_admins`, restore it in the same run and read it back.**
A repository left with admin enforcement off is worse than an unmerged PR.

**No poll loops.** Do not write `while true; do ...; sleep; done`. Wait with
`gh pr checks <number> --repo <owner>/<repo> --watch --required`, or schedule
a wakeup and query once when it fires. A hand-rolled poll loop cost $11.55 in
798 messages.

**If the checks do not go green in a reasonable wait, stop.** Leave the PR
open with a marker. That is the correct outcome. Shipping a regression and
calling it a flake is not.

## Finishing

Every PR you leave open ends in a marker. A plain comment is invisible to the
next sweep, so the sweep would hand you the same PR again next week.

- `failed`: you attempted this code and could not fix it.
- `blocked`: the fix is known and waits on something outside the repository:
  an upstream release, an ecosystem that is not ready, a decision.

Write the outcome, the reason and the estimated cost.

## Not on this page

The runbook also carries the hazards of driving a sweep from a shell: a
CircleCI token read from a local file, a guard script that needs bash, scratch
clones, agent instances to name and clean up, transcripts to capture, a budget
canary before a batch, and a trial report at the end. You have none of them.
You run inside one declared run, on the platform, with the App credential and
one repository at a time. The toolchain is chosen for you before you start.

## Known-pattern hints

These are patterns the runbook records with a remedy, that no rule expresses.
The hint is where the fix lives, not a script to run. Confirm it against the
log before you act on it.

| Failure | Where the fix is | Runbook row |
|---|---|---|
| A breaking Go API across a major bump | A code change spread over call sites; check the module's release notes for the rename | 3, 52, 61, 67, 72 |
| An ESM-only major with a CommonJS consumer | Usually a `patch-package` shim, chosen case by case | 11 |
| A lint rule that tightened | Mechanical but spread over files; sometimes a configuration decision instead | 10, 17 |
| Bumps that only pass together | Fold the PRs into one branch; no single PR can go green alone | 40, 65, 66 |
| A chart validator that newly rejects a chart | A CI values file, never a weakened guard | 22, 27 |
| An alignment migration that renamed a built binary | The Dockerfile needs a per-architecture selection | 43 |
| A CVE in a transitive dependency with no clean path | A judgement about the dependency tree; do not force the parent | 2 |
| A CI defect unrelated to the bump | Diagnose before any remedy; the bump is not the variable | 4 |
| OCM bundle-wiring drift after per-app releases | One combined catch-up PR, and usually a second round | 13, 103 |
| A chart test failing in the apptestctl bootstrap | Confirm `no matches for kind "PodMonitor"` in the log; merging past it is a decision | 18 |
| An OCI artifact a component references but nobody pushed | The reference and the version move together; the check found a repository defect | 45, 92 |
| A major bump whose module system changed | A small code change spread over call sites, for example a `js-yaml` v5 default export | 53 |
| A chart test that deploys and then times out in silence | Confirm the hang shape in the build log before any decision | 58 |
| `helm dependencies update` failing on a vendir-vendored subchart | The vendor target; mark the PR and leave it | 60 |
| A test environment whose DOM implementation changed | A shared test setup file, written case by case, for example `jsdom` v30 | 77 |
| An Align files PR blocked by the repository's own content | Fix the repository's content on its default branch, then refresh the align PR. The alignment payload is out of scope | 107 |
| A stale `go.sum` Renovate did not tidy | `go mod tidy`, commit, push | 1 |
| A pre-commit hook that modified files | Run the hook with its exact arguments and commit the result | 25, 41, 54, 64 |
| A generated Dockerfile that drifted | Run the repository's generate target on the branch | 35 |
| A vendir bump without the post-sync step | Run the repository's sync target with mikefarah `yq` | 71 |
| The architect v9 `multiarch` argument | Delete it from every `push-to-registries` invocation | 19 |
| A `TEAM-NAME` placeholder in `Chart.yaml` | Replace it with the team from `CODEOWNERS` | 42 |
| A staticcheck SA1019 deprecation after a bump | Rename the call site | 62 |
| `github.Ptr` rejected by the `inline` analyzer | Replace it with the `new` builtin | 73 |
| A Makefile target referencing a file the alignment migration deleted | Drop the file from the target; `Makefile.custom.mk` is the repository's own | 24 |
| A chart icon the `abs` validator rejects | `giantswarm-validator-ignored-checks: C0004` in `.abs/main.yaml` | 28 |
| ATS refusing an unknown cluster type in its pre-run | Add a minimal `.ats/main.yaml` | 50 |
| A placeholder PEM header tripping `gitleaks` | An allowlist regex in the repository's `.github/.gitleaks.toml` | 70 |
| architect v9 multi-arch breaking a Dockerfile that is not arm64-ready | Pin `platforms: linux/amd64` on the default branch, then refresh the PR | 29 |
| `check-values-schema` rejecting an empty `values.yaml` | Make the values file `{}` on the default branch, then refresh the PR | 34 |
| A required check the Align files PR has not delivered yet | Land the align PR, then refresh every PR it blocks; `pre-commit` and `semantic-pull-request` arrive that way | 36, 46 |

Some of these are mechanical enough to become rules once the engine has a
branch-writing action for them. Until then they are yours.

## When you find a pattern worth keeping

If you fixed a failure that will come back, say so in the run's summary with
the log signature. `marge rules draft <signature>` opens a draft rule from it.

**A PR that promotes a pattern to a rule deletes its hint row here, in the
same PR.** A hint that outlives its rule makes the model spend tokens on a
failure the engine now fixes by itself. `skill_test.go` fails the build when a
hint cites a runbook row that a rule already covers, so the two cannot drift
apart.
