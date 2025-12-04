{{/*
Expand the name of the chart.
*/}}
{{- define "hami-dra-driver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If the release name already contains the chart name it is used as the full name.
*/}}
{{- define "hami-dra-driver.fullname" -}}
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
Allow the release namespace to be overridden for multi-namespace deployments in combined charts.
*/}}
{{- define "hami-dra-driver.namespace" -}}
  {{- if .Values.namespaceOverride }}
    {{- .Values.namespaceOverride }}
  {{- else }}
    {{- .Release.Namespace }}
  {{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "hami-dra-driver.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Standard labels applied to all high-level objects (DaemonSet, ClusterRole, ...).
Includes templateLabels. Do NOT apply this directly to pod templates.
*/}}
{{- define "hami-dra-driver.labels" -}}
helm.sh/chart: {{ include "hami-dra-driver.chart" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "hami-dra-driver.templateLabels" . }}
{{- end }}

{{/*
Minimal labels applied to pod templates.
*/}}
{{- define "hami-dra-driver.templateLabels" -}}
app.kubernetes.io/name: {{ include "hami-dra-driver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Per-component selector label. Requires a dict with keys "context" and "componentName".
Usage: include "hami-dra-driver.selectorLabels" (dict "context" . "componentName" "kubelet-plugin")
*/}}
{{- define "hami-dra-driver.selectorLabels" -}}
{{- if and (hasKey . "componentName") (hasKey . "context") -}}
{{- printf "%s-component: %s" (include "hami-dra-driver.name" .context) .componentName }}
{{- else -}}
{{- fail "hami-dra-driver.selectorLabels: both 'context' and 'componentName' are required" -}}
{{- end }}
{{- end }}

{{/*
Full image reference with tag. Default tag is "v" + .Chart.AppVersion.
*/}}
{{- define "hami-dra-driver.fullimage" -}}
{{- $tag := .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default $tag) | quote }}
{{- end }}

{{/*
Name of the ServiceAccount to use.
Defaults to <fullname>-service-account.
*/}}
{{- define "hami-dra-driver.serviceAccountName" -}}
{{- $default := printf "%s-service-account" (include "hami-dra-driver.fullname" .) }}
{{- if .Values.serviceAccount.create }}
{{- default $default .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Detect the highest available resource.k8s.io API version from cluster capabilities.
Can be overridden via .Values.resourceApiVersion.
*/}}
{{- define "hami-dra-driver.resourceApiVersion" -}}
{{- if .Values.resourceApiVersion }}
{{- .Values.resourceApiVersion }}
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1" -}}
resource.k8s.io/v1
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta2" -}}
resource.k8s.io/v1beta2
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta1" -}}
resource.k8s.io/v1beta1
{{- else -}}
{{- fail "hami-dra-driver.resourceApiVersion: no supported resource.k8s.io API version found (requires v1, v1beta2, or v1beta1). Set resourceApiVersion explicitly to bypass." -}}
{{- end -}}
{{- end }}
