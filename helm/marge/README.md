# marge

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

A housekeeping tool for dependency update PRs, served as an MCP server

Runs `marge serve --transport streamable-http`: the `sweep` and `mark` MCP tools on `/mcp`, with `/healthz` and `/readyz` for the probes. Register the Service as a streamable-http MCP server in muster to make the tools available to people and agents.

marge needs a GitHub token with the permissions listed in the [repository README](https://github.com/giantswarm/marge#setup). Pass it inline (`marge.github.token`, the chart writes a Secret) or point at an existing Secret (`marge.github.existingSecret`, key `marge.github.existingSecretKey`). A CircleCI token (`marge.circleci.*`) is optional; it lets marge inspect private CircleCI projects and retry auto-cancelled builds.

## The scheduled sweep

`schedule.daily` runs `marge sweep --all-teams` as the sweep GitHub App. The run reads every team's policy from `giantswarm/github` at start and sweeps each team that has a policy file whose `schedule` key is `enabled`. It classifies, approves, merges, refreshes stale branches, retries cancelled builds and applies the catalogue's rules. Every one of those steps acts through the GitHub or CircleCI API: the daily run writes no code to any branch.

The schedule acts as the App and never as a person. Set `marge.github.app` and the CronJob mints an installation token per repository, for one hour, and stores none. Without `marge.github.app` the chart refuses to render an enabled schedule. The App's credentials live in 1Password, in the `Team Bumblebee` vault, in the item `marge sweep GitHub App`; see [docs/github-app.md](https://github.com/giantswarm/marge/blob/main/docs/github-app.md).

Set `marge.slack.token` and each run posts one summary to the channel the team's policy names. A run that changed nothing posts nothing.

`schedule.weekly` is the interim trigger of the weekly rescue run. It stays off: the command it runs does not exist yet, so an enabled weekly schedule without `schedule.weekly.args` fails to render.

**Homepage:** <https://github.com/giantswarm/marge>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Giant Swarm | <team-bumblebee@giantswarm.io> |  |

## Source Code

* <https://github.com/giantswarm/marge>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| mcp.enabled | bool | `true` | Run the MCP server: the Deployment, its Service and its NetworkPolicy. Turn it off on an installation that runs the scheduled sweep alone, until a muster registers the server. |
| muster.register | bool | `false` | Register the MCP server with muster, so a person reaches it through the gateway as themselves. Needs the App's OAuth client credentials and a muster in the cluster. |
| muster.name | string | `""` | Name of the MCPServer resource. Defaults to the release fullname. |
| muster.namespace | string | `""` | Namespace the MCPServer is created in. Defaults to the release namespace; set it when muster watches another one. |
| muster.toolPrefix | string | `"marge"` | Prefix muster gives the tools of this server, so they are reachable as x_<prefix>_<tool>. |
| muster.description | string | `"Sweep a team's dependency and alignment PRs: list, sweep, remedy and mark, as the signed-in person."` | Description muster shows for the server. |
| muster.toolGroup | string | `""` | Value of the agent-platform.giantswarm.io/tool-group label, which is how the portal and the toolset presets group a server. Empty leaves the label off. |
| muster.github.issuer | string | `"https://github.com"` | Issuer the grants are filed under. GitHub publishes no discovery document, so the endpoints below are pinned instead. |
| muster.github.authorizationEndpoint | string | `"https://github.com/login/oauth/authorize"` | GitHub's authorization endpoint. |
| muster.github.tokenEndpoint | string | `"https://github.com/login/oauth/access_token"` | GitHub's token endpoint. |
| muster.github.scopes | string | `""` | OAuth scopes requested at sign-in. A GitHub App's user-to-server token takes its rights from the App's permissions and the person's own, so no scope is requested. |
| replicaCount | int | `1` | Number of marge replicas. The MCP transport is stateful per session, so keep it at 1 unless a client-affine load balancer sits in front. |
| image.registry | string | `"gsoci.azurecr.io"` | Registry of the marge image |
| image.repository | string | `"giantswarm/marge"` | Repository of the marge image |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for the pod |
| nameOverride | string | `""` | Overrides the chart name in resource names |
| fullnameOverride | string | `""` | Overrides the fully qualified resource name |
| serviceAccount.create | bool | `true` | Create a ServiceAccount for the pod. marge talks to GitHub and CircleCI only, so it needs no Kubernetes API access. |
| serviceAccount.automount | bool | `false` | Mount the ServiceAccount token into the pod |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount |
| serviceAccount.name | string | `""` | Name of the ServiceAccount to use. Generated from the fullname when empty and create is true. |
| podAnnotations | object | `{}` | Annotations to add to the pod |
| podLabels | object | `{}` | Labels to add to the pod |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context (restricted Pod Security Standard) |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context (restricted Pod Security Standard) |
| service.type | string | `"ClusterIP"` | Service type |
| service.port | int | `8080` | Service port; the container listens on 8080 |
| resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources |
| nodeSelector | object | `{}` | Node selector for the pod |
| tolerations | list | `[]` | Tolerations for the pod |
| affinity | object | `{}` | Affinity for the pod |
| marge.github.token | string | `""` | GitHub token marge uses (needs the permissions listed in the repository README). The chart writes it into a Secret; prefer existingSecret in production. The scheduled sweeps ignore it and act as the App instead. |
| marge.github.existingSecret | string | `""` | Name of an existing Secret holding the GitHub token. Takes precedence over token. |
| marge.github.existingSecretKey | string | `"token"` | Key of the GitHub token inside existingSecret |
| marge.github.app.id | string | `""` | Numeric ID of the sweep GitHub App. Required by the scheduled sweeps, which mint an installation token per repository instead of holding a token. |
| marge.github.app.installationId | string | `""` | Numeric ID of the App's installation on the organization. |
| marge.github.app.privateKey | string | `""` | PEM private key of the App. The chart writes it into a Secret; prefer existingSecret in production. The values of this item live in 1Password, in the Team Bumblebee vault, item "marge sweep GitHub App". |
| marge.github.app.existingSecret | string | `""` | Name of an existing Secret holding the App credential. Takes precedence over the inline values, and must carry every key named below. |
| marge.github.app.idKey | string | `"github-app-id"` | Key of the App ID inside the Secret |
| marge.github.app.installationIdKey | string | `"github-app-installation-id"` | Key of the installation ID inside the Secret |
| marge.github.app.privateKeyKey | string | `"github-app-private-key"` | Key of the PEM private key inside the Secret |
| marge.github.app.oauth.clientId | string | `""` | OAuth client ID of the App. marge never reads it; it is held here so one place holds the App's credentials, and muster's GitHub connector references the same Secret. |
| marge.github.app.oauth.clientSecret | string | `""` | OAuth client secret of the App. Do not reuse the shared github-oauth-client secret. |
| marge.github.app.oauth.existingSecret | string | `""` | Name of an existing Secret holding the OAuth client credentials. Takes precedence over the inline values. |
| marge.github.app.oauth.clientIdKey | string | `"github-app-client-id"` | Key of the client ID inside the Secret |
| marge.github.app.oauth.clientSecretKey | string | `"github-app-client-secret"` | Key of the client secret inside the Secret |
| marge.slack.token | string | `""` | Bot token of the sweep's own Slack app, which holds chat:write and nothing else. Without it a scheduled sweep does its work and posts no summary. |
| marge.slack.existingSecret | string | `""` | Name of an existing Secret holding the Slack bot token. Takes precedence over token. |
| marge.slack.existingSecretKey | string | `"slack-token"` | Key of the Slack bot token inside existingSecret |
| marge.circleci.token | string | `""` | CircleCI API token, optional: lets marge inspect private CircleCI projects and retry auto-cancelled builds. The chart writes it into a Secret; prefer existingSecret in production. |
| marge.circleci.existingSecret | string | `""` | Name of an existing Secret holding the CircleCI token. Takes precedence over token. |
| marge.circleci.existingSecretKey | string | `"token"` | Key of the CircleCI token inside existingSecret |
| schedule.daily.enabled | bool | `false` | Run the daily sweep. It sweeps every team whose policy file leaves the schedule enabled, and needs marge.github.app. |
| schedule.daily.schedule | string | `"0 6 * * *"` | Cron expression of the daily sweep, in the cluster's timezone unless timeZone is set. |
| schedule.daily.timeZone | string | `"Europe/Berlin"` | IANA timezone the cron expression is read in. |
| schedule.daily.actions | string | `"classify,approve,merge,refresh,retry,remedy,mark"` | Sweep steps the daily run performs. Every step here acts through the GitHub or CircleCI API; none of them writes code to a branch. |
| schedule.daily.dryRun | bool | `false` | Report every outcome without writing anything. Turn it on for the first runs on a new installation. |
| schedule.daily.activeDeadlineSeconds | int | `3600` | Seconds the daily run may take before Kubernetes stops it. |
| schedule.daily.successfulJobsHistoryLimit | int | `3` | Successful Jobs kept |
| schedule.daily.failedJobsHistoryLimit | int | `3` | Failed Jobs kept |
| schedule.daily.resources | object | `{"limits":{"cpu":1,"memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Container resources of the daily run |
| schedule.weekly.enabled | bool | `false` | Run the weekly rescue trigger. It stays off until the rescue run exists: the command it would run is not built yet, so args has no default and an enabled weekly CronJob without args fails to render. |
| schedule.weekly.schedule | string | `"0 5 * * 1"` | Cron expression of the weekly rescue trigger. |
| schedule.weekly.timeZone | string | `"Europe/Berlin"` | IANA timezone the cron expression is read in. |
| schedule.weekly.args | list | `[]` | Arguments the weekly run passes to the marge binary. |
| schedule.weekly.tokenAudience | string | `"kagent"` | Audience of the projected ServiceAccount token the weekly run presents to the agent platform gateway. Mounted at /var/run/secrets/kagent/token. |
| schedule.weekly.activeDeadlineSeconds | int | `3600` | Seconds the weekly run may take before Kubernetes stops it. |
| schedule.weekly.successfulJobsHistoryLimit | int | `3` | Successful Jobs kept |
| schedule.weekly.failedJobsHistoryLimit | int | `3` | Failed Jobs kept |
| schedule.weekly.resources | object | `{"limits":{"cpu":1,"memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Container resources of the weekly run |
| networkPolicy.enabled | bool | `true` | Create a NetworkPolicy: ingress to the MCP port from the selected namespaces, egress to DNS and HTTPS only (GitHub, CircleCI). |
| networkPolicy.ingressNamespaceSelector | object | `{}` | Namespaces allowed to reach the MCP port; an empty selector allows every namespace of the cluster. |
