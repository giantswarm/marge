# marge

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

A housekeeping tool for dependency update PRs, served as an MCP server

Runs `marge serve --transport streamable-http`: the `sweep` and `mark` MCP tools on `/mcp`, with `/healthz` and `/readyz` for the probes. Register the Service as a streamable-http MCP server in muster to make the tools available to people and agents.

marge needs a GitHub token with the permissions listed in the [repository README](https://github.com/giantswarm/marge#setup). Pass it inline (`marge.github.token`, the chart writes a Secret) or point at an existing Secret (`marge.github.existingSecret`, key `marge.github.existingSecretKey`). A CircleCI token (`marge.circleci.*`) is optional; it lets marge inspect private CircleCI projects and retry auto-cancelled builds.

## The scheduled sweep

`schedules` holds one entry per scheduled run, and each entry renders one CronJob. An entry names a team and a cron expression, so every team carries its own cadence, its own steps and its own suspension:

```yaml
schedules:
  - name: bumblebee-sweep
    team: bumblebee
    schedule: "0 6 * * 1-5"
  - name: atlas-sweep
    team: atlas
    schedule: "10 6 * * 1-5"
```

An entry takes the keys it does not set from `scheduleDefaults`. `suspend` pauses one run and `suspendAll` pauses every run, in both cases without deleting the entry. Stagger the expressions: every run acts as the same GitHub App and shares its rate limit.

A run sweeps one team: it classifies, approves, merges, refreshes stale branches, retries cancelled builds and applies the catalogue's rules. Every one of those steps acts through the GitHub or CircleCI API, so a scheduled run writes no code to any branch. The team's policy file in `giantswarm/github` says what may merge.

The schedule acts as the App and never as a person. Set `marge.github.app` and the CronJob mints an installation token per repository, for one hour, and stores none. Without `marge.github.app` the chart refuses to render an enabled schedule. The App's credentials live in 1Password, in the `Team Bumblebee` vault, in the item `marge sweep GitHub App`; see [docs/github-app.md](https://github.com/giantswarm/marge/blob/main/docs/github-app.md).

Set `marge.slack.token` and each run posts one summary to the channel the team's policy names. A run that changed nothing posts nothing. A scheduled run passes `--post-summary`, which a sweep by hand does not, so a manual sweep stays out of the team channels.

An entry that sets `args` passes them to the binary as the whole command line, and `tokenAudience` mounts a projected ServiceAccount token at `/var/run/secrets/kagent`. That pair is how the weekly rescue trigger will run. Keep such an entry suspended until the rescue command exists.

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
| muster.timeout | int | `120` | Seconds muster waits on a connection to this server. The sweep reads the checks of every PR in a team's queue, which passes the CRD default of 30. The CRD caps the value at 300. |
| muster.github.issuer | string | `"https://github.com/login/oauth"` | Issuer the grants are filed under, and the identity muster files them under, so it must match the value the other GitHub-backed servers use. GitHub publishes no discovery document, so the endpoints below are pinned instead. |
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
| resources | object | `{"limits":{"cpu":"500m","ephemeral-storage":"1Gi","memory":"256Mi"},"requests":{"cpu":"50m","ephemeral-storage":"50Mi","memory":"64Mi"}}` | Container resources. The pod mounts an emptyDir on /tmp, so ephemeral-storage is bounded as well: a container that mounts one without both bounds is refused by the restricted policies. |
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
| marge.github.app.oauth.existingSecret | string | `""` | Name of an existing Secret holding the OAuth client credentials. Takes precedence over the inline values. Defaults to marge.github.app.existingSecret, so one App Secret that carries the client keys below needs no second name. |
| marge.github.app.oauth.clientIdKey | string | `"github-app-client-id"` | Key of the client ID inside the Secret |
| marge.github.app.oauth.clientSecretKey | string | `"github-app-client-secret"` | Key of the client secret inside the Secret |
| marge.slack.token | string | `""` | Bot token of the sweep's own Slack app, which holds chat:write and nothing else. Without it a scheduled sweep does its work and posts no summary. |
| marge.slack.existingSecret | string | `""` | Name of an existing Secret holding the Slack bot token. Takes precedence over token. |
| marge.slack.existingSecretKey | string | `"slack-token"` | Key of the Slack bot token inside existingSecret |
| marge.circleci.token | string | `""` | CircleCI API token, optional: lets marge inspect private CircleCI projects and retry auto-cancelled builds. The chart writes it into a Secret; prefer existingSecret in production. |
| marge.circleci.existingSecret | string | `""` | Name of an existing Secret holding the CircleCI token. Takes precedence over token. |
| marge.circleci.existingSecretKey | string | `"token"` | Key of the CircleCI token inside existingSecret |
| suspendAll | bool | `false` | Suspend every scheduled run without deleting its entry. A single run is suspended on its own entry instead. |
| scheduleDefaults | object | `{"actions":"classify,approve,merge,refresh,retry,remedy,mark","activeDeadlineSeconds":3600,"dryRun":false,"failedJobsHistoryLimit":3,"resources":{"limits":{"cpu":1,"ephemeral-storage":"1Gi","memory":"512Mi"},"requests":{"cpu":"100m","ephemeral-storage":"50Mi","memory":"128Mi"}},"successfulJobsHistoryLimit":3,"timeZone":"Europe/Berlin"}` | Values every entry of schedules takes for the keys it does not set itself. |
| scheduleDefaults.timeZone | string | `"Europe/Berlin"` | IANA timezone the cron expressions are read in. |
| scheduleDefaults.actions | string | `"classify,approve,merge,refresh,retry,remedy,mark"` | Sweep steps a scheduled sweep performs. Every step here acts through the GitHub or CircleCI API; none of them writes code to a branch. |
| scheduleDefaults.dryRun | bool | `false` | Report every outcome without writing anything. Turn it on for the first runs on a new installation. |
| scheduleDefaults.activeDeadlineSeconds | int | `3600` | Seconds a scheduled run may take before Kubernetes stops it. |
| scheduleDefaults.successfulJobsHistoryLimit | int | `3` | Successful Jobs kept |
| scheduleDefaults.failedJobsHistoryLimit | int | `3` | Failed Jobs kept |
| scheduleDefaults.resources | object | `{"limits":{"cpu":1,"ephemeral-storage":"1Gi","memory":"512Mi"},"requests":{"cpu":"100m","ephemeral-storage":"50Mi","memory":"128Mi"}}` | Container resources of a scheduled run. The pod mounts an emptyDir on /tmp, so ephemeral-storage is bounded as well: a container that mounts one without both bounds is refused by the restricted policies. |
| schedules | list | `[]` | Scheduled runs, one CronJob each. An entry sweeps one team, so every team carries its own cadence and its own suspension. Needs marge.github.app. Stagger the expressions: every run acts as the same GitHub App and shares its rate limit. |
| networkPolicy.enabled | bool | `true` | Create a NetworkPolicy: ingress to the MCP port from the selected namespaces, egress to DNS and HTTPS only (GitHub, CircleCI). |
| networkPolicy.ingressNamespaceSelector | object | `{}` | Namespaces allowed to reach the MCP port; an empty selector allows every namespace of the cluster. |
