# List available recipes
default:
    @just --list

# Run all CI checks locally before pushing
ci: lint test build

# Verify go.mod is tidy and run golangci-lint
lint:
    go mod tidy -diff
    golangci-lint run

# Run tests with race detector
test:
    go test -race ./...

# Build binary
build:
    go build -o launchpad-api .

# Run locally
run:
    go run .

# Preview hello-world-spa against a live sandbox's API (e.g. just preview-spa guest-atomic-llama)
preview-spa workspace port="8765":
    #!/usr/bin/env bash
    set -euo pipefail
    # The demoN slot a workspace gets is assigned at creation, so read the real
    # hostname off the ingress rather than assuming which slot it landed on.
    # `|| true` because grep exits 1 on no match and pipefail would abort first.
    api=$(kubectl get ingress -n "{{workspace}}" \
        -o jsonpath='{range .items[*]}{.spec.rules[*].host}{"\n"}{end}' 2>/dev/null \
        | grep -- '-api\.' | head -1 || true)
    if [ -z "$api" ]; then
        echo "No API ingress in {{workspace}}. Sandboxes expire after ~10 minutes - is it still live?" >&2
        echo "Live now: $(kubectl get ns -o name 2>/dev/null | grep guest- | sed 's|namespace/||' | paste -sd' ' - || echo none)" >&2
        exit 1
    fi
    url="http://localhost:{{port}}/index.html?api=https://$api/"
    if lsof -nP -iTCP:{{port}} -sTCP:LISTEN >/dev/null 2>&1; then
        echo "Reusing the server already on :{{port}}"
        open "$url" 2>/dev/null || echo "Open: $url"
        exit 0
    fi
    echo "Serving hello-world-spa -> $url"
    echo "Ctrl-C to stop."
    trap 'kill 0' EXIT
    python3 -m http.server {{port}} --directory hello-world-spa >/dev/null 2>&1 &
    sleep 1
    open "$url" 2>/dev/null || echo "Open: $url"
    wait
