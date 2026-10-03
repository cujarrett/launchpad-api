package main

import (
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	err := validate(writeRequest{Kind: "Api"})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected name error, got %v", err)
	}
}

func TestValidate_InvalidName(t *testing.T) {
	err := validate(writeRequest{Kind: "Api", Name: "UPPERCASE"})
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

func TestValidate_Api_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "Api",
		Name:   "my-api",
		Params: map[string]any{"image": "ghcr.io/foo/bar:latest"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Api_MissingImage(t *testing.T) {
	err := validate(writeRequest{Kind: "Api", Name: "my-api"})
	if err == nil || !strings.Contains(err.Error(), "params.image") {
		t.Fatalf("expected image error, got %v", err)
	}
}

func TestValidate_Spa_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "Spa",
		Name:   "my-spa",
		Params: map[string]any{"image": "ghcr.io/foo/spa:1.0", "host": "spa.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Spa_MissingHost(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "Spa",
		Name:   "my-spa",
		Params: map[string]any{"image": "ghcr.io/foo/spa:1.0"},
	})
	if err == nil || !strings.Contains(err.Error(), "params.host") {
		t.Fatalf("expected host error, got %v", err)
	}
}

func TestValidate_Topic_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "Topic",
		Name:   "my-topic",
		Params: map[string]any{"streamName": "events", "subjects": []string{"foo.>"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Subscription_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "Subscription",
		Name:   "my-sub",
		Params: map[string]any{"topicRef": "events"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Wordpress_Valid(t *testing.T) {
	err := validate(writeRequest{
		Kind:   "Wordpress",
		Name:   "my-wp",
		Params: map[string]any{"host": "wp.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Line breaks YAML honours beyond \n and \r, and a list item, which the old
// string-only check never looked at.
func TestRenderResource_Injection(t *testing.T) {
	cases := map[string]writeRequest{
		"newline":   {Kind: "Api", Name: "a", Params: map[string]any{"namespace": "dev", "image": "foo\nmetadata: {namespace: kube-system}"}},
		"nel":       {Kind: "Api", Name: "a", Params: map[string]any{"namespace": "dev", "image": "foo\u0085evil: x"}},
		"separator": {Kind: "Api", Name: "a", Params: map[string]any{"namespace": "dev", "image": "foo\u2028evil: x"}},
		"list item": {Kind: "Topic", Name: "a", Params: map[string]any{"namespace": "dev", "streamName": "S", "subjects": []any{"a\n---\nkind: RoleBinding"}}},
		"map value": {Kind: "Api", Name: "a", Params: map[string]any{"namespace": "dev", "image": "i", "secretRef": map[string]any{"name": "s\n    evil: x"}}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := RenderResource(req)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			dec := yaml.NewDecoder(strings.NewReader(out))
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("parse: %v\n%s", err, out)
			}
			if err := dec.Decode(&map[string]any{}); err != io.EOF {
				t.Fatalf("rendered more than one document:\n%s", out)
			}
			meta := doc["metadata"].(map[string]any)
			if meta["namespace"] != "dev" {
				t.Errorf("namespace = %v, want dev:\n%s", meta["namespace"], out)
			}
			for k := range doc["spec"].(map[string]any)["parameters"].(map[string]any) {
				if k == "evil" {
					t.Errorf("injected key reached parameters:\n%s", out)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RenderResource
// ─────────────────────────────────────────────────────────────────────────────

func TestRenderResource_Api(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:      "Api",
		Name:      "my-api",
		Workspace: "dev",
		Params:    map[string]any{"image": "ghcr.io/foo/bar:1.0", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The poll-interval annotation is what keeps AWS binding secrets from waiting
	// a full minute for Crossplane's next render pass.
	for _, want := range []string{"kind: Api", `name: "my-api"`, `image: "ghcr.io/foo/bar:1.0"`, `crossplane.io/poll-interval: "5s"`} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in rendered YAML:\n%s", want, yaml)
		}
	}
}

func TestRenderResource_Api_DefaultPort(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "Api",
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

func TestRenderResource_Spa(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "Spa",
		Name:   "my-spa",
		Params: map[string]any{"image": "foo:latest", "host": "spa.example.com", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"kind: Spa", `host: "spa.example.com"`} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in rendered YAML:\n%s", want, yaml)
		}
	}
}

func TestRenderResource_Spa_DefaultTLSIssuer(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "Spa",
		Name:   "my-spa",
		Params: map[string]any{"image": "foo:latest", "host": "spa.example.com", "namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, `tlsIssuer: "letsencrypt-prod"`) {
		t.Errorf("expected default tlsIssuer in rendered YAML:\n%s", yaml)
	}
}

func TestRenderResource_Api_OptionalHost(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "Api",
		Name:   "my-api",
		Params: map[string]any{"image": "foo:latest", "namespace": "dev", "host": "api.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, `host: "api.example.com"`) {
		t.Errorf("expected host in rendered YAML:\n%s", yaml)
	}
}

func TestRenderResource_Sql(t *testing.T) {
	yaml, err := RenderResource(writeRequest{
		Kind:   "Sql",
		Name:   "my-db",
		Params: map[string]any{"namespace": "dev"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "kind: Sql") {
		t.Errorf("expected kind Sql in rendered YAML:\n%s", yaml)
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
	// The sync-wave is what keeps namespace teardown from deadlocking on
	// managed-resource finalizers, and the two labels are what admission policy
	// matches on, so assert them rather than trust them.
	for _, want := range []string{
		"kind: Namespace",
		"name: my-workspace",
		`platform.local.lab/workloads: "true"`,
		"istio-injection: enabled",
		`argocd.argoproj.io/sync-wave: "-1"`,
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected %q in namespace YAML:\n%s", want, yaml)
		}
	}
}

func TestRenderGuestNamespace(t *testing.T) {
	out := RenderGuestNamespace("guest-phantom-burrito", "demo3")

	for _, want := range []string{
		"name: guest-phantom-burrito",
		"launchpad.local.lab/slot: demo3",
		`platform.local.lab/workloads: "true"`,
		"istio-injection: enabled",
		`argocd.argoproj.io/sync-wave: "-1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "kind: RoleBinding") {
		t.Errorf("RoleBinding belongs in rbac.yaml, not namespace.yaml:\n%s", out)
	}
}

func TestRenderGuestRBAC(t *testing.T) {
	out := RenderGuestRBAC("guest-phantom-burrito")

	for _, want := range []string{
		"kind: RoleBinding",
		"name: secret-mirror-writer",
		"namespace: guest-phantom-burrito",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestIsGuestName(t *testing.T) {
	cases := map[string]bool{
		"guest-phantom-burrito":            true,
		"phantom-burrito":                  false,
		"guest-a/../my-vinyl":              false,
		"guest-a/../../launchpad":          false,
		"guest-a?ref=main":                 false,
		"guest-" + strings.Repeat("a", 64): false,
	}
	for name, want := range cases {
		if got := isGuestName(name); got != want {
			t.Errorf("isGuestName(%q) = %v, want %v", name, got, want)
		}
	}
}
