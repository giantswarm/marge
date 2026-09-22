# The sweep GitHub App

marge acts on GitHub through one GitHub App, owned by the `giantswarm`
organization and installed on all repositories. The App is registered and its
permissions are settled. The App serves the unattended path; the interactive path is the person's own token:

- **Interactive.** Not the App. A person reaches marge through muster, and
  muster attaches the GitHub grant that person already holds for the muster's
  other GitHub-backed servers, minted by the shared OAuth client. The token
  is the person's, with the person's own rights. The MCPServer that
  registers marge is declared on the platform side, not by the chart.
- **Unattended.** Installation tokens, minted on demand from the private key,
  back the scheduled sweep and the weekly rescue run. GitHub sees the App.

**The unattended path runs.** `internal/github` authenticates as the App when
the environment carries the App credential, and with a token otherwise. The
scheduled sweep in the chart sets the credential, so it acts as the App; a
person running the CLI sets none and keeps the token path. The interactive
path runs through muster (roadmap#4357).

The target is that no personal access token is used anywhere.

| Field | Value |
|---|---|
| App name | `GiantSwarm Marge` |
| Slug | `giantswarm-marge` |
| App ID | `4950078` |
| Organization installation ID | `161842404` |
| Bot login | `giantswarm-marge[bot]` |
| Owner | `giantswarm` organization |
| Visibility | private to the organization |
| Webhooks | off. marge polls; it receives no events |

## How marge authenticates as the App

`LoadApp` reads three settings from the environment. It returns nothing when
all three are absent, and an error when only some of them are set: a partial
credential is a misconfigured unattended run, never a person's shell.

| Variable | Holds |
|---|---|
| `MARGE_GITHUB_APP_ID` | the numeric App ID |
| `MARGE_GITHUB_APP_INSTALLATION_ID` | the numeric installation ID |
| `MARGE_GITHUB_APP_PRIVATE_KEY_FILE` | the path of the PEM private key |
| `MARGE_GITHUB_APP_PRIVATE_KEY` | the PEM itself, for a local test |

Prefer the file. A PEM in an environment variable shows up in every process
listing of the pod, and the chart mounts the key from a Secret at
`/etc/marge/github-app/private-key.pem`.

Every request then carries a token the transport mints for it:

- A call on `/repos/{owner}/{name}/...` carries a token scoped to that one
  repository. GitHub answers `404` for every other repository under it.
- The mint itself and `GET /app` carry the App JWT, signed RS256 with the
  private key and valid for nine minutes.
- A call that names no repository carries an installation-wide token: the
  code search, which cannot be scoped to one repository, and the GraphQL
  discovery, which reads many repositories in one request.

A token is kept in memory until a minute before it expires, then replaced.
Nothing is written to disk and nothing survives the process.

`GET /user` has no meaning under an installation token, so
`AuthenticatedLogin` reads the App's slug from `GET /app` and returns
`giantswarm-marge[bot]`.

## Permission record

Read this table before you trim a permission. Each row states what the
permission is for. A permission with no stated reason is a permission somebody
deletes.

The Key column is the manifest key in `docs/github-app-manifest.json`.
`TestManifestMatchesTheRecord` compares the two, so the table and the manifest
can not drift apart. Metadata carries no key: GitHub grants it implicitly and
the manifest must not name it.

| Permission | Key | Level | Why marge holds it |
|---|---|---|---|
| Metadata | | read | Mandatory. GitHub grants it to every App |
| Pull requests | `pull_requests` | write | Read the queue, submit approving reviews, merge, label, comment, and write markers and evidence |
| Contents | `contents` | write | Two reasons. **(1)** Push the changelog entry, `update-branch` merge commits and the strict merge chain. **(2)** Make the App's approving review count. See the hazard below |
| Checks | `checks` | read | Read check runs to classify a failure |
| Commit statuses | `statuses` | read | Read commit statuses, which is how CircleCI reports |
| Issues | `issues` | write | Labels and comments on a pull request go through the Issues API. `Pull requests: write` also covers this today. The permission is held because D4 grants it. Before you remove it, run one sweep with `issues` dropped from the installation and confirm that labels and comments still land |
| Actions | `actions` | write | Re-run a wedged workflow run, and dispatch the Fix Go vulnerabilities workflow |
| Administration | `administration` | write | Repair a renamed required check after an alignment migration |

marge dispatches one workflow and no other. `dispatch-cve-workflow` names the
swept repository's own generated `zz_generated.fix_vulnerabilities.yaml`, on
the pull request's base branch. `Actions: write` is organization-wide once the
App is installed everywhere, so GitHub enforces no such limit; the action
does.

The App does **not** hold `Workflows: write`. The align-files App holds it
because it writes workflow files. marge never writes one.

### Hazard: `Contents: write` is what makes an approval count

**Do not remove `Contents: write` when a least-privilege review reports it as
unused by approvals.** The daily merge path stops with no error, and pull
requests sit at `REVIEW_REQUIRED`.

This was measured on a throwaway repository (roadmap#4349). With
`Pull requests: write` alone, the App's review was created and visible
(`APPROVED`, actor type `Bot`), but GitHub excluded it from
`latestOpinionatedReviews`, and `reviewDecision` stayed `REVIEW_REQUIRED`.
After the installation was granted `Contents: write`, the **same review**, with
no new action, appeared in `latestOpinionatedReviews`. `reviewDecision` became
`APPROVED` and `mergeStateStatus` became `CLEAN`. GitHub evaluates the
reviewer's repository write access when it reads the reviews, and
`Contents: write` is what confers that access.

marge asserts this before it submits a review. `ensureWriteAccess` settles the
answer once per repository per sweep and refuses the approval when the actor
may not write. A loud refusal beats a silent no-op. The check runs on the
dry-run path too, so `--dry-run` names the refusal after a permission change
instead of reporting a plain skip.

The guard asks a different question of each actor, because only one of them
can answer:

- Under the App, it reads the `permissions` object GitHub returns when it
  mints the installation token the call carries, and requires
  `contents: write`. That object describes the token itself.
- Under a person's token, it reads `permissions.push` from
  `GET /repos/{owner}/{repo}`. An absent field is `write access unknown`, and
  also refuses.

### `permissions.push` under an installation token

`permissions.push` is present under an installation token, and it is always
`false`. It can never be the signal.

The field describes the **authenticated user's** access to the repository. An
installation token has no user behind it, so GitHub resolves no permissions
and returns the zero value for every field of the block.

`hack/measure-push-permission.sh` is the probe. It mints an installation token
for one repository from the App private key and prints both the permissions
GitHub reports on the mint and what `GET /repos/{owner}/{repo}` answers for
`permissions.push`. Both states were measured on 2026-09-17, App `4950078`,
installation `161842404`, repository `giantswarm/marge`.

With the whole installation:

```text
the token's permissions, as GitHub reports them on the mint:
{
  "checks": "read", "issues": "write", "actions": "write",
  "contents": "write", "metadata": "read", "statuses": "read",
  "pull_requests": "write", "administration": "write"
}

GET /repos/giantswarm/marge -> .permissions:
{
  "permissions": {
    "admin": false, "maintain": false, "push": false,
    "triage": false, "pull": false
  },
  "push_present": true
}
```

With the token narrowed to `-p '{"pull_requests":"write","metadata":"read"}'`:

```text
the token's permissions, as GitHub reports them on the mint:
{
  "metadata": "read",
  "pull_requests": "write"
}

GET /repos/giantswarm/marge -> .permissions:
{
  "permissions": {
    "admin": false, "maintain": false, "push": false,
    "triage": false, "pull": false
  },
  "push_present": true
}
```

| Token permissions | Reported `permissions.push` | What `ensureWriteAccess` does |
|---|---|---|
| `Pull requests: write` + `Contents: write` | present, `false` | allows: the mint reports `contents: write` |
| `Pull requests: write` alone | present, `false` | refuses: the mint reports no `contents: write` |

`permissions.push` reads the same in both rows, which is the whole point: the
field cannot tell the two apart, and the mint permissions can.

The column names the permissions of the **token**, not of the installation.
The mint narrows a token to any subset of what the installation holds, which
is how the second row was measured. The installation keeps every permission it
has, so the daily merge path is never at risk. Do not measure that row by
trimming the production App: its permissions are organization-wide, and
removing `Contents: write` would stop the merge path for every team.

Two more results from the same test, both permanent:

- The App's approval does **not** satisfy "require review from Code Owners".
  The approval is counted; the code-owner requirement stays unmet. marge reads
  this from the merge state and reports `awaiting-approval`.
- The App **cannot** approve a pull request it authored. GitHub answers
  `422 Review Can not approve your own pull request`.

## Credentials

Three secrets come out of the registration. All three live in 1Password, in
the `Team Bumblebee` vault, in one item named `marge sweep GitHub App`.

| Secret | Used by | Note |
|---|---|---|
| Private key (PEM) | the unattended path, to mint installation tokens | An App can hold several keys at once, which is what makes a rotation with no downtime possible |
| OAuth client ID and client secret | nothing | muster signs people in with the shared `github-oauth-client`, because it keeps one client per issuer. The App's client stays registered and unused |
| Webhook secret | nothing | Webhooks are off. Keep the value; do not publish it |

The chart puts them in the cluster. `marge.github.app.existingSecret` names a
Secret that already holds `github-app-id`, `github-app-installation-id` and
`github-app-private-key`, which is what a real installation uses;
`marge.github.app.privateKey` and its siblings write them inline, for a test.
The chart registers nothing with muster. The platform declares the
MCPServer next to the muster's other GitHub-backed servers, with their shared
client.

### Rotation

Rotate the private key:

1. Generate a second private key on the App's settings page. The App now
   accepts both.
2. Put the new key in the 1Password item and update the cluster Secret.
3. Confirm that a token mints with the new key. Once marge mints its own,
   confirm it on a sweep.
4. Delete the old key on the settings page.

A leaked private key is an incident. Delete the key on the settings page
first, then rotate. The App's installations survive; only the key dies.

## Installation tokens

An installation token is minted on demand, for one repository, from the
private key. Both properties below are measured against GitHub, by hand.

- **Scoped.** A token minted with `repositories: ["marge"]` lists exactly one
  repository at `GET /installation/repositories`, and answers `404` on a
  private repository outside that scope.
- **Short-lived.** GitHub sets `expires_at` one hour after the mint. The
  design is one token per repository per run, stored nowhere.

Measure the scope against a **private** repository. A public repository
answers `200` to any valid token, in scope or not, so a public negative probe
proves nothing.

## Registration

The App is registered through GitHub's App Manifest flow, not by hand in the
settings form, so that the permission set is exact and reproducible.
`docs/github-app-manifest.json` is the manifest. To register the App again, or
to register an equivalent App for a test, post that manifest to
`https://github.com/organizations/<org>/settings/apps/new` from a browser
session of an organization owner, and exchange the returned code at
`POST /app-manifests/{code}/conversions`.

The manifest's `redirect_url` is where GitHub sends the one-time creation
code. It is not an OAuth callback URL, and it is deliberately a localhost
address.

### The OAuth callback URLs

The App holds one callback URL per muster that once hosted marge's sign-in,
of the shape `https://<muster public URL>/oauth/proxy/callback`. Since muster
signs people in with the shared client, these entries are unused. They stay
registered and harmless: a callback URL grants nothing on its own.

| muster | Callback URL |
|--------|--------------|
| gazelle | `https://muster.gazelle.awsprod.gigantic.io/oauth/proxy/callback` |

A permission change is an edit to the manifest **and** to this record, in the
same pull request. Apply it on the App's settings page afterwards. Every
installation must then accept the new permission before the App can use it.
