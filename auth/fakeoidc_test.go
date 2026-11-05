package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const (
	fakeClientID     = "fake-google-client"
	fakeClientSecret = "fake-google-secret"
)

// fakeOIDC is a real OIDC provider on the wire: discovery, JWKS, an authorize
// endpoint that signs in instantly, and a token endpoint that checks PKCE and
// the client secret and returns an RS256 id_token. The production verifier runs
// against it unchanged.
type fakeOIDC struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu       sync.Mutex
	identity Identity
	codes    map[string]fakeCode
	tamper   func(claims map[string]any) // changes the id_token's claims
	signKey  *rsa.PrivateKey             // signs with this key, if set
	noIDTok  bool
}

type fakeCode struct{ nonce, challenge, redirect string }

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{key: key, codes: map[string]fakeCode{},
		identity: Identity{Email: "alice@example.com", EmailVerified: true, Name: "Alice"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("GET /auth", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOIDC) signInAs(id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity = id
}

func (f *fakeOIDC) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                f.srv.URL,
		"authorization_endpoint":                f.srv.URL + "/auth",
		"token_endpoint":                        f.srv.URL + "/token",
		"jwks_uri":                              f.srv.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (f *fakeOIDC) jwks(w http.ResponseWriter, _ *http.Request) {
	enc := base64.RawURLEncoding.EncodeToString
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
		"n": enc(f.key.N.Bytes()), "e": enc(big.NewInt(int64(f.key.E)).Bytes()),
	}}})
}

func (f *fakeOIDC) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != fakeClientID || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	code := rand.Text()
	f.mu.Lock()
	f.codes[code] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	f.mu.Unlock()
	u, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	v := url.Values{"code": {code}, "state": {q.Get("state")}}
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (f *fakeOIDC) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[r.PostFormValue("code")]
	delete(f.codes, r.PostFormValue("code"))
	if !ok || r.PostFormValue("client_secret") != fakeClientSecret ||
		oauth2.S256ChallengeFromVerifier(r.PostFormValue("code_verifier")) != c.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	resp := map[string]any{"access_token": "google-access", "token_type": "Bearer", "expires_in": 3600}
	if !f.noIDTok {
		now := time.Now()
		claims := map[string]any{
			"iss": f.srv.URL, "aud": fakeClientID, "sub": "1234", "nonce": c.nonce,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"email": f.identity.Email, "email_verified": f.identity.EmailVerified, "name": f.identity.Name,
		}
		if f.tamper != nil {
			f.tamper(claims)
		}
		key := f.key
		if f.signKey != nil {
			key = f.signKey
		}
		tok, err := signRS256(key, claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp["id_token"] = tok
	}
	writeJSON(w, http.StatusOK, resp)
}

func signRS256(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	in := enc(h) + "." + enc(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return in + "." + enc(sig), nil
}
