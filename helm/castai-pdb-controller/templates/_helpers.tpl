{{/*
Expand the name of the chart.
*/}}
{{- define "castai-pdb-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "castai-pdb-controller.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- printf "%s" $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "castai-pdb-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "castai-pdb-controller.labels" -}}
helm.sh/chart: {{ include "castai-pdb-controller.chart" . }}
app.kubernetes.io/name: {{ include "castai-pdb-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }} 