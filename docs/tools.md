# The tools

Generated from the registered tools (`make tools-doc`); through muster every tool is `x_marge_<name>`. Both transports, stdio and streamable-http, register the same tools.

| Tool | Kind |
|---|---|
| `changelog` | write |
| `list` | read-only |
| `mark` | write |
| `remedy` | write, destructive |
| `sweep` | write, destructive |

## `changelog`

```text
Add one changelog entry, in the team's own format, to bot PRs and commit it to each PR's own branch.

The format is the changelog section of the team's bot-prs-sweep/team-<name>.yaml (file, heading, section and a line template over dependency, versions, PR, repository, bot and update type), else the company default. Nothing else is written: no approval, merge, label or comment. A PR already carrying the line is left as is. A PR on a fork branch, by an untrusted author, or in a repository without the file or with a cliff.toml (notes from commits) is refused with the reason.
```

```json
{
  "properties": {
    "dry_run": {
      "description": "Return the line each PR would receive and write nothing (default: false)",
      "type": "boolean"
    },
    "prs": {
      "description": "The pull requests to write an entry on, each a PR URL or OWNER/REPO#NUMBER.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "team": {
      "description": "Use this team's format, from bot-prs-sweep/team-<name>.yaml in the team-file repository; without it, the company default format.",
      "type": "string"
    }
  },
  "required": [
    "prs"
  ],
  "type": "object"
}
```

## `list`

```text
List the open bot PRs of a team or query scope with each PR's class, label, bot kind and age.

By default it reads back the class the last sweep stored in each PR's marge/<class> label: what the last sweep decided, not what a sweep would decide now. refresh: true runs the sweep's classify step on a dry run instead. unclassified means no sweep has labelled that PR, not that it needs nothing: call again with refresh: true. Entries are grouped as the sweep reports them; rescue tooling acts on action_required only and skips an entry whose rescue is not stale.
```

```json
{
  "properties": {
    "org": {
      "description": "GitHub organization or user to limit the scope to",
      "type": "string"
    },
    "prs": {
      "description": "Only these PRs of the scope, each a URL or OWNER/REPO#NUMBER. The scope still picks repositories and policy, so a PR outside it is refused.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "query": {
      "description": "Narrow the scope like `marge [query]`: GitHub search text (\"typescript\", \"repo:org/repo\"), or with repos/repos_file a filter on their org/repo.",
      "type": "string"
    },
    "refresh": {
      "description": "Classify every PR now instead of reading its stored marge/<class> label (default false); costs a PR and a check read per PR, adds evidence and policy.",
      "type": "boolean"
    },
    "repos": {
      "description": "org/repo entries to scan instead of searching GitHub; merged with repos_file, duplicates dropped.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "repos_file": {
      "description": "Path to a file of org/repo lines (blank lines and # comments ignored) to scan instead of searching GitHub; merged with repos, duplicates dropped.",
      "type": "string"
    },
    "team": {
      "description": "Cover this team's repositories (repositories/team-<name>.yaml in the team-file repository) under the team's policy.",
      "type": "string"
    },
    "teams": {
      "description": "Several teams in one call, each under its own team file and policy, with one shared discovery; answers {\"teams\": [{team, result or error}]} in order.",
      "items": {
        "type": "string"
      },
      "type": "array"
    }
  },
  "required": [],
  "type": "object"
}
```

## `mark`

```text
Record a failed AI rescue attempt on a PR as an ai-rescue marker comment, which later sweeps report.

The marker records the head SHA and a fingerprint of the diff: it goes stale when the PR content changes (new version, pushed fix), but survives a rebase that leaves the diff unchanged.
```

```json
{
  "properties": {
    "dry_run": {
      "description": "Return the marker that would be written, head SHA and fingerprint included, and post nothing (default: false)",
      "type": "boolean"
    },
    "outcome": {
      "description": "Rescue outcome (default: \"failed\")",
      "enum": [
        "failed",
        "blocked"
      ],
      "type": "string"
    },
    "pr_url": {
      "description": "The pull request, as an URL (https://github.com/OWNER/REPO/pull/NUMBER) or as OWNER/REPO#NUMBER",
      "type": "string"
    },
    "reason": {
      "description": "Short explanation of why the rescue did not succeed",
      "type": "string"
    },
    "tool": {
      "description": "Name of the tool/agent that attempted the rescue (default: \"ai\")",
      "type": "string"
    }
  },
  "required": [
    "pr_url"
  ],
  "type": "object"
}
```

## `remedy`

```text
Remedy one bot PR: classify it and apply the catalogue rule that matches, through that rule's action and its guards.

The sweep's classify, remedy and mark steps on one PR, with its guards, refusals and evidence comment. Naming a rule only removes the others from the contest: it still has to match. When nothing matches, nothing is written and the failure is reported under unhandled.
```

```json
{
  "properties": {
    "dry_run": {
      "description": "Report the rule that would apply and write nothing (default: false)",
      "type": "boolean"
    },
    "pr_url": {
      "description": "The pull request, as an URL (https://github.com/OWNER/REPO/pull/NUMBER) or as OWNER/REPO#NUMBER",
      "type": "string"
    },
    "rule": {
      "description": "Apply this rule of the catalogue instead of letting every rule compete. The rule must still match the PR.",
      "type": "string"
    },
    "team": {
      "description": "Decide the PR under this team's policy, from bot-prs-sweep/team-<name>.yaml in the team-file repository; without it, the company default policy.",
      "type": "string"
    }
  },
  "required": [
    "pr_url"
  ],
  "type": "object"
}
```

## `sweep`

```text
Sweep a team's or query's bot PRs (Renovate, Align files, Herald, Dependabot): approve and squash-merge green ones.

Labels each PR marge/<class>. Majors and unreadable updates wait for a person; a pending or unreported required check is a wait, never a bypass; a failing security check is never merged past. Not action_required: stale (fixed on base; refresh updates it), cancelled (CircleCI auto-cancelled; retry reruns it), ci_no_verdict (detail names the remedy), obsolete (superseded or no_op: close it). Rescue tooling acts on action_required only, skipping entries whose rescue is not stale.
```

```json
{
  "properties": {
    "actions": {
      "description": "Comma-separated steps, in fixed order: classify, changelog, approve, merge, refresh, retry, mark (default all). changelog commits before approve.",
      "type": "string"
    },
    "dry_run": {
      "description": "Show what would be done without making changes (default: false). Stale PRs are still classified, but not refreshed.",
      "type": "boolean"
    },
    "merge_auto": {
      "description": "Also merge PRs that have auto-merge enabled (default: false)",
      "type": "boolean"
    },
    "org": {
      "description": "GitHub organization or user to limit the scope to",
      "type": "string"
    },
    "prs": {
      "description": "Only these PRs of the scope, each a URL or OWNER/REPO#NUMBER. The scope still picks repositories and policy, so a PR outside it is refused.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "query": {
      "description": "Narrow the scope like `marge [query]`: GitHub search text (\"typescript\", \"repo:org/repo\"), or with repos/repos_file a filter on their org/repo.",
      "type": "string"
    },
    "repos": {
      "description": "org/repo entries to scan instead of searching GitHub; merged with repos_file, duplicates dropped.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "repos_file": {
      "description": "Path to a file of org/repo lines (blank lines and # comments ignored) to scan instead of searching GitHub; merged with repos, duplicates dropped.",
      "type": "string"
    },
    "security_patterns": {
      "description": "Comma-separated case-insensitive substrings added to the built-in list that flags failing CI checks as security-related",
      "type": "string"
    },
    "team": {
      "description": "Cover this team's repositories (repositories/team-<name>.yaml in the team-file repository) under the team's policy.",
      "type": "string"
    },
    "teams": {
      "description": "Several teams in one call, swept one after another under their own team file and policy; answers {\"teams\": [{team, result or error}]} in order.",
      "items": {
        "type": "string"
      },
      "type": "array"
    }
  },
  "required": [],
  "type": "object"
}
```
