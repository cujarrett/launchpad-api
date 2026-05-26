package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

type contextKey int

const rolesKey contextKey = 0

type authMiddleware struct {
	jwks     keyfunc.Keyfunc
	tenantID string
	clientID string
	disabled bool
}

func newAuthMiddleware(ctx context.Context) (*authMiddleware, error) {
	if os.Getenv("ENTRA_AUTH_DISABLED") == "true" {
		if os.Getenv("PORT") == "443" {
			return nil, fmt.Errorf("auth: ENTRA_AUTH_DISABLED=true is not permitted on port 443")
		}
		slog.Error("⚠️  AUTH DISABLED — all requests granted Contributor — development only")
		return &authMiddleware{disabled: true}, nil
	}

	tenantID := mustEnv("ENTRA_TENANT_ID")
	clientID := mustEnv("ENTRA_API_CLIENT_ID")

	jwksURL := fmt.Sprintf(
		"https://login.microsoftonline.com/%s/discovery/v2.0/keys",
		tenantID,
	)

	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("auth: failed to fetch JWKS: %w", err)
	}

	slog.Info("auth: JWKS loaded", "workspace", tenantID, "clientID", clientID)
	return &authMiddleware{
		jwks:     k,
		tenantID: tenantID,
		clientID: clientID,
	}, nil
}

// requireAuth wraps a handler with JWT validation.
//
// Read-only policy: GET requests and SSE are publicly accessible — no token
// required. If a token IS present on a GET it is still validated so that
// Contributors receive their roles (write buttons appear in the UI).
//
// Write policy: POST / DELETE / PUT require a valid Bearer token. The
// individual handlers also call requireContributor to enforce the role.
func (a *authMiddleware) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.disabled {
			// Grant all roles in dev mode so write endpoints work without Entra.
			ctx := context.WithValue(r.Context(), rolesKey, []string{"Contributor"})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// SSE: token is optional (passed as ?token= by logged-in clients).
		// Anonymous viewers get the status stream with no roles.
		if r.Method == http.MethodGet && r.URL.Path == "/api/status/watch" {
			token := r.URL.Query().Get("token")
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			roles, err := a.validateToken(token)
			if err != nil {
				slog.Debug("auth: SSE token rejected", "err", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), rolesKey, roles)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		// GET with no token — allow as anonymous (no roles, no write access).
		if raw == "" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		// POST / DELETE / PUT — token is mandatory.
		if raw == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		roles, err := a.validateToken(raw)
		if err != nil {
			slog.Debug("auth: token rejected", "err", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), rolesKey, roles)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireContributor checks the request context for the Contributor App Role.
// Returns false and writes 403 if the role is absent.
func requireContributor(w http.ResponseWriter, r *http.Request) bool {
	roles, _ := r.Context().Value(rolesKey).([]string)
	for _, role := range roles {
		if role == "Contributor" {
			return true
		}
	}
	http.Error(w, "forbidden: Contributor role required", http.StatusForbidden)
	return false
}

func (a *authMiddleware) validateToken(raw string) ([]string, error) {
	expectedIssuer := fmt.Sprintf(
		"https://login.microsoftonline.com/%s/v2.0",
		a.tenantID,
	)
	expectedAudience := a.clientID // v2 access tokens use bare GUID, not api:// URI

	token, err := jwt.Parse(raw,
		a.jwks.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(expectedIssuer),
		jwt.WithAudience(expectedAudience),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token not valid")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("claims not readable")
	}

	// Verify scp claim contains access_as_user
	scp, _ := claims["scp"].(string)
	hasScope := false
	for _, s := range strings.Fields(scp) {
		if s == "access_as_user" {
			hasScope = true
			break
		}
	}
	if !hasScope {
		return nil, errors.New("missing scope: access_as_user")
	}

	// Extract App Roles from the roles claim.
	var roles []string
	if rawRoles, ok := claims["roles"].([]interface{}); ok {
		for _, rv := range rawRoles {
			if s, ok := rv.(string); ok {
				roles = append(roles, s)
			}
		}
	}

	return roles, nil
}
