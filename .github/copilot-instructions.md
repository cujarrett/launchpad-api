# Copilot Instructions — launchpad-api

## Copilot Rules

- **Never run `git commit`, `git push`, or any git command that writes to or modifies repository history or remotes.** If a task requires committing or pushing, stop and tell the user to run the git command manually.
- Do not add external dependencies — stdlib only.
- Do not use global variables — put state on the `app` struct.
- Do not call `os.Getenv` inside handlers — read config once in `main()`.
- Keep all application code in `main.go` and all tests in `main_test.go` unless the file grows large enough to warrant splitting.
- **Never include environment variable values, file contents, or internal paths in HTTP responses.** This includes error messages, debug output, and structured error bodies. launchpad-api runs with AWS credentials injected at `/aws-credentials/credentials` — any response that leaks file system contents or env var values is a credential exposure to public guests at `launchpad.mattjarrett.dev`.

## Project Overview

launchpad-api is the Go backend-for-frontend for the Launchpad UI. It reads and writes workspace YAML files in the `cujarrett/homelab-workspaces` GitHub repo via the GitHub Contents API, exposes a REST API consumed by the Angular SPA, and streams live resource status to the browser over SSE using the Kubernetes dynamic client. All write endpoints require a valid Azure Entra ID JWT bearing the `Contributor` app role.

## Tech Stack

- **Language**: Go 1.26, stdlib only — no external dependencies
- **Container**: Multi-stage Dockerfile, `linux/arm64`, non-root user
- **CI/CD**: GitHub Actions → GHCR (`ghcr.io/cujarrett/launchpad-api`)
- **Deployment**: Kubernetes via homelab XApi Crossplane XR

## Project Structure

All application code lives in the repo root — no subdirectories.

| File | Purpose |
|---|---|
| `main.go` | Startup, config, routing, graceful shutdown |
| `auth.go` | Azure Entra ID JWT middleware (JWKS validation, app role extraction) |
| `github.go` | GitHub Contents API client (list, read, write, delete files) |
| `workspaces.go` | `GET /api/workspaces` and `GET /api/workspaces/{name}/resources` handlers |
| `write.go` | `POST` and `DELETE` workspace/resource handlers |
| `render.go` | YAML rendering from schema + user params |
| `schema.go` | `GET /api/schema/{kind}` handler — returns JSON schema for a resource kind |
| `watcher.go` | Kubernetes dynamic client watch loop, feeds the broadcaster |
| `broadcaster.go` | Fan-out SSE broadcaster for `GET /api/status/watch` |
| `validate.go` | Input validation for write requests |

## Architecture

The API is a thin stateless layer between the Launchpad SPA, GitHub, and Kubernetes.

- **Read path**: handlers call the GitHub Contents API to list/read YAML files and return JSON to the SPA.
- **Write path**: POST/DELETE handlers validate the request, render YAML, then commit the file to `homelab-workspaces` via the GitHub Contents API. ArgoCD picks up the commit and reconciles the cluster.
- **Status stream**: a Kubernetes dynamic client watch loop fans out resource status events to connected SSE clients via the broadcaster. GET requests are unauthenticated (read-only); write endpoints require the `Contributor` Entra app role.
- **Auth**: JWT tokens are issued by Azure Entra ID. The middleware validates RS256 signature (JWKS from `login.microsoftonline.com`), issuer, audience (`ENTRA_API_CLIENT_ID`), and `access_as_user` scope, then extracts app roles into request context.

## Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/healthz` | none | Kubernetes readiness probe |
| `GET` | `/api/workspaces` | none | List all workspaces |
| `GET` | `/api/workspaces/{name}/resources` | none | List resources in a workspace |
| `POST` | `/api/workspaces` | Contributor | Create a workspace |
| `POST` | `/api/workspaces/{name}/resources` | Contributor | Create a resource in a workspace |
| `DELETE` | `/api/workspaces/{name}` | Contributor | Delete a workspace |
| `DELETE` | `/api/workspaces/{name}/resources/{resource}` | Contributor | Delete a resource |
| `GET` | `/api/workspaces/{name}/resources/{resource}/values` | none | Get current values for a resource |
| `GET` | `/api/schema/{kind}` | none | Get JSON schema for a resource kind |
| `GET` | `/api/status/watch` | none (token optional) | SSE stream of live resource status |

## Environment Variables

| Variable | Required | Description |
|---|---|---|
| `LAUNCHPAD_API` | yes | GitHub PAT with `contents: write` on `cujarrett/homelab-workspaces` |
| `ENTRA_TENANT_ID` | yes | Azure Entra ID tenant GUID |
| `ENTRA_API_CLIENT_ID` | yes | Client ID of the API app registration (used as JWT audience) |
| `PORT` | no | HTTP listen port (default `8080`) |
| `GITHUB_OWNER` | no | GitHub org/user for the workspaces repo (default `cujarrett`) |
| `GITHUB_REPO` | no | GitHub repo name for the workspaces repo (default `homelab-workspaces`) |
| `ENTRA_AUTH_DISABLED` | no | Set to `true` to skip JWT validation in local dev — **never on port 443** |

## Coding Conventions

- Stdlib only — do not add external dependencies to `go.mod`
- No package-level globals — all state lives on the `app` struct
- Read config once at startup, not per-request
- Fail fast: use `log.Fatal` at startup for missing required config
- `defer func() { _ = resp.Body.Close() }()` — explicit discard for `errcheck`
- All tests use `httptest.NewServer` / `httptest.NewRecorder` — no real network calls, no env var dependencies

## Local Development

```bash
# 1. Copy and review the example env file
cp .env.example .env
# LAUNCHPAD_API comes from ~/.secrets — source it or set it manually
# Set ENTRA_AUTH_DISABLED=true to skip Entra auth locally

# 2. Run
just run

# 3. Verify
curl http://localhost:8080/healthz
curl http://localhost:8080/api/workspaces
```

Run `just ci` before pushing to catch lint and test failures early.

## CI/CD

- **`test` job**: runs on all pushes and PRs — `go test ./...` then `go vet ./...`
- **`build-and-push` job**: runs on `main` only after `test` passes — builds ARM64 Docker image and pushes to GHCR with `:main` and `:sha-<sha>` tags

## Version

Set at build time via `-ldflags="-X main.version=x.y.z"` in the Dockerfile. Defaults to `"dev"` when running locally with `go run`.

## Philosophy: Grug-Brained Development

> "Complexity very, very bad." — [grugbrain.dev](https://grugbrain.dev/)

- **Say no.** The best weapon against complexity is the word "no". No new feature, no new abstraction, until it earns its place.
- **No abstraction until a pattern repeats three times.** Let cut points emerge naturally from the code; don't invent them up front.
- **80/20 solutions.** Ship 80% of the value with 20% of the code. Ugly but working beats elegant but over-engineered.
- **Chesterton's Fence.** Understand why code exists before removing it. If you don't see the use, go away and think.
- **Boring, obvious code wins.** Intermediate variables with good names beat clever one-liners. Easier to debug.
- **DRY is not a law.** A little copy-paste beats a complex abstraction built for two cases.
- **No FOLD** (Fear Of Looking Dumb). If something is too complex, say so. That's a signal to simplify, not a personal failing.
