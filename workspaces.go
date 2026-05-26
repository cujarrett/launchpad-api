package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var validWorkspaceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$|^[a-z0-9]$`)

type workspaceJSON struct {
	Name      string  `json:"name"`
	IsGuest   bool    `json:"isGuest"`
	ExpiresAt *string `json:"expiresAt,omitempty"`
}

type resourceJSON struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace"`
	Spec      map[string]any `json:"spec"`
}

type xrManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Parameters map[string]any `yaml:"parameters"`
	} `yaml:"spec"`
}

func (a *app) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	entries, err := a.gh.listDir(r.Context(), "")
	if err != nil {
		slog.Error("list workspaces", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	excluded := excludedWorkspaces()
	workspaces := make([]workspaceJSON, 0)
	for _, e := range entries {
		if e.Type == "dir" && !strings.HasPrefix(e.Name, ".") && !excluded[e.Name] {
			ws := workspaceJSON{Name: e.Name}
			if strings.HasPrefix(e.Name, guestPrefix) {
				ws.IsGuest = true
				if expiry := a.fetchGuestExpiry(r.Context(), e.Name); expiry != nil {
					s := expiry.Format(time.RFC3339)
					ws.ExpiresAt = &s
				}
			}
			workspaces = append(workspaces, ws)
		}
	}
	writeJSON(w, workspaces)
}

// excludedWorkspaces parses LAUNCHPAD_EXCLUDED_WORKSPACES (comma-separated)
// into a set for O(1) lookup.
func excludedWorkspaces() map[string]bool {
	val := os.Getenv("LAUNCHPAD_EXCLUDED_WORKSPACES")
	set := map[string]bool{}
	for _, name := range strings.Split(val, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			set[name] = true
		}
	}
	return set
}

func (a *app) handleListResources(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	entries, err := a.gh.listDir(r.Context(), name)
	if err != nil {
		slog.Error("list resources", "workspace", name, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	resources := make([]resourceJSON, 0)
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(e.Name, ".yaml") {
			continue
		}
		if e.Name == "namespace.yaml" {
			continue
		}

		content, err := a.gh.fileContent(r.Context(), e.Path)
		if err != nil {
			slog.Warn("fetch file", "path", e.Path, "err", err)
			continue
		}

		var m xrManifest
		if err := yaml.Unmarshal(content, &m); err != nil {
			slog.Warn("parse yaml", "path", e.Path, "err", err)
			continue
		}

		if !strings.HasPrefix(m.APIVersion, "platform.local.lab") {
			continue
		}

		ns, _ := m.Spec.Parameters["namespace"].(string)
		resources = append(resources, resourceJSON{
			Name:      m.Metadata.Name,
			Kind:      m.Kind,
			Namespace: ns,
			Spec:      m.Spec.Parameters,
		})
	}
	writeJSON(w, resources)
}

// handleCreateWorkspace creates a new workspace by writing namespace.yaml to GitHub.
func (a *app) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireContributor(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !validWorkspaceName.MatchString(req.Name) {
		http.Error(w, "invalid workspace name: must be lowercase alphanumeric with hyphens", http.StatusUnprocessableEntity)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	nsPath := fmt.Sprintf("%s/namespace.yaml", req.Name)
	commitMsg := fmt.Sprintf("feat: create workspace %s", req.Name)
	if err := a.gh.upsertFile(ctx, nsPath, commitMsg, RenderNamespace(req.Name)); err != nil {
		slog.Error("create workspace", "err", err)
		http.Error(w, "failed to create workspace", http.StatusInternalServerError)
		return
	}

	a.triggerArgoSync(req.Name)
	slog.Info("created workspace", "name", req.Name)
	w.WriteHeader(http.StatusCreated)
}

// handleDeleteWorkspace deletes a workspace by removing namespace.yaml from GitHub.
// Returns 409 if the workspace still has resource files.
func (a *app) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !requireContributor(w, r) {
		return
	}
	name := r.PathValue("name")

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	entries, err := a.gh.listDir(ctx, name)
	if err != nil {
		slog.Error("delete workspace: list dir", "workspace", name, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	for _, e := range entries {
		if e.Type == "file" && e.Name != "namespace.yaml" {
			http.Error(w, "workspace is not empty", http.StatusConflict)
			return
		}
	}

	nsPath := fmt.Sprintf("%s/namespace.yaml", name)
	commitMsg := fmt.Sprintf("feat: delete workspace %s", name)
	if err := a.gh.deleteFile(ctx, nsPath, commitMsg); err != nil {
		slog.Error("delete workspace", "err", err)
		http.Error(w, "failed to delete workspace", http.StatusInternalServerError)
		return
	}

	// Best-effort: delete the ArgoCD Application so it doesn't sit in a
	// ComparisonError state while the ApplicationSet reconciles.
	if a.dynClient != nil {
		argoGVR := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
		if delErr := a.dynClient.Resource(argoGVR).Namespace("argocd").Delete(ctx, name, metav1.DeleteOptions{}); delErr != nil {
			slog.Warn("delete argocd app", "workspace", name, "err", delErr)
		}
	}

	slog.Info("deleted workspace", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// triggerArgoSync patches the ArgoCD Application for the given workspace with
// a hard-refresh annotation so ArgoCD picks up the latest Git commit immediately
// instead of waiting for the default polling interval (~3 min).
// Called fire-and-forget. For new workspaces the Application doesn't exist yet
// (ApplicationSet creates it within ~60 s), so on a not-found error we retry
// once after 30 s to avoid relying on the 3-min polling interval.
func (a *app) triggerArgoSync(workspace string) {
	if a.dynClient == nil {
		return
	}
	go func() {
		a.doArgoSyncPatch(workspace)
	}()
}

func (a *app) doArgoSyncPatch(workspace string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argoGVR := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	patch := []byte(`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"hard"}}}`)
	_, err := a.dynClient.Resource(argoGVR).Namespace("argocd").Patch(
		ctx, workspace, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	if err == nil {
		return
	}
	// App may not exist yet for brand-new workspaces. Schedule one retry so
	// we still get a fast sync once the ApplicationSet creates the Application,
	// without relying on ArgoCD's 3-minute polling interval.
	slog.Debug("argo sync trigger", "workspace", workspace, "err", err, "retry_in", "30s")
	time.AfterFunc(30*time.Second, func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		if _, err2 := a.dynClient.Resource(argoGVR).Namespace("argocd").Patch(
			ctx2, workspace, types.MergePatchType, patch, metav1.PatchOptions{},
		); err2 != nil {
			slog.Debug("argo sync trigger retry", "workspace", workspace, "err", err2)
		}
	})
}

// fetchGuestExpiry reads a guest workspace's meta.yaml and returns the expiry time.
// Returns nil on any error so the workspace is still listed without expiry data.
func (a *app) fetchGuestExpiry(ctx context.Context, workspace string) *time.Time {
	content, err := a.gh.fileContent(ctx, workspace+"/meta.yaml")
	if err != nil {
		return nil
	}
	var meta struct {
		CreatedAt string `yaml:"createdAt"`
	}
	if err := yaml.Unmarshal(content, &meta); err != nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, meta.CreatedAt)
	if err != nil {
		return nil
	}
	expiry := t.Add(guestTTL)
	return &expiry
}
