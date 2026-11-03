{{- define "lastwords.name" -}}lastwords{{- end -}}
{{- define "lastwords.fullname" -}}{{- if contains "lastwords" .Release.Name -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{- else -}}{{ printf "%s-lastwords" .Release.Name | trunc 63 | trimSuffix "-" }}{{- end -}}{{- end -}}
{{- define "lastwords.labels" -}}
app.kubernetes.io/name: lastwords
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
{{- define "lastwords.selector" -}}
app.kubernetes.io/name: lastwords
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "lastwords.serviceAccountName" -}}{{- if .Values.serviceAccount.create -}}{{ default (include "lastwords.fullname" .) .Values.serviceAccount.name }}{{- else -}}{{ default "default" .Values.serviceAccount.name }}{{- end -}}{{- end -}}
