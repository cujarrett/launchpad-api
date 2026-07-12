# How It Works — launchpad-api

The Angular SPA (`launchpad`) submits a form. This binary turns that submission into a YAML file committed to GitHub. ArgoCD detects the commit, Crossplane reconciles the XR, pods start. The write path ends when the cluster has something to reconcile.

The read path goes the other direction. A Kubernetes dynamic client watches all eight platform XR kinds. Status events fan out over SSE to every connected browser. The SPA's status badges update in real time.

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

A single Go binary with no web framework — stdlib `net/http` throughout. All application state lives on the `app` struct. Config is read once at startup with `mustEnv` / `envOrDefault`. No package-level globals except static templates and constant tables.

### `main.go` — Bootstrap and routing

Wires everything together. Creates the `app` struct holding a `*githubClient`, a `*broadcaster`, and a `dynamic.Interface` Kubernetes client. The K8s client is optional — if the kubeconfig is unavailable at startup, schema and watch features degrade gracefully.

Registers two mux layers. The inner `api` mux is wrapped by `auth.requireAuth` — every request through it goes through JWT validation. The outer `mux` registers `/healthz` and the guest endpoints (`/api/guest/*`) before applying the auth wrapper to the rest.

Starts three goroutines: the K8s watcher, the guest cleanup loop, and the HTTP server itself. Shutdown is coordinated with `signal.NotifyContext` — the server gets `Shutdown(ctx)` with a 5-second grace period.

**Key symbols**: `app struct { gh, bcast, dynClient }`, `mustEnv(key)`, `envOrDefault(key, default)`, `writeJSON(w, v)`

### `auth.go` — JWT middleware

Validates Azure Entra ID tokens on every non-guest request. At startup, fetches the JWKS from `login.microsoftonline.com/{tenantID}/discovery/v2.0/keys` using `MicahParks/keyfunc` — this runs once and then refreshes in the background.

Read requests (GET) pass through without a token. If a token is present on a GET, it is still validated so that the client's role context gets attached — that's how the UI knows whether to show write controls.

The SSE endpoint (`/api/status/watch`) is a special case. `EventSource` in browsers cannot set an `Authorization` header, so authenticated clients pass `?token=` as a query parameter instead.

`requireContributor(w, r)` is called at the top of every write handler. It reads roles from request context (attached by the middleware) and returns `false` + writes 403 if the `Contributor` app role is missing.

`ENTRA_AUTH_DISABLED=true` skips all validation — but the middleware explicitly refuses to start if `PORT=443` is set, making the dev shortcut impossible to accidentally deploy.

**Key symbols**: `authMiddleware`, `newAuthMiddleware(ctx)`, `requireAuth(next)`, `requireContributor(w, r)`, `rolesKey contextKey`

### `github.go` — GitHub Contents API client

Wraps the GitHub Contents API into four operations: list a directory, read a file, upsert a file, delete a file. All calls include a `Bearer` token, `Accept: application/vnd.github+json`, and a fixed `X-GitHub-Api-Version` header.

The write operations are worth understanding. GitHub's API requires the current file's SHA to update or delete a file — without it, the PUT returns 409 conflict. `currentSHA(ctx, url)` does a GET first and extracts `sha` from the response if the file exists.

**Key symbols**: `githubClient`, `listDir`, `fileContent`, `upsertFile`, `deleteFile`, `currentSHA`, `setHeaders`

### `workspaces.go` — Listing and reading

Handles `GET /api/workspaces` and `GET /api/workspaces/{name}/resources`. Lists the root of the `homelab-workspaces` repo and returns top-level workspace directories, excluding hidden directories and any internal-only entries filtered by current server logic.

For each workspace starting with `guest-`, it reads `{workspace}/guest.yaml` to extract the `createdAt` timestamp, adds `guestTTL`, and populates `expiresAt` in the response. The Angular UI uses this to show the countdown badge.

`handleListResources` walks `{workspace}/` in GitHub, reads each YAML file, and unmarshals it into an `xrManifest` struct to extract kind, name, namespace, and spec parameters.

**Key symbols**: `workspaceJSON`, `resourceJSON`, `xrManifest`, `excludedWorkspaces()`, `fetchGuestExpiry()`

### `write.go` — Upsert and delete

Contains the implementation for both `upsertFile` and `deleteFile` on the `githubClient`. The actual write handlers are in `workspaces.go` and `guest.go` — this file is purely the mechanics of base64 payloads, SHA handling, and commit messages.

### `render.go` — YAML generation

Holds one Go `text/template` per resource kind. Eight kinds: `XSpa`, `XApi`, `XSql`, `XNoSql`, `XObjectStorage`, `XTopic`, `XSubscription`, `XWordpress`. Templates are compiled at init time with `template.Must`.

`RenderResource(req writeRequest)` looks up the template by `req.Kind`, executes it with the request as data, and returns the YAML string. The request's `Params` map is accessible inside the template by key.

`RenderNamespace(name)` produces the bare namespace manifest used when creating a workspace.

Adding a new platform resource kind means: add a template constant here, add the kind to `validate.go`'s switch, add a case in `guest.go`'s `buildGuestParams`, and add the XRD mapping in `schema.go`. There's no single registry file — the kind is threaded through four places.

**Key symbols**: `RenderResource(req writeRequest) (string, error)`, `RenderNamespace(name string) string`, `templates map[string]*template.Template`

### `validate.go` — Input sanitization

Defines the `writeRequest` struct (the POST body shape) and validates it before anything gets rendered or written.

Per-kind validation checks required fields. `XSpa` requires `image` and `host`. `XApi` requires `image`. `XTopic` requires `streamName` and `subjects`. `XSubscription` requires `topicRef`. `XSql`, `XNoSql`, and `XObjectStorage` currently have no required params beyond the common fields.

`noNewlines(params)` walks every string value in the params map and rejects anything containing `\n` or `\r`. This is the YAML injection guard — a newline in a template parameter would break out of the intended scalar position.

**Key symbols**: `writeRequest { Workspace, Kind, Name string; Params map[string]any }`, `validate(req)`, `noNewlines(params)`

### `schema.go` — Live XRD introspection

Serves `GET /api/schema/{kind}` by fetching the live XRD from the cluster and extracting the OpenAPI schema for that kind's `parameters` block.

The path drilled through the XRD object is: `spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.parameters`. This is the Crossplane XRD structure — the parameters object contains the form schema the SPA needs.

`handleGetResourceValues` reads an existing resource's YAML from GitHub and returns its current `spec.parameters` as JSON — this is how the edit form pre-populates with current values.

Because both endpoints depend on a live K8s client, they return 503 when running locally without a cluster.

**Key symbols**: `kindToXRD map[string]string`, `xrdGVR`, `handleGetSchema`, `extractParametersSchema(xrd)`, `handleGetResourceValues`

### `guest.go` — The sandbox

Guest workspaces are created without authentication. Five DNS slots (`demo1`–`demo5`) map to pre-created Cloudflare CNAME records pointing at the tunnel. A slot is assigned at workspace creation time and released when the workspace expires.

Names are generated by combining one word from each of two 25-word lists — 625 total combinations. The result looks like `guest-quantum-pickle`. If a name is already taken, the client gets a 409 and retries with a different pair.

`startGuestCleanup` runs as a goroutine on a 1-minute ticker (and once immediately at startup). It loads all guest workspaces, checks whether their age exceeds `guestTTL`, and deletes all files in the workspace plus the workspace directory marker.

Guest resources are limited to `allowedGuestKinds`: XApi, XSpa, XSql, XNoSql, XObjectStorage, XTopic. XWordpress is excluded because it's too heavy. XSubscription is excluded because it requires a topic reference and would complicate the zero-input sandbox UX.

Guest workspaces are also capped at `guestMaxResources = 10` resources each.

`buildGuestParams` auto-generates all resource parameters so guests don't fill out forms. For XSpa, it uses `GUEST_SPA_IMAGE` (default `ghcr.io/cujarrett/hello-world-spa:latest`). For XApi, it uses `GUEST_API_IMAGE` (default `ghcr.io/cujarrett/hello-world-api:latest`) and derives the public host from the assigned slot.

**Key symbols**: `guestTTL = 10 * time.Minute`, `guestMaxCount = 5`, `allowedGuestKinds`, `guestWords1`, `guestWords2`, `guestSlots`, `startGuestCleanup`, `buildGuestParams`

### `watcher.go` — The eyes on the cluster

Opens a Kubernetes watch stream for all eight platform XR kinds. Each kind gets its own goroutine via `watchResource`. When the watch returns (network drop, API server restart), the goroutine restarts the watch after a short delay.

Every `ADDED`, `MODIFIED`, and `DELETED` event gets processed by `extractStatus`. This reads the XR's `status.conditions` — specifically `Synced` and `Ready` conditions — and maps them to the `ResourceStatus` payload sent to browsers.

`ResourceStatus` is published to the broadcaster.

**Key symbols**: `ResourceStatus { Workspace, Kind, Name string; Synced, Ready bool; Message string }`, `watchedResources []xrResource`, `startWatcher`, `watchResource`, `extractStatus`

### `broadcaster.go` — The message bus

An in-memory pub-sub. Each SSE client subscribes and gets a buffered channel of size 64. On `subscribe()`, the broadcaster replays its entire cache to the new client immediately — this is why the status panel can populate without waiting for the next cluster event.

`publish(s)` updates the cache (keyed by `workspace/kind/name`) then sends to every subscriber. If a client's channel is full (slow reader), the event is dropped rather than blocking. No client can stall the whole system.

`unsubscribe` closes the channel; the SSE handler detects channel close and cleans up.

**Key symbols**: `broadcaster { clients, cache }`, `subscribe() chan ResourceStatus`, `unsubscribe(ch)`, `publish(s ResourceStatus)`

### `Dockerfile` — Multi-stage arm64 build

Stage 1 (`builder`): `golang:1.24-alpine`, copies the whole repo, runs `go build -o launchpad-api`. Stage 2 (`runtime`): `gcr.io/distroless/static-debian12` — no shell, no package manager, no attack surface beyond the binary and CA certs.

### `justfile` — Dev automation

Five recipes: `ci` chains `lint → test → build`. `lint` runs `go mod tidy -diff` then `golangci-lint run`. `test` runs `go test -race ./...`. `build` produces the binary with `-o launchpad-api`. `run` executes it locally.

---

## hello-world-api

A demo API deployed inside guest XApi workspaces — the thing that actually runs at `demo3-api.mattjarrett.dev`.

### `hello-world-api/main.go`

Three routes: `GET /healthz` (200 OK), `GET /readyz` (readiness probe — 503 while any wired integration is still `starting`), and `GET /` which returns integration status JSON. The response shape is `{ integrations: [...] }`.

Integration status is determined by probing at request time. `readBinding(root, name)` loads the servicebinding.io files under `/bindings/{name}/`; a missing directory reports `not_configured` (the SPA hides the card). Every probe performs a real round-trip, not a presence check — statuses are `ok`, `starting` (binding present, backend or credentials not ready yet), `error`, or `not_configured`.

The six probes:

- **SQL Database** (`/bindings/sql/`) — private-cloud bindings (a `password` key) do a create/insert/count round-trip on Postgres. Public-cloud bindings (a `role-arn` key, RDS IAM auth) verify STS credentials via the `sql` profile, generate an RDS auth token, and attempt a short connect — the endpoint is VPC-internal, so a timeout still reports `ok` with an identity-verified detail.
- **Cache** (`/bindings/cache/`) — private-cloud does an INCR round-trip on Redis. Public-cloud (ElastiCache, always VPC-internal) verifies STS credentials via the `cache` profile and attempts a short TLS connect.
- **NoSQL Database** (`/bindings/nosql/`) — PutItem → GetItem → DeleteItem on the DynamoDB table using the `nosql` credentials profile.
- **Object Storage** (`/bindings/object-storage/`, `/bindings/object-storage-1/`, …) — PutObject → GetObject → DeleteObject in every bound bucket, one credentials profile per mount.
- **Topic** (gated on `NATS_STREAM` env) — publishes a message to the bound JetStream stream.
- **Subscription** (gated on `NATS_CONSUMER` env) — publishes a uniquely-tagged message, then pull-fetches through the durable consumer until it arrives: a full stream → cursor → delivery round-trip.

AWS probes use the shared credentials file written by the `aws-credentials-sidecar` (`AWS_SHARED_CREDENTIALS_FILE`); the profile name always equals the binding directory name, so no `AWS_PROFILE_*` env-var lookup is needed. A binding whose profile section hasn't been written yet reports `starting`.

`Access-Control-Allow-Origin: *` is set on `GET /` so the sibling SPA can fetch from it cross-origin.

**Key symbols**: `integrationStatus { Name, Status, Detail string }`, `readBinding`, `hasProfile`, `checkIntegrations`

### `hello-world-api/Dockerfile`

Multi-stage Go build. Same pattern as the main API: builder stage compiles, runtime stage is distroless non-root.

---

## hello-world-spa

A static single-page demo app deployed inside guest XSpa workspaces — what shows at `demo3.mattjarrett.dev`.

### `hello-world-spa/index.html`

No framework, no build step — one HTML file served by nginx. On load, derives the sibling API URL from its own hostname by replacing `demo3.` with `demo3-api.` via a regex replace: `hostname.replace(...)`.

Polls `GET /` on the API every 5 seconds with `fetch`. Renders one card per returned integration and shows the full API response JSON below the cards. Cards are colored by status: green border for `ok`, muted styling for `not_configured`.

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
