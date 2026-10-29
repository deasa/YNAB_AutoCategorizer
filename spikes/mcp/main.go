// Command mcp-spike is a hello MCP server (Streamable HTTP) protected by a
// minimal fosite-based OAuth 2.1 authorization server that delegates login to
// Google and admits only allowlisted accounts.
//
//	go run .                    serve on MCP_LISTEN_ADDR (default localhost:8080)
//	go run . revoke <email>     invalidate all of that user's tokens
//	go run . revoke --all       invalidate every user's tokens
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const mcpPath = "/mcp"

func main() {
	// .env lives at the repo root; the spike runs from spikes/mcp.
	for _, f := range []string{".env", "../../.env"} {
		_ = godotenv.Load(f) // first file wins; missing files are fine
	}
	gens := NewGenerations(envOr("MCP_DATA_DIR", ".data") + "/generations.json")

	if len(os.Args) > 1 && os.Args[1] == "revoke" {
		if err := revoke(gens, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(gens); err != nil {
		log.Fatal(err)
	}
}

func revoke(gens *Generations, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: revoke <email> | revoke --all")
	}
	id := allUsers
	if args[0] != "--all" {
		id = UserID(args[0])
	}
	gen, err := gens.Bump(id)
	if err != nil {
		return err
	}
	fmt.Printf("revoked tokens for %s (generation now %d)\n", id, gen)
	return nil
}

func serve(gens *Generations) error {
	addr := envOr("MCP_LISTEN_ADDR", "localhost:8080")
	issuer := strings.TrimRight(envOr("MCP_PUBLIC_URL", "http://"+addr), "/")
	allow := ParseAllowlist(os.Getenv("MCP_ALLOWED_EMAILS"))
	if len(allow) == 0 {
		log.Printf("warning: MCP_ALLOWED_EMAILS is empty; every sign-in will be rejected")
	}
	clientID, secret := os.Getenv("GOOGLE_OAUTH_CLIENT_ID"), os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")
	if clientID == "" || secret == "" {
		return fmt.Errorf("GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET are required")
	}
	idp, err := NewGoogleProvider(context.Background(), clientID, secret, issuer+"/oauth/google/callback")
	if err != nil {
		return err
	}
	as, err := NewAuthServer(issuer, idp, allow, gens)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	as.Register(mux, mcpPath)
	mux.Handle(mcpPath, mcpHandler(as.VerifyToken, as.ResourceMetadataURL()))
	log.Printf("serving MCP at %s%s (%d allowlisted)", issuer, mcpPath, len(allow))
	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(cors(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return srv.ListenAndServe()
}

// logRequests logs method, path and status; never query strings (codes) or headers (tokens).
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d", r.Method, r.URL.Path, rec.status)
	})
}

// cors lets browser-based clients (MCP Inspector) run discovery, DCR and the
// token exchange. Any origin is fine: nothing here uses cookies or other
// ambient credentials; every call carries its own bearer token or PKCE proof.
func cors(h http.Handler) http.Handler {
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
