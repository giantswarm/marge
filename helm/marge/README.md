# marge

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

A housekeeping tool for dependency update PRs, served as an MCP server

Runs `marge serve --transport streamable-http`: the `sweep` and `mark` MCP tools on `/mcp`, with `/healthz` and `/readyz` for the probes. Register the Service as a streamable-http MCP server in muster to make the tools available to people and agents.

marge needs a GitHub token with the permissions listed in the [repository README](https://github.com/giantswarm/marge#setup). Pass it inline (`marge.github.token`, the chart writes a Secret) or point at an existing Secret (`marge.github.existingSecret`, key `marge.github.existingSecretKey`). A CircleCI token (`marge.circleci.*`) is optional; it lets marge inspect private CircleCI projects and retry auto-cancelled builds.

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
| marge.github.token | string | `""` | GitHub token marge uses (needs the permissions listed in the repository README). The chart writes it into a Secret; prefer existingSecret in production. |
| marge.github.existingSecret | string | `""` | Name of an existing Secret holding the GitHub token. Takes precedence over token. |
| marge.github.existingSecretKey | string | `"token"` | Key of the GitHub token inside existingSecret |
| marge.circleci.token | string | `""` | CircleCI API token, optional: lets marge inspect private CircleCI projects and retry auto-cancelled builds. The chart writes it into a Secret; prefer existingSecret in production. |
| marge.circleci.existingSecret | string | `""` | Name of an existing Secret holding the CircleCI token. Takes precedence over token. |
| marge.circleci.existingSecretKey | string | `"token"` | Key of the CircleCI token inside existingSecret |
| networkPolicy.enabled | bool | `true` | Create a NetworkPolicy: ingress to the MCP port from the selected namespaces, egress to DNS and HTTPS only (GitHub, CircleCI). |
| networkPolicy.ingressNamespaceSelector | object | `{}` | Namespaces allowed to reach the MCP port; an empty selector allows every namespace of the cluster. |
