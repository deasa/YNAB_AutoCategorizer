package mcp

import (
	"log/slog"
	"net/http"
)

// LogRequests logs method, path and status, never query strings (codes) or
// headers (tokens). Paths outside finbot's routes are anyone's text, so they're
// logged as "other". Client IPs aren't logged at all.
func LogRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.Info("mcp request", "method", r.Method, "path", logPath(r.URL.Path), "status", rec.status)
	})
}

// loggedPaths are the routes logged by name.
var loggedPaths = map[string]bool{
	Path:                     true,
	"/oauth/register":        true,
	"/oauth/authorize":       true,
	"/oauth/google/callback": true,
	"/oauth/consent":         true,
	"/oauth/token":           true,
	"/oauth/revoke":          true,
	"/.well-known/oauth-authorization-server":      true,
	"/.well-known/oauth-protected-resource":        true,
	"/.well-known/oauth-protected-resource" + Path: true,
}

func logPath(p string) string {
	if loggedPaths[p] {
		return p
	}
	return "other"
}

// CORS lets browser-based clients run discovery, DCR and the token exchange. Any
// origin is fine: nothing here uses ambient credentials; every call carries its
// own bearer token or PKCE proof. (The consent page's cookie is only its CSRF
// token, and it's SameSite-bound by the auth package.)
func CORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Access-Control-Allow-Origin", "*")
		hd.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID")
		hd.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// statusRecorder keeps streaming working: Flush and Unwrap reach the real writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
