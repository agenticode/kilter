{{- define "kilter.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "kilter.labels" -}}
app.kubernetes.io/name: kilter
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kilter.brainURL" -}}
{{- if .Values.brain.externalURL }}{{ .Values.brain.externalURL }}{{- else }}http://{{ .Release.Name }}-brain:8180{{- end }}
{{- end }}

{{/* Stable random token: reuse the existing secret's value on upgrade. */}}
{{- define "kilter.token" -}}
{{- if .Values.token }}{{ .Values.token }}
{{- else }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (printf "%s-token" .Release.Name) }}
{{- if $existing }}{{ index $existing.data "token" | b64dec }}{{ else }}{{ randAlphaNum 40 }}{{ end }}
{{- end }}
{{- end }}

{{- define "kilter.securityContext" -}}
readOnlyRootFilesystem: true
allowPrivilegeEscalation: false
capabilities:
  drop: [ALL]
{{- end }}
