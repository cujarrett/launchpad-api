# launchpad-api

The Go backend for [launchpad](https://github.com/cujarrett/launchpad). A form submission comes in, this binary commits YAML to GitHub, ArgoCD picks it up, Crossplane provisions the workload. The write path never touches the Kubernetes API — GitHub is the source of truth.

The read path runs the opposite direction: a Kubernetes dynamic client watches all platform XR kinds and fans out status events over SSE to every connected browser.

Stdlib only — no web framework. See [HOW_IT_WORKS.md](./HOW_IT_WORKS.md) for the full architecture.

## Local dev

```bash
# Set LAUNCHPAD_API (GitHub PAT, contents:write on homelab-workspaces)
# Set ENTRA_TENANT_ID and ENTRA_API_CLIENT_ID from your Azure app registration
# Set ENTRA_AUTH_DISABLED=true to skip JWT validation locally

just run
```

```bash
curl http://localhost:8080/healthz
```

## Commands

| Command | What it does |
|---|---|
| `just ci` | `lint → test → build` |
| `just lint` | `go mod tidy -diff` + `golangci-lint` |
| `just test` | `go test -race ./...` |
| `just build` | `go build -o launchpad-api .` |
| `just run` | `go run .` |

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `LAUNCHPAD_API` | yes | GitHub PAT with `contents: write` on `cujarrett/homelab-workspaces` |
| `ENTRA_TENANT_ID` | yes | Azure Entra ID tenant GUID |
| `ENTRA_API_CLIENT_ID` | yes | Client ID of the API app registration |
| `PORT` | no | HTTP listen port (default `8080`) |
| `ENTRA_AUTH_DISABLED` | no | `true` to skip JWT validation locally — blocked on port 443 |

## Deployment

ARM64 Docker image built by CI and pushed to GHCR. Deployed as an XApi Crossplane XR in the cluster via ArgoCD. The XR mounts a `launchpad-secrets` K8s secret as `envFrom`, injecting `LAUNCHPAD_API`, `ENTRA_TENANT_ID`, and `ENTRA_API_CLIENT_ID` into the pod.

### Rotating `LAUNCHPAD_API`

`LAUNCHPAD_API` is a GitHub PAT with `contents: write` on `cujarrett/homelab-workspaces`. Rotate it when the PAT expires or is compromised.

1. Generate a new PAT in GitHub with `contents: write` on `cujarrett/homelab-workspaces`
2. Patch the secret (avoids the value appearing in shell history):

```bash
read -rs NEW_TOKEN
kubectl patch secret launchpad-secrets -n launchpad \
  --type='json' \
  -p='[{"op":"replace","path":"/data/LAUNCHPAD_API","value":"'"$(echo -n "$NEW_TOKEN" | base64)"'"}]'
unset NEW_TOKEN
```

3. Restart the deployment — env vars are injected at pod startup, so a restart is required:

```bash
kubectl rollout restart deployment/launchpad-api -n launchpad
```

4. Verify by opening `https://launchpad.mattjarrett.dev` and confirming workspace XR commits succeed in sandbox.
