package main

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/ory/fosite"
)

const consentCookie = "finbot_consent"

// consentPage shows which client is asking and where the code will be sent,
// so a DCR-registered phishing client can't silently receive a code.
var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Allow access to finbot?</title>
<style>body{font:16px system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem}
code{background:#eee;padding:.1rem .3rem}.warn{color:#a40}button{font-size:1rem;padding:.5rem 1rem;margin-right:.5rem}</style>
</head><body>
<h1>Allow access to finbot?</h1>
<p>Signed in as <strong>{{.User}}</strong>.</p>
<p>The app <strong>{{.ClientName}}</strong> wants to use finbot's tools on your behalf.
After you allow it, finbot sends access to <code>{{.Host}}</code>.</p>
{{if .Loopback}}<p class="warn">This address is on your own computer, so any program running there could be the requester.
Only allow it if you just started a sign-in from a local app such as Claude Code.</p>{{end}}
<p>If you didn't start this sign-in, or you don't recognise <code>{{.Host}}</code>, choose Deny.</p>
<form method="post" action="/oauth/consent">
<input type="hidden" name="consent_id" value="{{.ConsentID}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button name="decision" value="approve">Allow</button>
<button name="decision" value="deny">Deny</button>
</form></body></html>`))

func (s *AuthServer) renderConsent(w http.ResponseWriter, ar fosite.AuthorizeRequester, consentID, csrf string, id Identity) {
	ru := ar.GetRedirectURI()
	host := ru.Hostname()
	ip := net.ParseIP(host)
	name := s.store.name(ar.GetClient().GetID())
	if name == "" {
		name = "(unnamed app)"
	}
	user := id.Name
	if user == "" {
		user = UserID(id.Email)
	}
	http.SetCookie(w, &http.Cookie{
		Name: consentCookie, Value: csrf, Path: "/oauth/consent", MaxAge: int(pendingLoginTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: strings.HasPrefix(s.issuer, "https://"),
	})
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	// form-action also governs the redirect after the POST, so it must allow
	// the (validated, just-shown) redirect origin or browsers block the code.
	h.Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self' %s://%s; frame-ancestors 'none'", ru.Scheme, ru.Host))
	err := consentPage.Execute(w, map[string]any{
		"User": user, "ClientName": name, "Host": ru.Host, "ConsentID": consentID, "CSRF": csrf,
		"Loopback": host == "localhost" || (ip != nil && ip.IsLoopback()),
	})
	if err != nil {
		log.Printf("render consent: %v", err)
	}
}

func constantEq(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
