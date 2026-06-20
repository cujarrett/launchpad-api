package main

import (
	"bytes"
	"fmt"
	"text/template"
)

var templates = map[string]*template.Template{}

func init() {
	defs := map[string]string{
		"XSpa":           xspaTemplate,
		"XApi":           xapiTemplate,
		"XSql":           xsqlTemplate,
		"XNoSql":         xnosqlTemplate,
		"XObjectStorage": xobjectstorageTemplate,
		"XTopic":         xtopicTemplate,
		"XSubscription":  xsubscriptionTemplate,
		"XWordpress":     xwordpressTemplate,
	}
	for kind, tmpl := range defs {
		templates[kind] = template.Must(template.New(kind).Parse(tmpl))
	}
}

// RenderResource renders a platform XR YAML manifest from a writeRequest.
func RenderResource(r writeRequest) (string, error) {
	tmpl, ok := templates[r.Kind]
	if !ok {
		return "", fmt.Errorf("unknown kind: %s", r.Kind)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, r); err != nil {
		return "", fmt.Errorf("render %s: %w", r.Kind, err)
	}
	return buf.String(), nil
}

// RenderNamespace renders a namespace manifest.
func RenderNamespace(name string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name)
}

// ────────────────────────────────────────────────
// Templates — one per platform resource kind
// ────────────────────────────────────────────────

const xspaTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XSpa
metadata:
  name: {{ .Name }}
spec:
  parameters:
    namespace: {{ .Params.namespace }}
    image: {{ .Params.image }}
    host: {{ .Params.host }}
    tlsIssuer: {{ or .Params.tlsIssuer "letsencrypt-prod" }}
    replicas: {{ or .Params.replicas 1 }}
{{- if .Params.contentSecurityPolicy }}
    contentSecurityPolicy: {{ .Params.contentSecurityPolicy }}
{{- end }}
`

const xapiTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XApi
metadata:
  name: {{ .Name }}
spec:
  parameters:
    namespace: {{ .Params.namespace }}
    image: {{ .Params.image }}
    port: {{ or .Params.port 8080 }}
    replicas: {{ or .Params.replicas 1 }}
{{- if .Params.host }}
    host: {{ .Params.host }}
    tlsIssuer: {{ or .Params.tlsIssuer "letsencrypt-prod" }}
{{- end }}
{{- if .Params.sqlRef }}
    sqlRef:
      name: {{ .Params.sqlRef }}
      backend: private-cloud
{{- end }}
{{- if .Params.nosqlRef }}
    nosqlRef:
      name: {{ .Params.nosqlRef }}
{{- end }}
{{- if .Params.objectStorageRefs }}
    objectStorageRefs:
      - name: {{ .Params.objectStorageRefs }}
{{- end }}
{{- if .Params.topicRef }}
    topicRef:
      name: {{ .Params.topicRef }}
{{- end }}
{{- if .Params.subscriptionRef }}
    subscriptionRef:
      name: {{ .Params.subscriptionRef }}
{{- end }}
{{- if .Params.secretRef }}
    secretRef:
      name: {{ index .Params.secretRef "name" }}
{{- end }}
{{- if .Params.cache }}
    cache:
      enabled: true
      backend: private-cloud
{{- end }}
{{- if .Params.readinessCheckPath }}
    readinessCheckPath: {{ .Params.readinessCheckPath }}
{{- end }}
`

const xsqlTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XSql
metadata:
  name: {{ .Name }}
spec:
  parameters:
    namespace: {{ .Params.namespace }}
    backend: {{ or .Params.backend "private-cloud" }}
    dataRetention: {{ or .Params.dataRetention "delete" }}
{{- if .Params.size }}
    size: {{ .Params.size }}
{{- end }}
`

const xnosqlTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XNoSql
metadata:
  name: {{ .Name }}
spec:
  parameters:
    namespace: {{ .Params.namespace }}
    dataRetention: {{ or .Params.dataRetention "delete" }}
{{- if .Params.region }}
    region: {{ .Params.region }}
{{- end }}
{{- if .Params.partitionKey }}
    partitionKey: {{ .Params.partitionKey }}
{{- end }}
{{- if .Params.partitionKeyType }}
    partitionKeyType: {{ .Params.partitionKeyType }}
{{- end }}
`

const xobjectstorageTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XObjectStorage
metadata:
  name: {{ .Name }}
spec:
  parameters:
    namespace: {{ .Params.namespace }}
    dataRetention: {{ or .Params.dataRetention "delete" }}
{{- if .Params.region }}
    region: {{ .Params.region }}
{{- end }}
`

const xtopicTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XTopic
metadata:
  name: {{ .Name }}
spec:
  parameters:
    streamName: {{ .Params.streamName }}
    subjects:
{{- range .Params.subjects }}
      - {{ . }}
{{- end }}
    retention: {{ or .Params.retention "limits" }}
    maxAge: {{ or .Params.maxAge "720h" }}
    replicas: {{ or .Params.replicas 3 }}
`

const xsubscriptionTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XSubscription
metadata:
  name: {{ .Name }}
spec:
  parameters:
    topicRef:
      name: {{ .Params.topicRef }}
{{- if .Params.filterSubject }}
    filterSubject: {{ .Params.filterSubject }}
{{- end }}
    deliverPolicy: {{ or .Params.deliverPolicy "all" }}
    ackPolicy: {{ or .Params.ackPolicy "explicit" }}
    ackWait: {{ or .Params.ackWait "30s" }}
`

const xwordpressTemplate = `apiVersion: platform.local.lab/v1alpha1
kind: XWordpress
metadata:
  name: {{ .Name }}
spec:
  parameters:
    namespace: {{ .Params.namespace }}
    host: {{ .Params.host }}
    dataRetention: {{ or .Params.dataRetention "retain" }}
    size: {{ or .Params.size "sm" }}
{{- if .Params.storageSize }}
    storageSize: {{ .Params.storageSize }}
{{- end }}
{{- if .Params.dbStorageSize }}
    dbStorageSize: {{ .Params.dbStorageSize }}
{{- end }}
`
