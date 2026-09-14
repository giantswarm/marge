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
Whether the chart writes its own token Secret: at least one inline token is set
and not overridden by an existing Secret.
*/}}
{{- define "marge.writesTokenSecret" -}}
{{- if or (and .Values.marge.github.token (not .Values.marge.github.existingSecret)) (and .Values.marge.circleci.token (not .Values.marge.circleci.existingSecret)) -}}
true
{{- end -}}
{{- end }}
