# How It Works — launchpad-api

The Angular SPA (`launchpad`) submits a form. This binary turns that submission into a YAML file committed to GitHub. ArgoCD detects the commit, Crossplane reconciles the XR, pods start. The write path never touches the Kubernetes API directly — GitHub is the source of truth.

The read path goes the other direction. A Kubernetes dynamic client watches all eight platform XR kinds. Status events fan out over SSE to every connected browser. The SPA's status badges update in real time without polling.

See [launchpad/HOW_IT_WORKS.md](../launchpad/HOW_IT_WORKS.md) for the Angular side.

---

## Request Flow

```
Browser  →  POST /api/workspaces/{ws}/resources
              ↓
         auth.go          validate token, extract Contributor role
              ↓
         validate.go      required fields, noNewlines injection check
              ↓
         render.go        Go template → YAML string
              ↓
         github.go        fetch SHA → PUT to GitHub Contents API
              ↓
         homelab-workspaces repo  →  ArgoCD  →  Crossplane  →  Cluster
              ↓
         watcher.go       K8s watch event  →  broadcaster.go  →  SSE
              ↓
         Browser          status badge: QUEUED → PENDING → READY
```

---

## launchpad-api

A single Go binary with no web framework — stdlib `net/http` throughout. All application state lives on the `app` struct. Config is read once at startup with `mustEnv` / `envOrDefault`. No package-level globals.

### `main.go` — Bootstrap and routing

Wires everything together. Creates the `app` struct holding a `*githubClient`, a `*broadcaster`, and a `dynamic.Interface` Kubernetes client. The K8s client is optional — if the kubeconfig is unavailable, the app starts without it and logs a warning; the watch/schema endpoints return 503 instead of crashing.

Registers two mux layers. The inner `api` mux is wrapped by `auth.requireAuth` — every request through it goes through JWT validation. The outer `mux` registers `/healthz` and the guest endpoints (`/api/guest/...`) before the auth-wrapped catch-all, so guest creation never requires a token.

Starts three goroutines: the K8s watcher, the guest cleanup loop, and the HTTP server itself. Shutdown is coordinated with `signal.NotifyContext` — the server gets `Shutdown(ctx)` with a 10-second grace period.

**Key symbols**: `app struct { gh, bcast, dynClient }`, `mustEnv(key)`, `envOrDefault(key, default)`, `writeJSON(w, v)`

### `auth.go` — JWT middleware

Validates Azure Entra ID tokens on every non-guest request. At startup, fetches the JWKS from `login.microsoftonline.com/{tenantID}/discovery/v2.0/keys` using `MicahParks/keyfunc` — this runs once and refreshes automatically in the background.

Read requests (GET) pass through without a token. If a token is present on a GET, it is still validated so that the client's role context gets attached — that's how the UI knows whether to show write buttons. POST and DELETE are hard-gated: no token, no response.

The SSE endpoint (`/api/status/watch`) is a special case. `EventSource` in browsers cannot set an `Authorization` header, so authenticated clients pass `?token=` as a query parameter instead.

`requireContributor(w, r)` is called at the top of every write handler. It reads roles from request context (attached by the middleware) and returns `false` + writes 403 if the `Contributor` app role is absent.

`ENTRA_AUTH_DISABLED=true` skips all validation — but the middleware explicitly refuses to start if `PORT=443` is set, making the dev shortcut impossible to accidentally deploy.

**Key symbols**: `authMiddleware`, `newAuthMiddleware(ctx)`, `requireAuth(next)`, `requireContributor(w, r)`, `rolesKey contextKey`

### `github.go` — GitHub Contents API client

Wraps the GitHub Contents API into four operations: list a directory, read a file, upsert a file, delete a file. All calls include a `Bearer` token, `Accept: application/vnd.github+json`, and a fixed 10-second timeout.

The write operations are worth understanding. GitHub's API requires the current file's SHA to update or delete a file — without it, the PUT returns 409 conflict. `currentSHA(ctx, url)` does a GET first to fetch the SHA, then `upsertFile` includes it in the body. If the file doesn't exist yet (new resource), SHA is omitted and the API creates it. This is GitHub's atomic write protocol, not something invented here.

**Key symbols**: `githubClient`, `listDir`, `fileContent`, `upsertFile`, `deleteFile`, `currentSHA`, `setHeaders`

### `workspaces.go` — Listing and reading

Handles `GET /api/workspaces` and `GET /api/workspaces/{name}/resources`. Lists the root of the `homelab-workspaces` repo — every top-level directory is a workspace. Directories starting with `.` or matching `LAUNCHPAD_EXCLUDED_WORKSPACES` (comma-separated env var) are filtered out. The `launchpad` workspace itself is excluded this way so it doesn't show up in its own UI.

For each workspace starting with `guest-`, it reads `{workspace}/guest.yaml` to extract the `createdAt` timestamp, adds `guestTTL`, and populates `expiresAt` in the response. The Angular UI uses this to show the countdown timer.

`handleListResources` walks `{workspace}/` in GitHub, reads each YAML file, and unmarshals it into an `xrManifest` struct to extract kind, name, namespace, and spec parameters.

**Key symbols**: `workspaceJSON`, `resourceJSON`, `xrManifest`, `excludedWorkspaces()`, `fetchGuestExpiry()`

### `write.go` — Upsert and delete

Contains the implementation for both `upsertFile` and `deleteFile` on the `githubClient`. The actual write handlers are in `workspaces.go` and `guest.go` — this file is purely the mechanics of base64 encoding content, fetching the current SHA, building the GitHub API request body, and PUT/DELETE-ing it.

### `render.go` — YAML generation

Holds one Go `text/template` per resource kind. Eight kinds: `XSpa`, `XApi`, `XSql`, `XNoSql`, `XObjectStorage`, `XTopic`, `XSubscription`, `XWordpress`. Templates are compiled at init time with `template.Must`.

`RenderResource(req writeRequest)` looks up the template by `req.Kind`, executes it with the request as data, and returns the YAML string. The request's `Params` map is accessible inside the template as `.Params`.

`RenderNamespace(name)` produces the bare namespace manifest used when creating a workspace.

Adding a new platform resource kind means: add a template constant here, add the kind to `validate.go`'s switch, add a case in `guest.go`'s `buildGuestParams`, and add the XRD mapping in `schema.go`. Four places, no others.

**Key symbols**: `RenderResource(req writeRequest) (string, error)`, `RenderNamespace(name string) string`, `templates map[string]*template.Template`

### `validate.go` — Input sanitization

Defines the `writeRequest` struct (the POST body shape) and validates it before anything gets rendered or written.

Per-kind validation checks required fields. `XSpa` requires `image` and `host`. `XApi` requires `image`. `XTopic` requires `streamName` and `subjects`. `XSubscription` requires `topicRef`. `XSql`, `XNoSql`, `XObjectStorage` need only a name.

`noNewlines(params)` walks every string value in the params map and rejects anything containing `\n` or `\r`. This is the YAML injection guard — a newline in a template parameter would break out of its field and write arbitrary YAML. Short function; does one important job.

**Key symbols**: `writeRequest { Workspace, Kind, Name string; Params map[string]any }`, `validate(req)`, `noNewlines(params)`

### `schema.go` — Live XRD introspection

Serves `GET /api/schema/{kind}` by fetching the live XRD from the cluster and extracting the OpenAPI schema for that kind's `parameters` block.

The path drilled through the XRD object is: `spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.parameters`. This is the Crossplane XRD structure — the parameters object contains the fields that users actually fill in.

`handleGetResourceValues` reads an existing resource's YAML from GitHub and returns its current `spec.parameters` as JSON — this is how the edit form pre-populates with current values.

Because both endpoints depend on a live K8s client, they return 503 when running locally without a cluster.

**Key symbols**: `kindToXRD map[string]string`, `xrdGVR`, `handleGetSchema`, `extractParametersSchema(xrd)`, `handleGetResourceValues`

### `guest.go` — The sandbox

Guest workspaces are created without authentication. Five DNS slots (`demo1`–`demo5`) map to pre-created Cloudflare CNAME records pointing at the tunnel. A slot is assigned at workspace creation time, stored in `guest.yaml`, and returned to the client.

Names are generated by combining one word from each of two 25-word lists — 625 total combinations. The result looks like `guest-quantum-pickle`. If a name is already taken, the client gets a 409 and can retry. `guestTTL` is 10 minutes. `guestMaxCount` is 5 — hard-capped to match the number of DNS slots.

`startGuestCleanup` runs as a goroutine on a 1-minute ticker. It loads all guest workspaces, checks whether their age exceeds `guestTTL`, and deletes all files in the workspace directory from GitHub if so. ArgoCD detects the deletions and prunes the Application; Crossplane cascades down to the cluster resources.

Guest resources are limited to `allowedGuestKinds`: XApi, XSpa, XSql, XNoSql, XObjectStorage, XTopic. XWordpress is excluded because it's too heavy. XSubscription is excluded because it requires a topicRef pointing to an existing topic — too much state for a throwaway sandbox.

`buildGuestParams` auto-generates all resource parameters so guests don't fill out forms. For XSpa, it uses `GUEST_SPA_IMAGE` (default `ghcr.io/cujarrett/hello-world-spa:latest`). For XApi, it uses `GUEST_IMAGE` (default `ghcr.io/cujarrett/hello-world-api:latest`). The host is derived from the workspace slot: `demo3.mattjarrett.dev` for a spa, `demo3-api.mattjarrett.dev` for an api.

**Key symbols**: `guestTTL = 10 * time.Minute`, `guestMaxCount = 5`, `allowedGuestKinds`, `guestWords1`, `guestWords2`, `guestSlots`, `startGuestCleanup`, `buildGuestParams`

### `watcher.go` — The eyes on the cluster

Opens a Kubernetes watch stream for all eight platform XR kinds. Each kind gets its own goroutine via `watchResource`. When the watch returns (network drop, API server restart), the goroutine restarts it after a 5-second backoff.

Every `ADDED`, `MODIFIED`, and `DELETED` event gets processed by `extractStatus`. This reads the XR's `status.conditions` — specifically `Synced` and `Ready` conditions — and maps them to the `ResourceStatus` struct. The workspace is resolved from `spec.parameters.namespace` if present, otherwise `metadata.namespace`, falling back to an ArgoCD tracking annotation. This is belt-and-suspenders: it handles whatever the Crossplane composition decides to set.

`ResourceStatus` is published to the broadcaster. On `DELETED`, it publishes with `synced: false, ready: false`.

**Key symbols**: `ResourceStatus { Workspace, Kind, Name string; Synced, Ready bool; Message string }`, `watchedResources []xrResource`, `startWatcher`, `watchResource`, `extractStatus`

### `broadcaster.go` — The message bus

An in-memory pub-sub. Each SSE client subscribes and gets a buffered channel of size 64. On `subscribe()`, the broadcaster replays its entire cache to the new client immediately — this is why the status map appears populated instantly when you open the detail page, without waiting for a cluster event.

`publish(s)` updates the cache (keyed by `workspace/kind/name`) then sends to every subscriber. If a client's channel is full (slow reader), the event is dropped rather than blocking. No client can stall the bus.

`unsubscribe` closes the channel; the SSE handler detects channel close and cleans up.

**Key symbols**: `broadcaster { clients, cache }`, `subscribe() chan ResourceStatus`, `unsubscribe(ch)`, `publish(s ResourceStatus)`

### `Dockerfile` — Multi-stage arm64 build

Stage 1 (`builder`): `golang:1.24-alpine`, copies the whole repo, runs `go build -o launchpad-api`. Stage 2 (`runtime`): `gcr.io/distroless/static-debian12` — no shell, no package manager, no attack surface. Copies only the binary. Runs as non-root UID 65532. The final image is under 20MB.

### `justfile` — Dev automation

Five recipes: `ci` chains `lint → test → build`. `lint` runs `go mod tidy -diff` then `golangci-lint run`. `test` runs `go test -race ./...`. `build` produces the binary with `-o launchpad-api`. `run` does `go run .`.

---

## hello-world-api

A demo API deployed inside guest XApi workspaces — the thing that actually runs at `demo3-api.mattjarrett.dev`.

### `hello-world-api/main.go`

Two routes: `GET /healthz` (200 OK), and `GET /` which returns integration status JSON. The response shape is `{ message, workspace, timestamp, integrations[] }` where each integration is `{ name, status, detail }`.

Integration status is determined by probing at request time. `probeBinding(root, name, displayName)` checks whether `/bindings/{name}/host` exists (the service binding convention), reads the host and port from files in that directory, then dials a TCP connection with a 3-second timeout. Status is `not_configured` if the binding directory is absent, `unavailable` if the TCP dial fails, `ok` if it connects.

`probeNATS()` does the same thing via the `NATS_URL` environment variable instead of a binding directory.

The three probed integrations are PostgreSQL (`/bindings/sql/`), Redis (`/bindings/cache/`), and NATS (`NATS_URL`). This reflects the three integration types a guest can add to a sandbox.

`Access-Control-Allow-Origin: *` is set on `GET /` so the sibling SPA can fetch from it cross-origin.

**Key symbols**: `integrationStatus { Name, Status, Detail string }`, `probeBinding`, `probeNATS`, `checkIntegrations`

### `hello-world-api/Dockerfile`

Multi-stage Go build. Same pattern as the main API: builder stage compiles, runtime stage is distroless non-root.

---

## hello-world-spa

A static single-page demo app deployed inside guest XSpa workspaces — what shows at `demo3.mattjarrett.dev`.

### `hello-world-spa/index.html`

No framework, no build step — one HTML file served by nginx. On load, derives the sibling API URL from its own hostname by replacing `demo3.` with `demo3-api.` via a regex replace: `hostname.replace(/^(demo\d+)\./, '$1-api.')`.

Polls `GET /` on the API every 5 seconds with `fetch`. Renders one card per integration. Cards are colored by status: green border for `ok`, red for `unavailable`, grey/dimmed for `not_configured`. The workspace name badge comes from the `workspace` field in the API response.

It asks one question: "which integrations does this sandbox have?" and shows the answer in real time as you add or remove resources.

### `hello-world-spa/Dockerfile`

`FROM nginx:alpine`, copies `index.html` to `/usr/share/nginx/html/`. Three lines. Exposes port 80.

---

## What the API receives

The SPA POSTs a `writeRequest` JSON body:

```json
{ "workspace": "my-vinyl", "kind": "XApi", "name": "my-vinyl-api", "params": { "image": "ghcr.io/...", "replicas": 2 } }
```

The API validates it, renders YAML, and commits it to `homelab-workspaces/{workspace}/{name}.yaml`. Everything after that is ArgoCD and Crossplane.

For the full browser-side flow — auth, form generation, signal graph, SSE reconnect logic — see [launchpad/HOW_IT_WORKS.md](../launchpad/HOW_IT_WORKS.md).
