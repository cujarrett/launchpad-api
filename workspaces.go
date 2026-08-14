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
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var validWorkspaceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$|^[a-z0-9]$`)

type workspaceJSON struct {
	Name       string            `json:"name"`
	IsGuest    bool              `json:"isGuest"`
	ExpiresAt  *string           `json:"expiresAt,omitempty"`
	PhaseTimes map[string]string `json:"phaseTimes,omitempty"`
	DoneAt     *string           `json:"doneAt,omitempty"`
}

type guestMeta struct {
	expiry     *time.Time
	phaseTimes map[string]string
	doneAt     *string
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

// workspacesCacheTTL bounds how stale the /api/workspaces listing can be. The
// UI polls this endpoint frequently, and hits it again on every navigation
// back to the home page; without a cache, each of those re-fans-out a GitHub
// API call per guest workspace, so latency grows with sandbox count and every
// occasional slow GitHub response is fully exposed to whoever's clicking
// around at that moment. Held generously long since create/delete invalidate
// it immediately, so staleness is bounded by actual mutations, not this TTL.
const workspacesCacheTTL = 20 * time.Second

func (a *app) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	a.wsCacheMu.Lock()
	if !a.wsCacheAt.IsZero() && time.Since(a.wsCacheAt) < workspacesCacheTTL {
		cached := a.wsCache
		a.wsCacheMu.Unlock()
		writeJSON(w, cached)
		return
	}
	a.wsCacheMu.Unlock()

	entries, err := a.gh.listDir(r.Context(), "")
	if err != nil {
		slog.Error("list workspaces", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	excluded := excludedWorkspaces()

	// Collect dirs first, then fetch guest expiry in parallel.
	var dirs []string
	for _, e := range entries {
		if e.Type == "dir" && !strings.HasPrefix(e.Name, ".") && !excluded[e.Name] {
			dirs = append(dirs, e.Name)
		}
	}

	workspaces := make([]workspaceJSON, len(dirs))
	var wg sync.WaitGroup
	for i, name := range dirs {
		workspaces[i] = workspaceJSON{Name: name}
		if strings.HasPrefix(name, guestPrefix) {
			workspaces[i].IsGuest = true
			wg.Add(1)
			go func(idx int, wsName string) {
				defer wg.Done()
				meta := a.fetchGuestMeta(r.Context(), wsName)
				if meta.expiry != nil {
					s := meta.expiry.Format(time.RFC3339)
					workspaces[idx].ExpiresAt = &s
				}
				workspaces[idx].PhaseTimes = meta.phaseTimes
				workspaces[idx].DoneAt = meta.doneAt
			}(i, name)
		}
	}
	wg.Wait()

	a.wsCacheMu.Lock()
	a.wsCache = workspaces
	a.wsCacheAt = time.Now()
	a.wsCacheMu.Unlock()

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

// resourcesCacheTTL bounds how stale the /api/workspaces/{name}/resources
// listing can be for guest workspaces. Fetching each resource file is a
// separate GitHub API call, so without a cache repeated page loads/polls
// re-pay that latency in full. Kept short because guests actively create and
// delete resources through the UI and expect that to show up promptly - this
// cache has no active invalidation hook (unlike wsCache/guestListCache), so
// TTL is the only thing bounding staleness here.
const resourcesCacheTTL = 3 * time.Second

// nonGuestResourcesCacheTTL applies to hand-maintained workspaces (everything
// not prefixed guest-). Those are committed directly to homelab-workspaces
// outside launchpad-api entirely, so there's no create/delete event to react
// to either way - a much longer TTL just means fewer repeat GitHub calls for
// data that in practice changes on the order of days, not seconds.
const nonGuestResourcesCacheTTL = 5 * time.Minute

type resourceCacheEntry struct {
	resources []resourceJSON
	at        time.Time
}

func (a *app) handleListResources(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validWorkspaceName.MatchString(name) {
		http.Error(w, "invalid workspace name", http.StatusBadRequest)
		return
	}

	ttl := nonGuestResourcesCacheTTL
	if strings.HasPrefix(name, guestPrefix) {
		ttl = resourcesCacheTTL
	}

	a.resourceCacheMu.Lock()
	if entry, ok := a.resourceCache[name]; ok && time.Since(entry.at) < ttl {
		a.resourceCacheMu.Unlock()
		writeJSON(w, entry.resources)
		return
	}
	a.resourceCacheMu.Unlock()

	entries, err := a.gh.listDir(r.Context(), name)
	if err != nil {
		slog.Error("list resources", "workspace", name, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	var files []ghEntry
	for _, e := range entries {
		if e.Type != "file" || !strings.HasSuffix(e.Name, ".yaml") || e.Name == "namespace.yaml" {
			continue
		}
		files = append(files, e)
	}

	// Fetch file contents in parallel - each is a separate GitHub API call,
	// and doing them serially made this endpoint take seconds per workspace.
	parsed := make([]*resourceJSON, len(files))
	var wg sync.WaitGroup
	for i, e := range files {
		wg.Add(1)
		go func(idx int, entry ghEntry) {
			defer wg.Done()
			content, err := a.gh.fileContent(r.Context(), entry.Path)
			if err != nil {
				slog.Warn("fetch file", "path", entry.Path, "err", err)
				return
			}

			var m xrManifest
			if err := yaml.Unmarshal(content, &m); err != nil {
				slog.Warn("parse yaml", "path", entry.Path, "err", err)
				return
			}

			if !strings.HasPrefix(m.APIVersion, "platform.local.lab") {
				return
			}

			ns, _ := m.Spec.Parameters["namespace"].(string)
			parsed[idx] = &resourceJSON{
				Name:      m.Metadata.Name,
				Kind:      m.Kind,
				Namespace: ns,
				Spec:      m.Spec.Parameters,
			}
		}(i, e)
	}
	wg.Wait()

	resources := make([]resourceJSON, 0, len(parsed))
	for _, r := range parsed {
		if r != nil {
			resources = append(resources, *r)
		}
	}

	a.resourceCacheMu.Lock()
	a.resourceCache[name] = resourceCacheEntry{resources: resources, at: time.Now()}
	a.resourceCacheMu.Unlock()

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

	a.triggerAppSetRefresh()
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
	if !validWorkspaceName.MatchString(name) {
		http.Error(w, "invalid workspace name", http.StatusBadRequest)
		return
	}

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

// triggerAppSetRefresh patches the xrs ApplicationSet with a refresh annotation
// so it immediately re-scans the Git repo for new workspace directories instead
// of waiting for the 2–3 min polling interval. Call this whenever a new workspace
// directory is created in GitHub so the Application gets created right away.
func (a *app) triggerAppSetRefresh() {
	if a.dynClient == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		appSetGVR := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applicationsets"}
		patch := []byte(`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"normal"}}}`)
		if _, err := a.dynClient.Resource(appSetGVR).Namespace("argocd").Patch(
			ctx, "xrs", types.MergePatchType, patch, metav1.PatchOptions{},
		); err != nil {
			slog.Debug("appset refresh trigger", "err", err)
		}
	}()
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

// fetchGuestMeta reads guest.yaml and returns expiry, phase timestamps, and doneAt.
// Returns zero-value struct on any error so the workspace is still listed without metadata.
func (a *app) fetchGuestMeta(ctx context.Context, workspace string) guestMeta {
	content, err := a.gh.fileContent(ctx, workspace+"/guest.yaml")
	if err != nil {
		return guestMeta{}
	}
	var raw struct {
		CreatedAt  string            `yaml:"createdAt"`
		PhaseTimes map[string]string `yaml:"phaseTimes,omitempty"`
		DoneAt     string            `yaml:"doneAt,omitempty"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		return guestMeta{}
	}
	t, err := time.Parse(time.RFC3339, raw.CreatedAt)
	if err != nil {
		return guestMeta{}
	}
	expiry := t.Add(guestTTL)
	var doneAt *string
	if raw.DoneAt != "" {
		doneAt = &raw.DoneAt
	}
	var phaseTimes map[string]string
	if len(raw.PhaseTimes) > 0 {
		phaseTimes = raw.PhaseTimes
	}
	return guestMeta{expiry: &expiry, phaseTimes: phaseTimes, doneAt: doneAt}
}

// fetchGuestExpiry is a thin wrapper kept for the cleanup loop.
func (a *app) fetchGuestExpiry(ctx context.Context, workspace string) *time.Time {
	return a.fetchGuestMeta(ctx, workspace).expiry
}
