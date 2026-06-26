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
	"time"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	guestPrefix       = "guest-"
	guestTTL          = 10 * time.Minute
	guestMaxCount     = 5
	guestMaxResources = 10
)

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

// allowedGuestKinds is the set of resource kinds guests may create.
// XWordpress is excluded (too heavy / production data risk).
// XSubscription is excluded (requires a topicRef pointing to an existing topic).
var allowedGuestKinds = map[string]bool{
	"XApi":           true,
	"XSpa":           true,
	"XSql":           true,
	"XNoSql":         true,
	"XObjectStorage": true,
	"XTopic":         true,
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

	nsPath := fmt.Sprintf("%s/namespace.yaml", fullName)
	if err := a.gh.upsertFile(ctx, nsPath, "feat: create guest workspace "+fullName, RenderNamespace(fullName)); err != nil {
		slog.Error("create guest namespace", "workspace", fullName, "err", err)
		http.Error(w, "failed to create workspace", http.StatusInternalServerError)
		return
	}
	metaPath := fmt.Sprintf("%s/guest.yaml", fullName)
	if err := a.gh.upsertFile(ctx, metaPath, "feat: create guest meta "+fullName, metaYAML); err != nil {
		slog.Error("create guest meta", "workspace", fullName, "err", err)
		http.Error(w, "failed to create workspace metadata", http.StatusInternalServerError)
		return
	}

	a.copyDemoTLSSecrets(ctx, slot, fullName)
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

// handleCreateGuestResource creates a resource inside an existing guest workspace.
// Params are entirely auto-generated — guests supply only kind and name.
func (a *app) handleCreateGuestResource(w http.ResponseWriter, r *http.Request) {
	workspaceName := r.PathValue("name")
	if !strings.HasPrefix(workspaceName, guestPrefix) {
		http.Error(w, "not a guest workspace", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req struct {
		Kind      string `json:"kind"`
		WithCache bool   `json:"withCache"`
		WithSql   bool   `json:"withSql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if !allowedGuestKinds[req.Kind] {
		http.Error(w, fmt.Sprintf("kind %q is not available in guest workspaces", req.Kind), http.StatusUnprocessableEntity)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Verify the workspace exists and has not expired.
	workspaces, err := a.loadGuestWorkspaces(ctx)
	if err != nil {
		slog.Error("create guest resource: load workspaces", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	var live bool
	var wsSlot string
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
		live = true
		break
	}
	if !live {
		http.NotFound(w, r)
		return
	}

	// Enforce per-workspace resource cap.
	entries, err := a.gh.listDir(ctx, workspaceName)
	if err != nil {
		slog.Error("create guest resource: list files", "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	// Count only resource files (exclude namespace.yaml and meta.yaml).
	resourceCount := 0
	for _, e := range entries {
		if e.Name != "namespace.yaml" && e.Name != "guest.yaml" {
			resourceCount++
		}
	}
	if resourceCount >= guestMaxResources {
		http.Error(w, fmt.Sprintf("guest workspaces are limited to %d resources", guestMaxResources), http.StatusTooManyRequests)
		return
	}

	// Auto-generate all params — guests cannot inject arbitrary values.
	var guestImage string
	if req.Kind == "XSpa" {
		guestImage = envOrDefault("GUEST_SPA_IMAGE", "ghcr.io/cujarrett/hello-world-spa:latest")
	} else {
		guestImage = envOrDefault("GUEST_IMAGE", "ghcr.io/cujarrett/hello-world-api:latest")
	}
	resourceName, err := generateGuestResourceName(req.Kind, workspaceName, entries)
	if err != nil {
		slog.Error("generate resource name", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Collect existing file names so XApi can auto-wire nosqlRef/objectStorageRef.
	existingFileNames := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			existingFileNames = append(existingFileNames, e.Name)
		}
	}

	wr := writeRequest{
		Workspace: workspaceName,
		Kind:      req.Kind,
		Name:      resourceName,
		Params:    buildGuestParams(workspaceName, wsSlot, resourceName, req.Kind, guestImage, existingFileNames, req.WithCache, req.WithSql),
	}

	rendered, err := RenderResource(wr)
	if err != nil {
		slog.Error("render guest resource", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}

	path := fmt.Sprintf("%s/%s.yaml", workspaceName, resourceName)
	commitMsg := fmt.Sprintf("feat: add guest resource %s/%s", workspaceName, resourceName)
	if err := a.gh.upsertFile(ctx, path, commitMsg, rendered); err != nil {
		slog.Error("create guest resource", "workspace", workspaceName, "name", resourceName, "err", err)
		http.Error(w, "failed to create resource", http.StatusInternalServerError)
		return
	}

	slog.Info("created guest resource", "workspace", workspaceName, "kind", req.Kind, "name", resourceName)

	// When a non-XApi resource is added, re-render the XApi (if present) so
	// it picks up fresh nosqlRef/objectStorageRef wiring.
	// Pass the just-written filename so updateGuestApiRefs includes it even if
	// GitHub's listDir hasn't indexed the new commit yet (eventual consistency).
	if req.Kind != "XApi" {
		a.updateGuestApiRefs(ctx, workspaceName, wsSlot, resourceName+".yaml")
	}

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
			// Support both short names (new: sql, nosql, store) and old prefixed names (xsql-, xnosql-, xobjectstorage-).
			if base == "sql" || strings.HasPrefix(base, "sql-") || strings.HasPrefix(base, "xsql-") {
				p["sqlRef"] = base
			}
			if base == "nosql" || strings.HasPrefix(base, "nosql-") || strings.HasPrefix(base, "xnosql-") {
				p["nosqlRef"] = base
			}
			if base == "store" || strings.HasPrefix(base, "store-") || strings.HasPrefix(base, "xobjectstorage-") {
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

// updateGuestApiRefs re-renders the XApi in a guest workspace whenever a
// sibling resource is added, preserving the existing withCache/withSql state.
// It fetches a fresh file list from GitHub to handle concurrent creates.
// guaranteeFiles lists filenames that must be included even if GitHub's listDir
// hasn't indexed the freshly committed file yet (eventual consistency).
func (a *app) updateGuestApiRefs(ctx context.Context, workspace, slot string, guaranteeFiles ...string) {
	entries, err := a.gh.listDir(ctx, workspace)
	if err != nil {
		slog.Warn("update guest api refs: list dir", "workspace", workspace, "err", err)
		return
	}
	seen := make(map[string]bool, len(entries))
	fileNames := make([]string, 0, len(entries)+len(guaranteeFiles))
	var apiName string
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		seen[e.Name] = true
		fileNames = append(fileNames, e.Name)
		if (e.Name == "api.yaml" || strings.HasPrefix(e.Name, "api-") || strings.HasPrefix(e.Name, "xapi-")) && strings.HasSuffix(e.Name, ".yaml") {
			apiName = strings.TrimSuffix(e.Name, ".yaml")
		}
	}
	for _, f := range guaranteeFiles {
		if !seen[f] {
			fileNames = append(fileNames, f)
		}
	}
	if apiName == "" {
		return // no XApi in this workspace yet
	}

	// Preserve the existing withCache choice from the live file.
	var withCache bool
	if existing, err := a.gh.fileContent(ctx, workspace+"/"+apiName+".yaml"); err == nil {
		var m xrManifest
		if yaml.Unmarshal(existing, &m) == nil {
			withCache = m.Spec.Parameters["cache"] != nil
		}
	}

	guestImage := envOrDefault("GUEST_IMAGE", "ghcr.io/cujarrett/hello-world-api:latest")
	wr := writeRequest{
		Workspace: workspace,
		Kind:      "XApi",
		Name:      apiName,
		Params:    buildGuestParams(workspace, slot, apiName, "XApi", guestImage, fileNames, withCache, false),
	}
	rendered, err := RenderResource(wr)
	if err != nil {
		slog.Warn("update guest api refs: render", "workspace", workspace, "err", err)
		return
	}
	path := fmt.Sprintf("%s/%s.yaml", workspace, apiName)
	if err := a.gh.upsertFile(ctx, path, "feat: update guest api refs in "+workspace, rendered); err != nil {
		slog.Warn("update guest api refs: upsert", "workspace", workspace, "err", err)
	}
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
	if resourceName != "api" && !strings.HasPrefix(resourceName, "api-") && !strings.HasPrefix(resourceName, "xapi-") {
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

func (a *app) cleanupExpiredGuests(ctx context.Context) {
	workspaces, err := a.loadGuestWorkspaces(ctx)
	if err != nil {
		slog.Error("guest cleanup: load workspaces", "err", err)
		return
	}
	for _, ws := range workspaces {
		expiry, err := time.Parse(time.RFC3339, ws.ExpiresAt)
		if err != nil || time.Now().Before(expiry) {
			continue
		}
		slog.Info("guest cleanup: deleting expired workspace", "workspace", ws.Name)
		a.deleteGuestWorkspaceFiles(ctx, ws.Name)
	}
}

// deleteGuestWorkspaceFiles deletes every file in the guest workspace directory.
// ArgoCD and Crossplane cascade from there.
func (a *app) deleteGuestWorkspaceFiles(ctx context.Context, name string) {
	entries, err := a.gh.listDir(ctx, name)
	if err != nil {
		slog.Error("guest cleanup: list dir", "workspace", name, "err", err)
		return
	}
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if err := a.gh.deleteFile(ctx, e.Path, "chore: cleanup expired guest workspace "+name); err != nil {
			slog.Error("guest cleanup: delete file", "path", e.Path, "err", err)
		}
	}
	slog.Info("guest cleanup: workspace cleaned", "workspace", name)
}

// generateGuestResourceName returns a short, fixed name per kind (e.g. "api", "store").
// Resources are namespace-scoped, so the workspace slug does not need to repeat in the name.
// Shorter names keep IAM role names under AWS's 64-char limit regardless of workspace slug length.
// A 2-byte random hex suffix is appended only when a resource with that base name already exists.
func generateGuestResourceName(kind, workspaceName string, existing []ghEntry) (string, error) {
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
	base, ok := kindShortNames[kind]
	if !ok {
		base = strings.ToLower(kind)
	}

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


// copyDemoTLSSecrets copies the pre-provisioned demo slot TLS secrets from the
// demo-certs namespace into the guest workspace namespace so the Ingress can
// reference them directly without triggering cert-manager issuance.
func (a *app) copyDemoTLSSecrets(ctx context.Context, slot, targetNamespace string) {
	if a.dynClient == nil {
		slog.Warn("copyDemoTLSSecrets: no k8s client, skipping")
		return
	}
	secretGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	secretNames := []string{slot + "-api-tls", slot + "-tls"}
	for _, name := range secretNames {
		src, err := a.dynClient.Resource(secretGVR).Namespace("demo-certs").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			slog.Warn("copyDemoTLSSecrets: get source secret", "name", name, "err", err)
			continue
		}
		dst := src.DeepCopy()
		dst.SetNamespace(targetNamespace)
		dst.SetResourceVersion("")
		dst.SetUID("")
		dst.SetCreationTimestamp(metav1.Time{})
		dst.SetOwnerReferences(nil)
		_, err = a.dynClient.Resource(secretGVR).Namespace(targetNamespace).Create(ctx, dst, metav1.CreateOptions{})
		if err != nil {
			slog.Warn("copyDemoTLSSecrets: create secret in guest namespace", "name", name, "namespace", targetNamespace, "err", err)
		} else {
			slog.Info("copyDemoTLSSecrets: copied", "name", name, "namespace", targetNamespace)
		}
	}
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
