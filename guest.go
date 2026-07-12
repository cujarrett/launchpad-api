package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	guestPrefix       = "guest-"
	guestTTL          = 10 * time.Minute
	guestMaxCount     = 5
	guestMaxResources = 10
)

var (
	errWorkspaceNotFound = fmt.Errorf("workspace not found")
	errWorkspaceExpired  = fmt.Errorf("workspace expired")
)

// validateGuestWorkspace reads guest.yaml directly from the workspace path and
// returns the slot string. This avoids the root listDir call in loadGuestWorkspaces
// which can return a stale CDN-cached response for a brand-new directory.
func (a *app) validateGuestWorkspace(ctx context.Context, workspaceName string) (string, error) {
	metaContent, err := a.gh.fileContent(ctx, workspaceName+"/guest.yaml")
	if err != nil {
		return "", errWorkspaceNotFound
	}
	var meta struct {
		CreatedAt string `yaml:"createdAt"`
		Slot      string `yaml:"slot"`
	}
	if err := yaml.Unmarshal(metaContent, &meta); err != nil {
		return "", err
	}
	t, err := time.Parse(time.RFC3339, meta.CreatedAt)
	if err != nil || time.Now().After(t.Add(guestTTL)) {
		return "", errWorkspaceExpired
	}
	return meta.Slot, nil
}

// isValidWord checks if a word is a valid guest name component:
// lowercase alphanumeric, 2-8 chars, no special chars.
func isValidWord(w string) bool {
	if len(w) < 2 || len(w) > 12 {
		return false
	}
	for _, c := range w {
		if ('a' > c || c > 'z') && ('0' > c || c > '9') {
			return false
		}
	}
	return true
}

type guestWorkspaceJSON struct {
	Name      string `json:"name"`
	Slot      string `json:"slot"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
}

// handleListGuestWorkspaces returns all live guest workspaces with expiry info.
func (a *app) handleListGuestWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := a.loadGuestWorkspaces(r.Context())
	if err != nil {
		slog.Error("list guest workspaces", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	writeJSON(w, workspaces)
}

// guestSlots are the fixed DNS slots — each maps to a pre-configured public hostname.
// Slots are assigned at workspace creation time and stored in guest.yaml.
var guestSlots = []string{
	"demo1",
	"demo2",
	"demo3",
	"demo4",
	"demo5",
}

// handleCreateGuestWorkspace creates a guest-<name> workspace (no auth required).
// The client may suggest a name in the JSON body: {"name":"biscuit-factory"}.
// If the suggested name is valid and unused it is honoured; otherwise a random
// name is picked from the pool.
func (a *app) handleCreateGuestWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Parse optional name suggestion from body.
	var body struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1*1024)
	_ = json.NewDecoder(r.Body).Decode(&body) // ignore parse errors — name is optional

	existing, err := a.loadGuestWorkspaces(ctx)
	if err != nil {
		slog.Error("create guest workspace: check cap", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if len(existing) >= guestMaxCount {
		http.Error(w, fmt.Sprintf("all %d demo slots are in use — try again in a few minutes", guestMaxCount), http.StatusTooManyRequests)
		return
	}

	// Pick a fun name and an available DNS slot.
	usedNames := make(map[string]bool, len(existing))
	usedSlots := make(map[string]bool, len(existing))
	for _, ws := range existing {
		usedNames[ws.Name] = true
		if ws.Slot != "" {
			usedSlots[ws.Slot] = true
		}
	}

	// Honour the suggested name if it is valid and unused.
	var fullName string
	if suggested := strings.TrimSpace(body.Name); suggested != "" {
		// Validate format: two words separated by dash, lowercase alphanumeric
		w1, w2, ok := strings.Cut(suggested, "-")
		if ok && isValidWord(w1) && isValidWord(w2) {
			candidateFull := guestPrefix + suggested
			if usedNames[candidateFull] {
				// Name is valid but already taken — tell the client so it can reroll.
				http.Error(w, fmt.Sprintf("%q is already in use — try another name", suggested), http.StatusConflict)
				return
			}
			fullName = candidateFull
		}
	}
	if fullName == "" {
		// No valid suggestion provided — frontend must suggest a name
		http.Error(w, "invalid or missing workspace name", http.StatusBadRequest)
		return
	}

	slot, err := pickGuestSlot(usedSlots)
	if err != nil {
		slog.Error("create guest workspace: pick slot", "err", err)
		http.Error(w, fmt.Sprintf("all %d demo slots are in use — try again in a few minutes", guestMaxCount), http.StatusTooManyRequests)
		return
	}

	now := time.Now().UTC()
	metaYAML := fmt.Sprintf("createdAt: %q\nslot: %q\n", now.Format(time.RFC3339), slot)

	// namespace.yaml and guest.yaml are independent files with no shared tree
	// state, so write them concurrently instead of paying two round trips'
	// worth of GitHub API latency back to back.
	nsPath := fmt.Sprintf("%s/namespace.yaml", fullName)
	metaPath := fmt.Sprintf("%s/guest.yaml", fullName)
	var nsErr, metaErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		nsErr = a.gh.upsertFile(ctx, nsPath, "feat: create guest workspace "+fullName, RenderNamespace(fullName))
	}()
	go func() {
		defer wg.Done()
		metaErr = a.gh.upsertFile(ctx, metaPath, "feat: create guest meta "+fullName, metaYAML)
	}()
	wg.Wait()
	if nsErr != nil {
		slog.Error("create guest namespace", "workspace", fullName, "err", nsErr)
		http.Error(w, "failed to create workspace", http.StatusInternalServerError)
		return
	}
	if metaErr != nil {
		slog.Error("create guest meta", "workspace", fullName, "err", metaErr)
		http.Error(w, "failed to create workspace metadata", http.StatusInternalServerError)
		return
	}

	// The namespace itself doesn't exist yet — it was just committed to git and
	// still needs an ArgoCD sync before the cluster creates it. Retry in the
	// background instead of racing it on the request path.
	go a.copyDemoTLSSecretsWhenReady(context.Background(), slot, fullName)
	a.invalidateWorkspacesCache()
	slog.Info("created guest workspace", "name", fullName, "slot", slot)
	a.triggerAppSetRefresh()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, guestWorkspaceJSON{
		Name:      fullName,
		Slot:      slot,
		CreatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(guestTTL).Format(time.RFC3339),
	})
}

// handleCreateGuestResourceBatch creates an XApi or XSpa plus any requested
// add-ons (SQL, NoSQL, object storage, a paired SPA/API) in a single atomic commit.
func (a *app) handleCreateGuestResourceBatch(w http.ResponseWriter, r *http.Request) {
	workspaceName := r.PathValue("name")
	if !strings.HasPrefix(workspaceName, guestPrefix) {
		http.Error(w, "not a guest workspace", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req struct {
		Kind        string `json:"kind"`
		WithCache   bool   `json:"withCache"`
		WithSql     bool   `json:"withSql"`
		WithNoSql   bool   `json:"withNoSql"`
		WithStorage bool   `json:"withStorage"`
		WithSpa     bool   `json:"withSpa"`
		WithApi     bool   `json:"withApi"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Kind != "XApi" && req.Kind != "XSpa" {
		http.Error(w, fmt.Sprintf("kind %q is not available in guest workspaces", req.Kind), http.StatusUnprocessableEntity)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Validating the workspace and listing its files are independent reads —
	// run them concurrently instead of paying two round trips back to back.
	var wsSlot string
	var validateErr, listErr error
	var entries []ghEntry
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		wsSlot, validateErr = a.validateGuestWorkspace(ctx, workspaceName)
	}()
	go func() {
		defer wg.Done()
		entries, listErr = a.gh.listDir(ctx, workspaceName)
	}()
	wg.Wait()

	if validateErr == errWorkspaceNotFound {
		http.NotFound(w, r)
		return
	}
	if validateErr == errWorkspaceExpired {
		http.Error(w, "guest workspace has expired", http.StatusGone)
		return
	}
	if validateErr != nil {
		slog.Error("create guest resource batch: validate workspace", "err", validateErr)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if listErr != nil {
		slog.Error("create guest resource batch: list files", "err", listErr)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	resourceCount := 0
	for _, e := range entries {
		if e.Name != "namespace.yaml" && e.Name != "guest.yaml" {
			resourceCount++
		}
	}

	// Build the ordered plan of kinds to create, mirroring the guest-create UI.
	var plan []string
	switch req.Kind {
	case "XApi":
		if req.WithStorage {
			plan = append(plan, "XObjectStorage")
		}
		if req.WithSql {
			plan = append(plan, "XSql")
		}
		if req.WithNoSql {
			plan = append(plan, "XNoSql")
		}
		if req.WithSpa {
			plan = append(plan, "XSpa")
		}
		plan = append(plan, "XApi")
	case "XSpa":
		if req.WithApi {
			plan = append(plan, "XApi")
		}
		plan = append(plan, "XSpa")
	}

	if resourceCount+len(plan) > guestMaxResources {
		http.Error(w, fmt.Sprintf("guest workspaces are limited to %d resources", guestMaxResources), http.StatusTooManyRequests)
		return
	}

	// Generate a resource name for every planned kind up front, and collect the
	// full sibling file list (existing + about-to-be-created) so XApi's
	// sqlRef/nosqlRef/objectStorageRefs wiring is correct in a single pass —
	// no re-read from GitHub needed between resources.
	allFileNames := make([]string, 0, len(entries)+len(plan))
	for _, e := range entries {
		if e.Type == "file" {
			allFileNames = append(allFileNames, e.Name)
		}
	}
	names := make(map[string]string, len(plan)) // kind -> resourceName
	for _, kind := range plan {
		name, err := generateGuestResourceName(kind, workspaceName, entries)
		if err != nil {
			slog.Error("create guest resource batch: generate name", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		names[kind] = name
		allFileNames = append(allFileNames, name+".yaml")
	}

	files := make([]batchFile, 0, len(plan))
	createdNames := make([]string, 0, len(plan))
	for _, kind := range plan {
		var guestImage string
		if kind == "XSpa" {
			guestImage = envOrDefault("GUEST_SPA_IMAGE", "ghcr.io/cujarrett/hello-world-spa:latest")
		} else {
			guestImage = envOrDefault("GUEST_IMAGE", "ghcr.io/cujarrett/hello-world-api:latest")
		}
		resourceName := names[kind]
		wr := writeRequest{
			Workspace: workspaceName,
			Kind:      kind,
			Name:      resourceName,
			Params:    buildGuestParams(workspaceName, wsSlot, resourceName, kind, guestImage, allFileNames, req.WithCache, req.WithSql),
		}
		rendered, err := RenderResource(wr)
		if err != nil {
			slog.Error("render guest resource batch", "kind", kind, "err", err)
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		files = append(files, batchFile{
			Path:    fmt.Sprintf("%s/%s.yaml", workspaceName, resourceName),
			Content: rendered,
		})
		createdNames = append(createdNames, resourceName)
	}

	commitMsg := fmt.Sprintf("feat: create guest resources %s (%s)", workspaceName, strings.Join(createdNames, ", "))
	if err := a.gh.upsertFilesAtomic(ctx, files, commitMsg); err != nil {
		slog.Error("create guest resource batch", "workspace", workspaceName, "names", createdNames, "err", err)
		http.Error(w, "failed to create resources", http.StatusInternalServerError)
		return
	}

	slog.Info("created guest resources", "workspace", workspaceName, "kinds", plan, "names", createdNames)
	a.triggerArgoSync(workspaceName)
	w.WriteHeader(http.StatusCreated)
}

// buildGuestParams generates all YAML template params for a guest resource.
// All values are controlled by the server — guests supply only kind.
// existingFiles is the list of filenames already present in the workspace.
// withSql wires sqlRef (opt-in); nosqlRef and objectStorageRef are always
// auto-wired when sibling files exist. topicRef is not wired.
// slot is the fixed DNS slot (e.g. "demo4") — decoupled from the workspace name.
func buildGuestParams(workspace, slot, name, kind, image string, existingFiles []string, withCache, withSql bool) map[string]any {
	p := map[string]any{
		"namespace": workspace,
	}
	switch kind {
	case "XApi":
		p["image"] = image
		p["port"] = 8080
		if withCache {
			p["cache"] = true
		}
		p["host"] = fmt.Sprintf("%s-api.mattjarrett.dev", slot)
		p["tlsSecret"] = slot + "-api-tls"
		p["readinessCheckPath"] = "/readyz"
		for _, f := range existingFiles {
			base := strings.TrimSuffix(f, ".yaml")
			// Support "{slug}-{kind}" names (e.g. "phantom-burrito-sql"), legacy short names
			// ("sql"), and old prefixed names ("xsql-{slug}").
			if base == "sql" || strings.HasSuffix(base, "-sql") || strings.HasPrefix(base, "xsql-") {
				p["sqlRef"] = base
			}
			if base == "nosql" || strings.HasSuffix(base, "-nosql") || strings.HasPrefix(base, "xnosql-") {
				p["nosqlRef"] = base
			}
			if base == "store" || strings.HasSuffix(base, "-store") || strings.HasPrefix(base, "xobjectstorage-") {
				p["objectStorageRefs"] = base
			}
		}
	case "XSpa":
		p["image"] = image
		p["host"] = fmt.Sprintf("%s.mattjarrett.dev", slot)
		p["tlsSecret"] = slot + "-tls"
		// hello-world-spa uses inline <style>/<script> and fetches the companion API
		// on a different subdomain — relax CSP accordingly.
		p["contentSecurityPolicy"] = "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self' https://*.mattjarrett.dev; frame-ancestors 'none'; base-uri 'self';"
	case "XSql":
		p["backend"] = "private-cloud"
		p["dataRetention"] = "delete"
	case "XNoSql":
		p["dataRetention"] = "delete"
	case "XObjectStorage":
		p["dataRetention"] = "delete"
	case "XTopic":
		streamName := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		p["streamName"] = streamName
		p["subjects"] = []string{name + ".*"}
	}
	return p
}

// handlePatchGuestResource re-renders a guest XApi with updated connection params.
// Body: { withSql: bool, withCache: bool }. Returns 204 on success.
func (a *app) handlePatchGuestResource(w http.ResponseWriter, r *http.Request) {
	workspaceName := r.PathValue("name")
	resourceName := r.PathValue("resource")

	if !strings.HasPrefix(workspaceName, guestPrefix) {
		http.Error(w, "not a guest workspace", http.StatusForbidden)
		return
	}
	if resourceName != "api" && !strings.HasSuffix(resourceName, "-api") && !strings.HasPrefix(resourceName, "xapi-") {
		http.Error(w, "only XApi resources can be patched", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1*1024)
	var req struct {
		WithSql   bool `json:"withSql"`
		WithCache bool `json:"withCache"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Verify the workspace exists and hasn't expired.
	workspaces, err := a.loadGuestWorkspaces(ctx)
	if err != nil {
		slog.Error("patch guest resource: load workspaces", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	var wsSlot string
	var found bool
	for _, ws := range workspaces {
		if ws.Name != workspaceName {
			continue
		}
		expiry, parseErr := time.Parse(time.RFC3339, ws.ExpiresAt)
		if parseErr != nil || time.Now().After(expiry) {
			http.Error(w, "guest workspace has expired", http.StatusGone)
			return
		}
		wsSlot = ws.Slot
		found = true
		break
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	// Verify the resource file exists.
	resourcePath := fmt.Sprintf("%s/%s.yaml", workspaceName, resourceName)
	if _, err := a.gh.fileContent(ctx, resourcePath); err != nil {
		http.NotFound(w, r)
		return
	}

	// List all files in the workspace for auto-wiring nosqlRef/objectStorageRef.
	entries, err := a.gh.listDir(ctx, workspaceName)
	if err != nil {
		slog.Error("patch guest resource: list files", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	fileNames := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			fileNames = append(fileNames, e.Name)
		}
	}

	guestImage := envOrDefault("GUEST_IMAGE", "ghcr.io/cujarrett/hello-world-api:latest")
	wr := writeRequest{
		Workspace: workspaceName,
		Kind:      "XApi",
		Name:      resourceName,
		Params:    buildGuestParams(workspaceName, wsSlot, resourceName, "XApi", guestImage, fileNames, req.WithCache, req.WithSql),
	}

	rendered, err := RenderResource(wr)
	if err != nil {
		slog.Error("patch guest resource: render", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}

	commitMsg := fmt.Sprintf("feat: update guest resource refs %s/%s", workspaceName, resourceName)
	if err := a.gh.upsertFile(ctx, resourcePath, commitMsg, rendered); err != nil {
		slog.Error("patch guest resource: upsert", "workspace", workspaceName, "name", resourceName, "err", err)
		http.Error(w, "failed to update resource", http.StatusInternalServerError)
		return
	}

	slog.Info("patched guest resource", "workspace", workspaceName, "name", resourceName, "withSql", req.WithSql, "withCache", req.WithCache)
	a.triggerArgoSync(workspaceName)
	w.WriteHeader(http.StatusNoContent)
}

// handleRecordGuestPhase records a phase timestamp in guest.yaml.
// Called by the SPA as each provisioning phase transitions, so timings persist
// across browsers and users. Body: { "phase": "1" }. No auth required.
func (a *app) handleRecordGuestPhase(w http.ResponseWriter, r *http.Request) {
	workspaceName := r.PathValue("name")
	if !strings.HasPrefix(workspaceName, guestPrefix) {
		http.Error(w, "not a guest workspace", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 256)
	var req struct {
		Phase string `json:"phase"`
		Done  bool   `json:"done"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// Validate phase is a single digit 0-4 or empty (done path uses Done:true instead).
	// Prevents injection into YAML keys and the git commit message.
	validPhases := map[string]bool{"0": true, "1": true, "2": true, "3": true, "4": true, "": true}
	if !validPhases[req.Phase] {
		http.Error(w, "invalid phase", http.StatusBadRequest)
		return
	}

	// Phase times are best-effort telemetry (elapsed-time-per-stage display),
	// not something the UI needs to block on, so persist in the background.
	// A per-workspace lock keeps concurrent phase writes from racing.
	go a.persistGuestPhase(context.Background(), workspaceName, req.Phase, req.Done)

	w.WriteHeader(http.StatusNoContent)
}

func (a *app) persistGuestPhase(ctx context.Context, workspaceName, phase string, done bool) {
	lk := a.lockGuestMeta(workspaceName)
	lk.Lock()
	defer lk.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	metaPath := workspaceName + "/guest.yaml"
	content, err := a.gh.fileContent(ctx, metaPath)
	if err != nil {
		slog.Warn("record guest phase: workspace not found", "workspace", workspaceName, "err", err)
		return
	}

	var raw struct {
		CreatedAt  string            `yaml:"createdAt"`
		Slot       string            `yaml:"slot"`
		PhaseTimes map[string]string `yaml:"phaseTimes,omitempty"`
		DoneAt     string            `yaml:"doneAt,omitempty"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		slog.Warn("record guest phase: parse metadata", "workspace", workspaceName, "err", err)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if done {
		if raw.DoneAt == "" {
			raw.DoneAt = now
		}
	} else if phase != "" {
		if raw.PhaseTimes == nil {
			raw.PhaseTimes = make(map[string]string)
		}
		if _, exists := raw.PhaseTimes[phase]; !exists {
			raw.PhaseTimes[phase] = now
		}
	}

	updated, err := yaml.Marshal(raw)
	if err != nil {
		slog.Warn("record guest phase: serialize metadata", "workspace", workspaceName, "err", err)
		return
	}
	if err := a.gh.upsertFile(ctx, metaPath, "chore: record phase "+phase+" for "+workspaceName, string(updated)); err != nil {
		slog.Warn("record guest phase: upsert", "workspace", workspaceName, "phase", phase, "err", err)
	}
}

// loadGuestWorkspaces lists all live guest-* workspaces with their expiry times,
// sorted oldest-first (for the "hogging the sandbox" cap message).
func (a *app) loadGuestWorkspaces(ctx context.Context) ([]guestWorkspaceJSON, error) {
	entries, err := a.gh.listDir(ctx, "")
	if err != nil {
		return nil, err
	}

	var result []guestWorkspaceJSON
	for _, e := range entries {
		if e.Type != "dir" || !strings.HasPrefix(e.Name, guestPrefix) {
			continue
		}

		metaContent, err := a.gh.fileContent(ctx, e.Name+"/guest.yaml")
		if err != nil {
			slog.Warn("load guest meta", "workspace", e.Name, "err", err)
			continue
		}

		var meta struct {
			CreatedAt string `yaml:"createdAt"`
			Slot      string `yaml:"slot"`
		}
		if err := yaml.Unmarshal(metaContent, &meta); err != nil {
			slog.Warn("parse guest meta", "workspace", e.Name, "err", err)
			continue
		}

		t, err := time.Parse(time.RFC3339, meta.CreatedAt)
		if err != nil {
			slog.Warn("invalid guest createdAt", "workspace", e.Name, "err", err)
			continue
		}

		result = append(result, guestWorkspaceJSON{
			Name:      e.Name,
			Slot:      meta.Slot,
			CreatedAt: meta.CreatedAt,
			ExpiresAt: t.Add(guestTTL).Format(time.RFC3339),
		})
	}

	// Oldest first — used for the cap-exceeded message.
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt < result[j].CreatedAt
	})
	return result, nil
}

// startGuestCleanup runs a background loop that deletes expired guest workspaces.
// It runs immediately on startup to catch workspaces that expired while the API
// was offline, then ticks every minute.
func (a *app) startGuestCleanup(ctx context.Context) {
	a.cleanupExpiredGuests(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupExpiredGuests(ctx)
		}
	}
}

// staleGuestSweepThreshold is how many consecutive cleanup ticks (~1/min) a
// workspace can have a missing/unreadable guest.yaml before it's swept as a
// leftover from a previously-interrupted delete, rather than skipped forever.
// A workspace genuinely mid-creation resolves within seconds, not minutes.
const staleGuestSweepThreshold = 3

func (a *app) recordStaleGuestMiss(name string) int {
	a.staleGuestMu.Lock()
	defer a.staleGuestMu.Unlock()
	if a.staleGuestMisses == nil {
		a.staleGuestMisses = map[string]int{}
	}
	a.staleGuestMisses[name]++
	return a.staleGuestMisses[name]
}

func (a *app) clearStaleGuestMisses(name string) {
	a.staleGuestMu.Lock()
	defer a.staleGuestMu.Unlock()
	delete(a.staleGuestMisses, name)
}

func (a *app) cleanupExpiredGuests(ctx context.Context) {
	entries, err := a.gh.listDir(ctx, "")
	if err != nil {
		slog.Error("guest cleanup: list root", "err", err)
		return
	}
	for _, e := range entries {
		if e.Type != "dir" || !strings.HasPrefix(e.Name, guestPrefix) {
			continue
		}
		expiry := a.fetchGuestExpiry(ctx, e.Name)
		if expiry == nil {
			// guest.yaml missing or unreadable. Usually a workspace still mid-creation,
			// but if this persists across several ticks it's a leftover directory from
			// a delete that failed partway through — sweep whatever files remain.
			if misses := a.recordStaleGuestMiss(e.Name); misses >= staleGuestSweepThreshold {
				slog.Warn("guest cleanup: guest.yaml missing after repeated checks, sweeping leftover files", "workspace", e.Name, "misses", misses)
				a.deleteGuestWorkspaceFiles(ctx, e.Name)
				a.clearStaleGuestMisses(e.Name)
				continue
			}
			slog.Warn("guest cleanup: could not read expiry, skipping", "workspace", e.Name)
			continue
		}
		a.clearStaleGuestMisses(e.Name)
		if time.Now().After(*expiry) {
			slog.Info("guest cleanup: deleting expired workspace", "workspace", e.Name)
			a.deleteGuestWorkspaceFiles(ctx, e.Name)
		}
	}
}

// deleteGuestWorkspaceFiles deletes every file in the guest workspace directory.
// ArgoCD and Crossplane cascade from there. Each delete is retried a few times —
// GitHub's contents API can briefly 404 the SHA lookup for a file that was just
// listed, a read-after-write consistency hiccup rather than a real absence.
func (a *app) deleteGuestWorkspaceFiles(ctx context.Context, name string) {
	defer a.invalidateWorkspacesCache()
	defer a.forgetGuestMeta(name)

	entries, err := a.gh.listDir(ctx, name)
	if err != nil {
		slog.Error("guest cleanup: list dir", "workspace", name, "err", err)
		return
	}
	ok := true
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Second)
			}
			if err = a.gh.deleteFile(ctx, e.Path, "chore: cleanup expired guest workspace "+name); err == nil {
				break
			}
		}
		if err != nil {
			slog.Error("guest cleanup: delete file", "path", e.Path, "err", err)
			ok = false
		}
	}
	if !ok {
		slog.Error("guest cleanup: workspace not fully cleaned, will retry next tick", "workspace", name)
		return
	}
	slog.Info("guest cleanup: workspace cleaned", "workspace", name)
}

// generateGuestResourceName returns a name that is unique across the cluster.
// XRs (XApi, XSpa, XSql, etc.) are cluster-scoped in Crossplane, so names must not
// collide between workspaces. We use "{slug}-{kind-short}" (e.g. "phantom-burrito-api").
// IAM role names that exceed 64 chars are handled by the hash fallback in the XApi
// composition (xp-{sha256}), so slug length is not a concern here.
// A 2-byte random hex suffix is appended only when a resource with that base name already exists.
func generateGuestResourceName(kind, workspaceName string, existing []ghEntry) (string, error) {
	slug := strings.TrimPrefix(workspaceName, guestPrefix)
	kindShortNames := map[string]string{
		"XApi":           "api",
		"XSpa":           "spa",
		"XSql":           "sql",
		"XNoSql":         "nosql",
		"XObjectStorage": "store",
		"XTopic":         "topic",
		"XSubscription":  "sub",
		"XWordpress":     "wordpress",
	}
	suffix, ok := kindShortNames[kind]
	if !ok {
		suffix = strings.ToLower(kind)
	}
	base := slug + "-" + suffix // e.g. "phantom-burrito-api"

	// Check if a file with this base name is already present.
	for _, e := range existing {
		if e.Name == base+".yaml" {
			// Collision — append a 2-byte random hex suffix.
			b := make([]byte, 2)
			if _, err := rand.Read(b); err != nil {
				return "", err
			}
			return fmt.Sprintf("%s-%s", base, hex.EncodeToString(b)), nil
		}
	}
	return base, nil
}

// copyDemoTLSSecretsWhenReady retries copyDemoTLSSecrets until it fully
// succeeds or the bound elapses. The guest namespace doesn't exist yet when
// this is first called — it was just committed to git and still needs an
// ArgoCD sync — so early attempts are expected to fail with "not found"
// until the namespace shows up. launchpad-api has no RBAC grant to get
// namespaces directly, so readiness is inferred from the copy itself
// succeeding rather than a separate existence check.
func (a *app) copyDemoTLSSecretsWhenReady(ctx context.Context, slot, targetNamespace string) {
	if a.dynClient == nil {
		slog.Warn("copyDemoTLSSecrets: no k8s client, skipping")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		if a.copyDemoTLSSecrets(ctx, slot, targetNamespace) {
			return
		}
		select {
		case <-ctx.Done():
			slog.Warn("copyDemoTLSSecrets: gave up", "namespace", targetNamespace)
			return
		case <-ticker.C:
		}
	}
}

// copyDemoTLSSecrets copies the pre-provisioned demo slot TLS secrets from the
// demo-certs namespace into the guest workspace namespace so the Ingress can
// reference them directly without triggering cert-manager issuance. Returns
// true only if both secrets were copied successfully.
func (a *app) copyDemoTLSSecrets(ctx context.Context, slot, targetNamespace string) bool {
	if a.dynClient == nil {
		slog.Warn("copyDemoTLSSecrets: no k8s client, skipping")
		return false
	}
	secretGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	secretNames := []string{slot + "-api-tls", slot + "-tls"}
	ok := true
	for _, name := range secretNames {
		src, err := a.dynClient.Resource(secretGVR).Namespace("demo-certs").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			slog.Warn("copyDemoTLSSecrets: get source secret", "name", name, "err", err)
			ok = false
			continue
		}
		dst := src.DeepCopy()
		dst.SetNamespace(targetNamespace)
		dst.SetResourceVersion("")
		dst.SetUID("")
		dst.SetCreationTimestamp(metav1.Time{})
		dst.SetOwnerReferences(nil)
		_, err = a.dynClient.Resource(secretGVR).Namespace(targetNamespace).Create(ctx, dst, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			slog.Warn("copyDemoTLSSecrets: create secret in guest namespace", "name", name, "namespace", targetNamespace, "err", err)
			ok = false
		} else {
			slog.Info("copyDemoTLSSecrets: copied", "name", name, "namespace", targetNamespace)
		}
	}
	return ok
}

// pickGuestSlot returns a random available slot from guestSlots.
// Slots are independent of workspace names — they own the fixed DNS hostnames.
func pickGuestSlot(used map[string]bool) (string, error) {
	candidates := make([]string, 0, len(guestSlots))
	for _, s := range guestSlots {
		if !used[s] {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("all guest slots in use")
	}
	b := make([]byte, 1)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return candidates[int(b[0])%len(candidates)], nil
}

// handleMetrics serves a Prometheus text-format metrics endpoint.
// It exposes two gauges: active guest workspace count and total resource count.
func (a *app) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	workspaces, err := a.loadGuestWorkspaces(ctx)
	if err != nil {
		slog.Error("metrics: load guest workspaces", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	// Count only non-expired workspaces.
	now := time.Now()
	var liveWorkspaces []guestWorkspaceJSON
	for _, ws := range workspaces {
		expiry, parseErr := time.Parse(time.RFC3339, ws.ExpiresAt)
		if parseErr != nil || now.After(expiry) {
			continue
		}
		liveWorkspaces = append(liveWorkspaces, ws)
	}

	totalResources := 0
	for _, ws := range liveWorkspaces {
		entries, listErr := a.gh.listDir(ctx, ws.Name)
		if listErr != nil {
			slog.Warn("metrics: list workspace", "workspace", ws.Name, "err", listErr)
			continue
		}
		for _, e := range entries {
			if e.Name != "namespace.yaml" && e.Name != "guest.yaml" {
				totalResources++
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP launchpad_guest_workspace_count Number of active guest workspaces.\n")
	_, _ = fmt.Fprintf(w, "# TYPE launchpad_guest_workspace_count gauge\n")
	_, _ = fmt.Fprintf(w, "launchpad_guest_workspace_count %d\n", len(liveWorkspaces))
	_, _ = fmt.Fprintf(w, "# HELP launchpad_guest_resource_count Total resources across all active guest workspaces.\n")
	_, _ = fmt.Fprintf(w, "# TYPE launchpad_guest_resource_count gauge\n")
	_, _ = fmt.Fprintf(w, "launchpad_guest_resource_count %d\n", totalResources)
}
