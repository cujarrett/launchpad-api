package main

import (
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// validate
// ─────────────────────────────────────────────────────────────────────────────

func TestValidate_MissingKind(t *testing.T) {
	err := validate(writeRequest{Name: "foo"})
	if err == nil || !strings.Contains(err.Error(), "kind is required") {
		t.Fatalf("expected kind error, got %v", err)
	}
}

func TestValidate_MissingName(t *testing.T) {
	err := validate(writeRequest{Kind: "XApi"})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected name error, got %v", err)
	}
}

func TestValidate_InvalidName(t *testing.T) {
	err := validate(writeRequest{Kind: "XApi", Name: "UPPERCASE"})
	if err == nil || !strings.Contains(err.Error(), "invalid resource name") {
		t.Fatalf("expected name validation error, got %v", err)
	}
}

func TestValidate_UnknownKind(t *testing.T) {
	err := validate(writeRequest{Kind: "XFoo", Name: "my-resource"})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("expected unknown kind error, got %v", err)
	}
}

func TestValidate_XApi_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XApi",
		Name:   "my-api",
		Params: map[string]any{"image": "ghcr.io/foo/bar:latest"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_XApi_MissingImage(t *testing.T) {
	err := validate(writeRequest{Kind: "XApi", Name: "my-api"})
	if err == nil || !strings.Contains(err.Error(), "params.image") {
		t.Fatalf("expected image error, got %v", err)
	}
}

func TestValidate_XSpa_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XSpa",
		Name:   "my-spa",
		Params: map[string]any{"image": "ghcr.io/foo/spa:1.0", "host": "spa.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_XSpa_MissingHost(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XSpa",
		Name:   "my-spa",
		Params: map[string]any{"image": "ghcr.io/foo/spa:1.0"},
	})
	if err == nil || !strings.Contains(err.Error(), "params.host") {
		t.Fatalf("expected host error, got %v", err)
	}
}

func TestValidate_XTopic_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XTopic",
		Name:   "my-topic",
		Params: map[string]any{"streamName": "events", "subjects": []string{"foo.>"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_XSubscription_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XSubscription",
		Name:   "my-sub",
		Params: map[string]any{"topicRef": "events"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_XWordpress_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XWordpress",
		Name:   "my-wp",
		Params: map[string]any{"host": "wp.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_NewlineInjection(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XApi",
		Name:   "my-api",
		Params: map[string]any{"image": "foo\nbar: injected"},
	})
	if err == nil || !strings.Contains(err.Error(), "newlines are not allowed") {
		t.Fatalf("expected newline error, got %v", err)
	}
}

func TestValidate_CarriageReturnInjection(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "XApi",
		Name:   "my-api",
		Params: map[string]any{"image": "foo\rbar"},
	})
	if err == nil || !strings.Contains(err.Error(), "newlines are not allowed") {
		t.Fatalf("expected newline error, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RenderResource
// ─────────────────────────────────────────────────────────────────────────────

func TestRenderResource_XApi(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:      "XApi",
		Name:      "my-api",
		Workspace: "dev",
		Params:    map[string]any{"image": "ghcr.io/foo/bar:1.0", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"kind: XApi", "name: my-api", "image: ghcr.io/foo/bar:1.0"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in rendered YAML:\n%s", want, yaml)
		}
	}
}

func TestRenderResource_XApi_DefaultPort(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "XApi",
		Name:   "my-api",
		Params: map[string]any{"image": "foo:latest", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "port: 8080") {
		t.Errorf("expected default port 8080 in rendered YAML:\n%s", yaml)
	}
}

func TestRenderResource_XSpa(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "XSpa",
		Name:   "my-spa",
		Params: map[string]any{"image": "foo:latest", "host": "spa.example.com", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"kind: XSpa", "host: spa.example.com"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in rendered YAML:\n%s", want, yaml)
		}
	}
}

func TestRenderResource_XSpa_DefaultTLSIssuer(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "XSpa",
		Name:   "my-spa",
		Params: map[string]any{"image": "foo:latest", "host": "spa.example.com", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "tlsIssuer: letsencrypt-prod") {
		t.Errorf("expected default tlsIssuer in rendered YAML:\n%s", yaml)
	}
}

func TestRenderResource_XApi_OptionalHost(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "XApi",
		Name:   "my-api",
		Params: map[string]any{"image": "foo:latest", "namespace": "dev", "host": "api.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "host: api.example.com") {
		t.Errorf("expected host in rendered YAML:\n%s", yaml)
	}
}

func TestRenderResource_XSql(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "XSql",
		Name:   "my-db",
		Params: map[string]any{"namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "kind: XSql") {
		t.Errorf("expected kind XSql in rendered YAML:\n%s", yaml)
	}
}

func TestRenderResource_UnknownKind(t *testing.T) {
	_, err := RenderResource(writeRequest{Kind: "XFoo", Name: "x"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestRenderNamespace(t *testing.T) {
	yaml := RenderNamespace("my-workspace")
	for _, want := range []string{"kind: Namespace", "name: my-workspace"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in namespace YAML:\n%s", want, yaml)
		}
	}
}
