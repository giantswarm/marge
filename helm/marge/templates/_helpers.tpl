{{/*
Expand the name of the chart.
*/}}
{{- define "marge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "marge.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "marge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "marge.labels" -}}
helm.sh/chart: {{ include "marge.chart" . }}
{{ include "marge.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "marge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "marge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Selector labels of the MCP server, which is the Deployment and the Service.
The CronJob pods carry the same common labels, so the server's own selector
names its component as well and never reaches them.
*/}}
{{- define "marge.serverSelectorLabels" -}}
{{ include "marge.selectorLabels" . }}
app.kubernetes.io/component: mcp
{{- end }}

{{/*
Name of one scheduled sweep. Takes a dict of "context" and "component".
*/}}
{{- define "marge.cronName" -}}
{{- printf "%s-%s" (include "marge.fullname" .context) .component | trunc 52 | trimSuffix "-" }}
{{- end }}

{{/*
Name of the ServiceAccount of one scheduled sweep.
*/}}
{{- define "marge.cronServiceAccountName" -}}
{{- include "marge.cronName" . }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "marge.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "marge.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret the chart writes for inline tokens.
*/}}
{{- define "marge.tokenSecretName" -}}
{{- printf "%s-tokens" (include "marge.fullname" .) }}
{{- end }}

{{/*
Whether the chart writes its own token Secret: at least one inline credential
is set and not overridden by an existing Secret.
*/}}
{{- define "marge.writesTokenSecret" -}}
{{- if or (include "marge.writesGitHubToken" .) (include "marge.writesCircleCIToken" .) (include "marge.writesAppCredential" .) (include "marge.writesOAuthCredential" .) (include "marge.writesSlackToken" .) -}}
true
{{- end -}}
{{- end }}

{{- define "marge.writesGitHubToken" -}}
{{- if and .Values.marge.github.token (not .Values.marge.github.existingSecret) -}}true{{- end -}}
{{- end }}

{{- define "marge.writesCircleCIToken" -}}
{{- if and .Values.marge.circleci.token (not .Values.marge.circleci.existingSecret) -}}true{{- end -}}
{{- end }}

{{- define "marge.writesAppCredential" -}}
{{- if and .Values.marge.github.app.privateKey (not .Values.marge.github.app.existingSecret) -}}true{{- end -}}
{{- end }}

{{- define "marge.writesOAuthCredential" -}}
{{- if and .Values.marge.github.app.oauth.clientSecret (not .Values.marge.github.app.oauth.existingSecret) -}}true{{- end -}}
{{- end }}

{{- define "marge.writesSlackToken" -}}
{{- if and .Values.marge.slack.token (not .Values.marge.slack.existingSecret) -}}true{{- end -}}
{{- end }}

{{/*
Whether the App's OAuth client credentials are configured at all. Without
them muster has no client to run the sign-in with.
*/}}
{{- define "marge.hasOAuthCredential" -}}
{{- if or .Values.marge.github.app.oauth.existingSecret .Values.marge.github.app.oauth.clientSecret -}}
true
{{- end -}}
{{- end }}

{{/*
Name of the Secret that holds the App's OAuth client credentials, which is
what muster reads to run the sign-in.
*/}}
{{- define "marge.oauthSecretName" -}}
{{- default (include "marge.tokenSecretName" .) .Values.marge.github.app.oauth.existingSecret }}
{{- end }}

{{/*
Name of the Secret that holds the App credential, and the keys inside it.
The App path is what the schedule runs as, so its Secret is resolved once
here and read by both CronJobs.
*/}}
{{- define "marge.appSecretName" -}}
{{- default (include "marge.tokenSecretName" .) .Values.marge.github.app.existingSecret }}
{{- end }}

{{/*
Whether the App credential is configured at all. Without it a scheduled
sweep has no identity to act as.
*/}}
{{- define "marge.hasAppCredential" -}}
{{- if or .Values.marge.github.app.existingSecret .Values.marge.github.app.privateKey -}}
true
{{- end -}}
{{- end }}

{{/*
Environment of the App credential. The private key is a file and never an
environment variable: a PEM in the environment shows up in every process
listing of the pod.
*/}}
{{- define "marge.appEnv" -}}
- name: MARGE_GITHUB_APP_ID
  valueFrom:
    secretKeyRef:
      name: {{ include "marge.appSecretName" . }}
      key: {{ .Values.marge.github.app.idKey }}
- name: MARGE_GITHUB_APP_INSTALLATION_ID
  valueFrom:
    secretKeyRef:
      name: {{ include "marge.appSecretName" . }}
      key: {{ .Values.marge.github.app.installationIdKey }}
- name: MARGE_GITHUB_APP_PRIVATE_KEY_FILE
  value: /etc/marge/github-app/private-key.pem
{{- end }}

{{/*
Environment of the Slack bot token. Renders nothing when no token is
configured, and the run then posts no summary.
*/}}
{{- define "marge.slackEnv" -}}
{{- if or .Values.marge.slack.existingSecret .Values.marge.slack.token -}}
- name: MARGE_SLACK_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ default (include "marge.tokenSecretName" .) .Values.marge.slack.existingSecret }}
      key: {{ .Values.marge.slack.existingSecretKey }}
{{- end }}
{{- end }}

{{/*
Environment of the CircleCI token, which is optional everywhere.
*/}}
{{- define "marge.circleciEnv" -}}
{{- if or .Values.marge.circleci.existingSecret .Values.marge.circleci.token -}}
- name: CIRCLECI_CLI_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ default (include "marge.tokenSecretName" .) .Values.marge.circleci.existingSecret }}
      key: {{ if .Values.marge.circleci.existingSecret }}{{ .Values.marge.circleci.existingSecretKey }}{{ else }}circleci-token{{ end }}
{{- end }}
{{- end }}

{{/*
The scheduled runs of this release, as a YAML list. One entry renders one
CronJob and one ServiceAccount, so the CronJob template and the RBAC
template read the same list.

An entry of .Values.schedules takes the missing keys from
.Values.scheduleDefaults. The deprecated .Values.schedule.daily and
.Values.schedule.weekly are translated into an entry each, under the names
their resources already carry.
*/}}
{{- define "marge.schedules" -}}
{{- $entries := list }}
{{- range $entry := .Values.schedules }}
{{- $entries = append $entries (mergeOverwrite (deepCopy $.Values.scheduleDefaults) $entry) }}
{{- end }}
{{- if .Values.schedule.daily.enabled }}
{{- $entries = append $entries (merge (dict "name" "daily-sweep" "allTeams" true) .Values.schedule.daily) }}
{{- end }}
{{- if .Values.schedule.weekly.enabled }}
{{- $entries = append $entries (merge (dict "name" "weekly-rescue") .Values.schedule.weekly) }}
{{- end }}
{{- toYaml $entries }}
{{- end }}

{{/*
Refuses a scheduled run the chart cannot render, and names the entry.
*/}}
{{- define "marge.checkSchedule" -}}
{{- $entry := .entry }}
{{- if not $entry.name }}
{{- fail "every entry of schedules needs a name: it names the CronJob and its ServiceAccount." }}
{{- end }}
{{- if not $entry.schedule }}
{{- fail (printf "schedule entry %s needs a cron expression in schedule." $entry.name) }}
{{- end }}
{{- if and (not $entry.team) (not $entry.allTeams) (not $entry.args) }}
{{- fail (printf "schedule entry %s needs a team: the sweep it runs names one team, or args gives the whole command line." $entry.name) }}
{{- end }}
{{- if not (include "marge.hasAppCredential" .context) }}
{{- fail (printf "schedule entry %s needs marge.github.app: set app.existingSecret, or app.id, app.installationId and app.privateKey. A scheduled run acts as the sweep App and never as a person." $entry.name) }}
{{- end }}
{{- end }}
