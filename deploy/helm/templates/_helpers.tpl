{{/*
Expand the name of the chart.
*/}}
{{- define "acs-sandbox-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "acs-sandbox-manager.fullname" -}}
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
{{- define "acs-sandbox-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "acs-sandbox-manager.labels" -}}
helm.sh/chart: {{ include "acs-sandbox-manager.chart" . }}
{{ include "acs-sandbox-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "agent-sandbox-operator.labels" -}}
helm.sh/chart: {{ include "acs-sandbox-manager.chart" . }}
{{ include "agent-sandbox-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "acs-sandbox-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "acs-sandbox-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
component: sandbox-manager
{{- end }}

{{- define "agent-sandbox-operator.selectorLabels" -}}
app.kubernetes.io/name: agent-sandbox-operator
app.kubernetes.io/instance: {{ .Release.Name }}
component: agent-sandbox-operator
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "acs-sandbox-manager.serviceAccountName" -}}
{{- default (include "acs-sandbox-manager.fullname" .) .Values.serviceAccount.name }}
{{- end }}
