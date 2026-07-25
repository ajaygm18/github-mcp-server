package http

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// The upstream HTTP server is multi-tenant: every request must carry its own
// GitHub credential in an Authorization header, and the server holds no token
// of its own. That is the correct design for a shared deployment, but it makes
// the server unusable from MCP clients that can only be given a bare URL.
//
// The gateway below is an opt-in, single-tenant mode for exactly that case. It
// injects an operator-supplied GitHub token into each request, and is therefore
// only safe when access to the endpoint is itself restricted. A shared secret is
// mandatory rather than optional: without it, publishing the URL would publish
// write access to the token owner's account.
const (
	// envGatewayToken holds the GitHub token injected into each authorized
	// request. When empty, the gateway is disabled and upstream per-request
	// bearer authentication applies unchanged.
	envGatewayToken = "MCP_GATEWAY_GITHUB_TOKEN"

	// envGatewayKey holds the shared secret callers must present. Required
	// whenever envGatewayToken is set.
	envGatewayKey = "MCP_GATEWAY_KEY"

	// headerGatewayKey is the preferred way to present the shared secret.
	headerGatewayKey = "X-MCP-Key"

	// queryGatewayKey allows the secret in the query string, for clients that
	// accept only a URL and cannot set custom headers.
	queryGatewayKey = "key"

	// minGatewayKeyLength is enforced because the key is the sole barrier in
	// front of an injected write-scoped credential.
	minGatewayKeyLength = 32
)

// newStaticTokenGateway builds the gateway middleware from the environment.
//
// It returns (nil, nil) when the gateway is disabled, leaving upstream
// behaviour untouched. It returns an error — failing startup — when the
// configuration would be unsafe, rather than silently degrading to an open
// endpoint.
func newStaticTokenGateway(logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	token := strings.TrimSpace(os.Getenv(envGatewayToken))
	key := strings.TrimSpace(os.Getenv(envGatewayKey))

	if token == "" {
		if key != "" {
			logger.Warn(
				"gateway key is set but no gateway token was provided; gateway disabled and per-request bearer auth still applies",
				"keyVar", envGatewayKey,
				"tokenVar", envGatewayToken,
			)
		}
		return nil, nil
	}

	if len(key) < minGatewayKeyLength {
		return nil, fmt.Errorf(
			"%s must be at least %d characters of unguessable randomness whenever %s is set: the gateway injects a GitHub token into every authorized request, so this secret is the only thing standing between a public URL and write access to the token owner's account (generate one with: openssl rand -hex 32)",
			envGatewayKey, minGatewayKeyLength, envGatewayToken,
		)
	}

	logger.Info("static token gateway enabled", "keyLength", len(key))

	expected := []byte(key)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// CORS preflight carries no credentials by design; let the CORS
			// middleware answer it.
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			presented := []byte(presentedGatewayKey(r))
			if subtle.ConstantTimeCompare(presented, expected) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized: missing or invalid MCP gateway key"}`))
				return
			}

			// Replace rather than append: once the gateway is on, the caller's
			// own Authorization value is the shared secret, never a credential
			// that should reach the GitHub API.
			r.Header.Set("Authorization", "Bearer "+token)
			next.ServeHTTP(w, r)
		})
	}, nil
}

// presentedGatewayKey extracts the caller's shared secret, preferring the
// dedicated header, then the query string, then a bearer token.
func presentedGatewayKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(headerGatewayKey)); v != "" {
		return v
	}

	if v := strings.TrimSpace(r.URL.Query().Get(queryGatewayKey)); v != "" {
		return v
	}

	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}

	return ""
}
