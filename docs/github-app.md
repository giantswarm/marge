# The sweep GitHub App

marge acts on GitHub through one GitHub App, owned by the `giantswarm`
organization and installed on all repositories. The App is registered and its
permissions are settled. The App serves both paths:

- **Interactive.** The App's user-to-server flow backs muster's GitHub
  connector. The token is the person's. The person's effective rights are the
  intersection of the App's permissions and their own. Nobody can do more
  through a sweep than a sweep is meant to do, not even an organization owner.
- **Unattended.** Installation tokens, minted on demand from the private key,
  back the scheduled sweep and the weekly rescue run. GitHub sees the App.

**Neither path runs yet.** This page records the App, not marge's behaviour
today. `internal/github` authenticates with one token string read from
`GITHUB_TOKEN`, `GH_TOKEN` or `gh auth login`. marge holds no App ID, no
private key and no client credentials, and it mints no installation token.
Read every claim below about what marge does as the target state. The
interactive path needs the callback URLs (roadmap#4357); the unattended path
needs App authentication in `internal/github` and the credentials in cluster
Secrets (roadmap#4356).

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
| Contents | `contents` | write | Two reasons. **(1)** Push branch-writing remedies, `update-branch` merge commits and the strict merge chain. **(2)** Make the App's approving review count. See the hazard below |
| Checks | `checks` | read | Read check runs to classify a failure |
| Commit statuses | `statuses` | read | Read commit statuses, which is how CircleCI reports |
| Issues | `issues` | write | Labels and comments on a pull request go through the Issues API. `Pull requests: write` also covers this today. The permission is held because D4 grants it. Before you remove it, run one sweep with `issues` dropped from the installation and confirm that labels and comments still land |
| Actions | `actions` | write | Re-run a wedged workflow run, and dispatch the Align files workflow |
| Administration | `administration` | write | Lift `enforce_admins` for an admin merge and restore it in the same run, and repair a renamed required check after an alignment migration |

marge dispatches a workflow on `giantswarm/github` only. `Actions: write` is
organization-wide once the App is installed everywhere, so GitHub does not
enforce that limit. marge enforces it: the `dispatch-align-workflow` action
refuses every other repository.

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

marge asserts this before it submits a review. `ensureWriteAccess` reads
`GET /repos/{owner}/{repo}` once per repository per sweep and refuses the
approval when `permissions.push` is `false`. A loud refusal beats a silent
no-op. The check runs on the dry-run path too, so `--dry-run` names the
refusal after a permission change instead of reporting a plain skip.

**Not yet measured: `permissions.push` under an installation token.** The
review-counting result below is measured. The field marge reads to predict it
is not. Nobody has confirmed that `GET /repos` under an installation token
with `Pull requests: write` and no `Contents: write` answers
`permissions.push: false`. Until somebody does, two outcomes stay open: the
field always reports `true`, which makes the guard a no-op, or the field is
absent, which marge reports as `write access unknown` and treats as a refusal.
Measure both permission states on the roadmap#4349 repository and record the
result here.

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
| OAuth client ID and client secret | the interactive path, through muster's GitHub connector | The App's **own** client credentials. Do not reuse the shared `github-oauth-client` secret. A broader shared client would widen the permission ceiling of every sweep |
| Webhook secret | nothing | Webhooks are off. Keep the value; do not publish it |

The cluster-side Secrets arrive with the chart. Until then the credentials
exist in 1Password only.

### Rotation

Rotate the private key:

1. Generate a second private key on the App's settings page. The App now
   accepts both.
2. Put the new key in the 1Password item and update the cluster Secret.
3. Confirm that a token mints with the new key. Once marge mints its own,
   confirm it on a sweep.
4. Delete the old key on the settings page.

Rotate the client secret:

1. Generate a second client secret on the App's settings page.
2. Put it in the 1Password item and update muster's
   `clientCredentialsSecretRef`.
3. Sign in once to confirm the new secret works. Every existing grant stays
   valid; the secret authenticates marge to GitHub, not the person.
4. Delete the old client secret.

A leaked private key is an incident. Delete the key on the settings page
first, then rotate. The App's installations survive; only the key dies.

## Installation tokens

An installation token is minted on demand, for one repository, from the
private key. Both properties below are measured against GitHub, by hand. The
mint is not wired into marge yet.

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

### Open: the OAuth callback URLs

The App carries no callback URL yet, so the interactive path cannot run. Each
muster that hosts marge's MCP server adds one URL of the shape
`https://<muster public URL>/oauth/proxy/callback` to the App's settings. A
callback URL is added at any time and needs no new registration. This is done
with the MCP mode (roadmap#4357).

A permission change is an edit to the manifest **and** to this record, in the
same pull request. Apply it on the App's settings page afterwards. Every
installation must then accept the new permission before the App can use it.
