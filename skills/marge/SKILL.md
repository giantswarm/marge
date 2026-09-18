---
name: marge
description: Drain a team's bot PR queue with the marge sweep engine, from Claude Code. Load it before listing, sweeping, remedying or marking bot PRs, and before fixing a bot PR failure by hand.
---

# Sweeping bot PRs with marge

marge classifies the open bot PRs of a scope, merges the ones its policy and
its guards allow, and hands the rest back with a reason. You drive it. You do
not re-implement it: every decision it makes is in the engine, behind guards
that have no override flag.

**Never bypass a guard. If the engine waits or refuses, say why and propose
the GitHub UI under the person's own name.**

## What marge touches

Four bot authors and nothing else: `renovate[bot]` (Renovate),
`giantswarm-align-files[bot]` (Align files), `heraldbot[bot]` (Herald, the
security remediation PRs of nancy-fixer) and `dependabot[bot]`. A PR a person
authored, the caller's own included, is reported as `Untrusted author` and is
never approved and never merged.

A green PR merges when the team's policy makes its kind and its update size
eligible. The company defaults merge Align files and Herald PRs, and the
patch, minor, digest, pin and lockfile updates of Renovate and Dependabot. A
major update, or one whose size marge cannot read, is held for a person.

## The classifications

Every tool reports the PRs of a scope in these groups, and a swept PR carries
the matching `marge/<class>` label:

| Group | What it means | Who acts |
|---|---|---|
| `merged` | marge approved and squash-merged it | nobody |
| `eligible` | the policy allows the merge; a dry run stops here | the next sweep |
| `security_failures` | a failing check whose name matches the security list | a person |
| `action_required` | a real failure on the code, or a PR the policy holds for a person (label `marge/held`) | a rescue, or a decision |
| `stale` | every failing check is green on the base head | the `refresh` action |
| `refreshed` | marge updated the branch from its base | the next sweep |
| `cancelled` | CircleCI cancelled the build itself | the `retry` action |
| `retried` | marge reran the workflow on the same head | the next sweep |
| `obsolete` | a higher-version sibling replaces it, or the diff executes nothing | close it |
| `waiting` | a required check is pending or nobody reported it | the next sweep |
| `ci_unavailable` | the Actions budget is spent | a person |
| `ci_no_verdict` | the checks established nothing about the code | the remedy in the detail |
| `skipped` | untrusted author, a head branch in another repository, or the sweep switched off for the repository | a person |
| `unclassified` | no sweep has labelled this PR yet | call `list` with `refresh: true` |
| `repositories_failed` | the PRs of this repository could not be read | report it |

Act on `action_required` only.

## The tools

muster serves marge's five tools as `x_marge_list`, `x_marge_sweep`,
`x_marge_remedy`, `x_marge_mark` and `x_marge_changelog`. Each one is an
adapter over the engine call the CLI makes, with the same guards, and each one
that writes takes `dry_run`.

Every tool takes the same scope arguments, and the scope decides the policy:
`team` (the repositories and the policy of that team), or `query`, `org`,
`repos` and `repos_file` for the query scope, which has no team and runs under
the company defaults. `prs` narrows a call to single PRs of that scope.

| Tool | Writes | Arguments beyond the scope |
|---|---|---|
| `x_marge_list` | no | `refresh`, `teams` |
| `x_marge_sweep` | yes | `dry_run`, `actions`, `merge_auto`, `security_patterns` |
| `x_marge_remedy` | yes | `pr_url` (required), `rule`, `team`, `dry_run` |
| `x_marge_mark` | yes | `pr_url` (required), `outcome` (`failed` or `blocked`), `reason`, `tool`, `dry_run` |
| `x_marge_changelog` | yes | `prs` (required), `team`, `dry_run` |

`x_marge_list` with `refresh: false` reads back the label of the last sweep
and costs the discovery of the scope and nothing more: one search for a query
scope, or one listing per twenty-five repositories for a team scope.
`refresh: true` classifies every PR again and costs a PR read and a check read
per PR, and it returns the evidence, the update type, the policy and the prior
rescue of each. Prefer the cheap read. Refresh when the answer must be
current.

`teams` reads several teams in one call, each under its own policy. The answer
is then `{"teams": [{"team": ..., "result": ...}]}`, and a team whose files
are missing carries an `error` instead of a `result`. Use it instead of one
call per team: the teams share one discovery, so their repository lists are
read once.

`x_marge_sweep` runs the steps `classify`, `changelog`, `approve`, `merge`,
`refresh`, `retry`, `remedy` and `mark`, in that order. `actions` selects a
subset. `remedy` needs `mark` and is refused without it.

## The changelog entry

A bot PR carries no changelog entry of its own, so a repository's release
notes lose every dependency update. marge writes one in the team's format:
the `changelog` section of `bot-prs-sweep/team-<name>.yaml` names the file,
the heading and section the entry goes under, and the line itself.

Two doors, and the same write behind both:

- The `changelog` **step** of a sweep, where `changelog.enabled` is true in
  the team's policy. It is false by default.
- `x_marge_changelog`, on the PRs a person picked, whatever the policy says.

The entry is a commit on the PR's own branch, and that has two consequences a
person has to hear before they ask for one:

- **CI runs again.** The step therefore writes before anything is approved,
  never after -- a commit pushed after an approval dismisses it wherever the
  branch protection dismisses stale reviews -- and the PR is then left under
  `waiting`. The next sweep classifies the new head and merges it. Say this
  when someone asks why their PR did not merge in the run that wrote its
  entry.
- **It is written once.** The check is on the line itself, so a rebase, a
  second sweep and a person writing by hand all converge on one entry. Do not
  add a guard of your own around it.

A repository with no changelog file is refused with that reason and is swept
exactly as it would have been. A changelog is a courtesy, never a guard.

## How to read a marker

A failing PR carries the most recent ai-rescue marker of its comments in its
`rescue` object: `tool`, `outcome`, `reason`, `at`, `stale` and `rebased`.

- `stale: false` means the attempt still describes this change. Automation
  already lost on it. Escalate it to a person and do not try again.
- `rebased: true` means the head moved but the diff did not, which is what a
  Renovate rebase does. The attempt still stands, so `stale` stays false.
- `stale: true` means the PR carries a new version or a pushed fix. It is
  open for another rescue.

## Sequence A: the tool path

1. `x_marge_list` with the person's `team`. Report what is waiting.
2. `x_marge_sweep` with the same scope and `dry_run: true`.
3. Present the outcome of the dry run, group by group, with the numbers.
4. Ask the person to confirm.
5. `x_marge_sweep` with the same scope and no `dry_run`.
6. Report what merged and what remains, and name the group of each PR left.

Steps 3 and 4 are not optional. A sweep approves and merges under the
person's own grant, and the dry run is what they confirm.

## Sequence B: the CLI fallback

When the muster MCP server is absent, the same six steps are the marge CLI:

```bash
marge sweep --team <name> --dry-run --no-tui
marge sweep --team <name> --no-tui
marge mark <pr-url> --outcome blocked --reason "<why>" --tool claude
```

`marge sweep --output json` prints the structure the tools return. The CLI
acts with the person's own credential, so the confirmation step stays.

## Never

- **Never poll.** No `while true` loop, no `sleep` loop. Wait with
  `gh pr checks <number> --repo <owner>/<repo> --watch --required`, or
  schedule one wake-up and ask once. A hand-rolled poll loop cost $11.55 in
  798 messages.
- **Never run a `gh` command the engine covers.** Listing, classifying,
  approving, merging, updating a branch, retrying a build and marking are
  tool calls. A shell loop over `gh pr list` is a second implementation with
  none of the guards.
- **Never merge past a guard.** No `--admin`, no change to `enforce_admins`,
  no approval of your own PR, no merge past a pending or unreported required
  check, and no merge past a failing security check.
- **Never widen the scope.** The person asked for one team's queue. Another
  team's PRs, an unrelated repository and a refactor on the way past are not
  in it. A rescue that grew three times larger than the bump cost $20.76 and
  still needed a second round.
- **Never start an agent without confirmation.** Say what it will do and what
  it may cost, then wait.
- **Never touch a PR a person authored.**

When no rule matched and you must fix the failure yourself, read
[`references/hazards.md`](references/hazards.md) first.
