# launchpad-api

## Rules

- **Never run `git commit`, `git push`, or any git command that writes to or modifies repository history or remotes.** If a task requires committing or pushing, stop and tell the user to run the git command manually.
- **Never add a `Co-Authored-By` trailer or a "Generated with Claude Code" line** to commit messages or PR descriptions, including in suggested commit messages. Commits are authored by the user alone.
- Standard library first. Add a dependency only when it is the official client for something the app talks to, such as client-go or the Temporal and Azure SDKs.
- Put state on the `app` struct, with no global variables.
- Read config once in `main()`. Handlers never call `os.Getenv`.
- Keep every Go file in the repo root, one file per concern. [Project Structure](#project-structure) lists them.
- **Never include environment variable values, file contents, or internal paths in HTTP responses.** This includes error messages, debug output, and structured error bodies. launchpad-api runs with AWS credentials injected at `/aws-credentials/credentials`. Any response that leaks file system contents or env var values is a credential exposure to public guests at `launchpad.mattjarrett.dev`.

### Pre-commit safety check

Before telling the user to commit, always run `/security-review`. It reviews the pending changes on the current branch for security issues. Once it confirms the changes are safe, offer the user a suggested commit message - do not run `git commit` yourself.

## Project Overview

launchpad-api is the Go backend-for-frontend for the Launchpad UI. It reads and writes workspace YAML files in the `cujarrett/homelab-workspaces` GitHub repo via the GitHub Contents API, exposes a REST API consumed by the Angular SPA, and streams live resource status to the browser over SSE using the Kubernetes dynamic client. All write endpoints require a valid Azure Entra ID JWT bearing the `Contributor` app role.

## Tech Stack

- **Language**: Go 1.27, standard library first
- **Container**: Multi-stage Dockerfile, `linux/arm64`, non-root user
- **CI/CD**: GitHub Actions → GHCR (`ghcr.io/cujarrett/launchpad-api`)
- **Deployment**: Kubernetes via homelab Api Crossplane XR

## Project Structure

All application code lives in the repo root, with no subdirectories.

| File | Purpose |
|---|---|
| `main.go` | Startup, config, routing, graceful shutdown |
| `auth.go` | Azure Entra ID JWT middleware (JWKS validation, app role extraction) |
| `github.go` | GitHub Contents API client (list, read, write, delete files) |
| `workspaces.go` | `GET /api/workspaces` and `GET /api/workspaces/{name}/resources` handlers |
| `write.go` | `POST` and `DELETE` workspace/resource handlers |
| `render.go` | YAML rendering from schema + user params |
| `schema.go` | `GET /api/schema/{kind}` handler, which returns the JSON schema for a resource kind |
| `watcher.go` | Kubernetes dynamic client watch loop, feeds the broadcaster |
| `broadcaster.go` | Fan-out SSE broadcaster for `GET /api/status/watch` |
| `sse_ticket.go` | Short-lived, single-use tickets that let the browser open the SSE stream |
| `guest.go` | Guest sandbox handlers under `/api/guest/` |
| `validate.go` | Input validation for write requests |
| `cleanup.go` | Guest sandbox cleanup on Temporal: a durable timer workflow per sandbox, and a sweep on a one-minute schedule |

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
| `POST` | `/api/status/watch/ticket` | Bearer token | Trade the token for a single-use SSE ticket |
| `GET` | `/metrics` | none | Prometheus metrics |
| `GET`, `POST`, `PATCH` | `/api/guest/workspaces/...` | none, capped | Guest sandbox routes. The caps are in [README → Guest sandbox](./README.md#guest-sandbox) |

## Environment Variables

[README → Environment variables](./README.md#environment-variables) has the full table. `ENTRA_AUTH_DISABLED=true` skips JWT validation for local dev only.

## Coding Conventions

- The GitHub PAT is the one config value read on every call. It comes from its mounted file, so a rotation needs no restart.
- Fail fast. Exit at startup when required config is missing.
- `defer func() { _ = resp.Body.Close() }()` is the explicit discard that `errcheck` wants.
- Tests use `httptest.NewServer` and `httptest.NewRecorder`, with no real network calls and no env var dependencies.

## Local Development

```bash
# 1. Copy and review the example env file
cp .env.example .env
# GITHUB_TOKEN comes from ~/.secrets. Source it or set it by hand.
# Set ENTRA_AUTH_DISABLED=true to skip Entra auth locally

# 2. Run
just run

# 3. Verify
curl http://localhost:8080/healthz
curl http://localhost:8080/api/workspaces
```

Run `just ci` before pushing to catch lint and test failures early.

## CI/CD

- **`test` job**: runs on all pushes and PRs. `go test ./...` then `go vet ./...`.
- **`build-and-push` job**: runs on `main` after `test` passes. Builds the ARM64 image and pushes it to GHCR with `:main` and `:sha-<sha>` tags.
- **`deploy` job**: runs after `build-and-push`. Updates the image tag in `homelab-workspaces`.

## Philosophy: Grug-Brained Development

> "Complexity very, very bad." - [grugbrain.dev](https://grugbrain.dev/)

- **Say no.** The best weapon against complexity is the word "no". No new feature, no new abstraction, until it earns its place.
- **Cheapest rung that works.** Before writing code go down the ladder and stop at the first rung that solves it - skip the feature, reuse code already here, standard library, native platform feature, a dependency already installed, one line, then build the minimum.
- **No abstraction until a pattern repeats three times.** Let cut points emerge naturally from the code; don't invent them up front.
- **80/20 solutions.** Ship 80% of the value with 20% of the code. Ugly but working beats elegant but over-engineered.
- **Chesterton's Fence.** Understand why code exists before removing it. If you don't see the use, go away and think.
- **Boring, obvious code wins.** Intermediate variables with good names beat clever one-liners. Easier to debug.
- **DRY is not a law.** A little copy-paste beats a complex abstraction built for two cases.
- **No FOLD** (Fear Of Looking Dumb). If something is too complex, say so. That's a signal to simplify, not a personal failing.
