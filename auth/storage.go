// Package auth holds finbot's OAuth state for the MCP authorization server: a
// SQLite implementation of the fosite storage interfaces, DCR clients, per-user
// token generations and the persisted HMAC secret.
//
// Sessions must be *fosite.DefaultSession; anything else is refused when it is
// stored. Get methods ignore the session argument (fosite passes nil from
// revocation and a live session from the code exchange) and return a fresh one.
package auth

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/handler/pkce"
	fositestorage "github.com/ory/fosite/storage"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	tableCodes   = "oauth_auth_codes"
	tableAccess  = "oauth_access_tokens"
	tableRefresh = "oauth_refresh_tokens"
	tablePKCE    = "oauth_pkce_sessions"

	// timeLayout is fixed-width, so stored UTC timestamps compare as strings.
	timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
)

var (
	_ fosite.Storage                = (*Storage)(nil)
	_ oauth2.CoreStorage            = (*Storage)(nil)
	_ oauth2.TokenRevocationStorage = (*Storage)(nil)
	_ pkce.PKCERequestStorage       = (*Storage)(nil)
	_ fositestorage.Transactional   = (*Storage)(nil)
)

// Storage is the fosite storage over finbot's database. It logs nothing, and
// its errors carry no token, signature or secret.
type Storage struct {
	w, r *sql.DB
	now  func() time.Time
}

// New returns a Storage on db's writer and reader pools.
func New(db *store.DB) *Storage {
	return &Storage{w: db.Writer(), r: db.Reader(), now: time.Now}
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// querier is the part of *sql.DB and *sql.Conn that Storage uses.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{}

type txn struct {
	conn *sql.Conn
	done bool
}

func txFrom(ctx context.Context) *txn {
	t, _ := ctx.Value(txKey{}).(*txn)
	return t
}

// writer returns ctx's transaction, or the write pool.
func (s *Storage) writer(ctx context.Context) querier {
	if t := txFrom(ctx); t != nil {
		return t.conn
	}
	return s.w
}

// reader returns ctx's transaction, so a transaction reads its own writes, or
// the read pool.
func (s *Storage) reader(ctx context.Context) querier {
	if t := txFrom(ctx); t != nil {
		return t.conn
	}
	return s.r
}

// BeginTX implements fosite's Transactional: it holds the single writer
// connection inside BEGIN IMMEDIATE until Commit or Rollback. A nested begin is
// an error, because waiting for the held connection would deadlock.
func (s *Storage) BeginTX(ctx context.Context) (context.Context, error) {
	if txFrom(ctx) != nil {
		return ctx, errors.New("auth: nested transaction")
	}
	c, err := s.w.Conn(ctx)
	if err != nil {
		return ctx, fmt.Errorf("auth: acquire writer: %w", err)
	}
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		c.Close()
		return ctx, fmt.Errorf("auth: begin: %w", err)
	}
	return context.WithValue(ctx, txKey{}, &txn{conn: c}), nil
}

// Commit commits ctx's transaction. A failed COMMIT can leave SQLite's
// transaction open, so it rolls back and releases the writer before returning.
func (s *Storage) Commit(ctx context.Context) error {
	t := txFrom(ctx)
	if t == nil || t.done {
		return errors.New("auth: commit without a transaction")
	}
	if _, err := t.conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.rollback()
		return fmt.Errorf("auth: commit: %w", err)
	}
	t.done = true
	return t.conn.Close()
}

// Rollback rolls back ctx's transaction. It is a no-op once the transaction has
// finished, and it runs even if ctx is cancelled.
func (s *Storage) Rollback(ctx context.Context) error {
	t := txFrom(ctx)
	if t == nil {
		return errors.New("auth: rollback without a transaction")
	}
	if t.done {
		return nil
	}
	return t.rollback()
}

func (t *txn) rollback() error {
	t.done = true
	_, err := t.conn.ExecContext(context.Background(), "ROLLBACK")
	if err != nil {
		// The connection's state is unknown; drop it from the pool.
		t.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	t.conn.Close()
	if err != nil {
		return fmt.Errorf("auth: rollback: %w", err)
	}
	return nil
}

// inTx runs fn in ctx's transaction, or in a new one if ctx has none.
func (s *Storage) inTx(ctx context.Context, fn func(context.Context) error) error {
	if txFrom(ctx) != nil {
		return fn(ctx)
	}
	ctx, err := s.BeginTX(ctx)
	if err != nil {
		return err
	}
	defer s.Rollback(ctx)
	if err := fn(ctx); err != nil {
		return err
	}
	return s.Commit(ctx)
}

// storedForm lists the request parameters fosite reads back from storage. It's
// an allowlist because fosite's own default keeps "code": any other parameter,
// which could hold a raw token or code, is dropped before the row is written.
var storedForm = []string{"client_id", "code_challenge", "code_challenge_method", "grant_type", "redirect_uri", "response_type", "scope"}

// storedRequest is the JSON form of a fosite request. The client is stored as
// its id and read back from oauth_clients.
type storedRequest struct {
	ID                string                 `json:"id"`
	RequestedAt       time.Time              `json:"requested_at"`
	ClientID          string                 `json:"client_id"`
	RequestedScope    fosite.Arguments       `json:"requested_scope"`
	GrantedScope      fosite.Arguments       `json:"granted_scope"`
	RequestedAudience fosite.Arguments       `json:"requested_audience"`
	GrantedAudience   fosite.Arguments       `json:"granted_audience"`
	Form              url.Values             `json:"form"`
	Session           *fosite.DefaultSession `json:"session"`
}

// create stores req under signature. The row expires with the session's expiry
// for kind, and a session without one is refused: Prune needs it.
func (s *Storage) create(ctx context.Context, table string, kind fosite.TokenType, signature string, req fosite.Requester) error {
	sess, ok := req.GetSession().(*fosite.DefaultSession)
	if !ok || sess == nil {
		return fmt.Errorf("auth: store %s: session must be a *fosite.DefaultSession", table)
	}
	client := req.GetClient()
	if client == nil {
		return fmt.Errorf("auth: store %s: request has no client", table)
	}
	expires := sess.GetExpiresAt(kind)
	if expires.IsZero() {
		return fmt.Errorf("auth: store %s: session has no %s expiry", table, kind)
	}
	form := url.Values{}
	for _, k := range storedForm {
		if v, ok := req.GetRequestForm()[k]; ok {
			form[k] = v
		}
	}
	body, err := json.Marshal(storedRequest{
		ID:                req.GetID(),
		RequestedAt:       req.GetRequestedAt(),
		ClientID:          client.GetID(),
		RequestedScope:    req.GetRequestedScopes(),
		GrantedScope:      req.GetGrantedScopes(),
		RequestedAudience: req.GetRequestedAudience(),
		GrantedAudience:   req.GetGrantedAudience(),
		Form:              form,
		Session:           sess,
	})
	if err != nil {
		return fmt.Errorf("auth: encode %s: %w", table, err)
	}
	_, err = s.writer(ctx).ExecContext(ctx,
		`INSERT INTO `+table+` (signature, request_id, client_id, expires_at, request) VALUES (?, ?, ?, ?, ?)`,
		signature, req.GetID(), client.GetID(), formatTime(expires), string(body))
	if err != nil {
		return fmt.Errorf("auth: store %s: %w", table, err)
	}
	return nil
}

// load reads the request stored under signature, and the row's active flag.
// activeExpr is "active", or "1" for a table without that column. A session
// that fails the generation check reads as not found, whatever its row says.
func (s *Storage) load(ctx context.Context, table, activeExpr, signature string) (*fosite.Request, bool, error) {
	var body string
	var active bool
	err := s.reader(ctx).QueryRowContext(ctx,
		`SELECT request, `+activeExpr+` FROM `+table+` WHERE signature = ?`, signature).Scan(&body, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fosite.ErrNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("auth: load %s: %w", table, err)
	}
	var sr storedRequest
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		return nil, false, fmt.Errorf("auth: decode %s: bad stored request", table)
	}
	if err := s.CheckSession(ctx, sr.Session); errors.Is(err, ErrRevoked) {
		return nil, false, fosite.ErrNotFound
	} else if err != nil {
		return nil, false, err
	}
	client, err := s.GetClient(ctx, sr.ClientID)
	if err != nil {
		return nil, false, err
	}
	return &fosite.Request{
		ID:                sr.ID,
		RequestedAt:       sr.RequestedAt,
		Client:            client,
		RequestedScope:    sr.RequestedScope,
		GrantedScope:      sr.GrantedScope,
		RequestedAudience: sr.RequestedAudience,
		GrantedAudience:   sr.GrantedAudience,
		Form:              sr.Form,
		Session:           sr.Session,
	}, active, nil
}

func (s *Storage) delete(ctx context.Context, table, signature string) error {
	if _, err := s.writer(ctx).ExecContext(ctx, `DELETE FROM `+table+` WHERE signature = ?`, signature); err != nil {
		return fmt.Errorf("auth: delete %s: %w", table, err)
	}
	return nil
}

// CreateAuthorizeCodeSession stores an authorization code's request.
func (s *Storage) CreateAuthorizeCodeSession(ctx context.Context, signature string, req fosite.Requester) error {
	return s.create(ctx, tableCodes, fosite.AuthorizeCode, signature, req)
}

// GetAuthorizeCodeSession returns the request, along with
// ErrInvalidatedAuthorizeCode if the code was used: fosite needs the request's
// id to revoke the grant.
func (s *Storage) GetAuthorizeCodeSession(ctx context.Context, signature string, _ fosite.Session) (fosite.Requester, error) {
	req, active, err := s.load(ctx, tableCodes, "active", signature)
	if err != nil {
		return nil, err
	}
	if !active {
		return req, fosite.ErrInvalidatedAuthorizeCode
	}
	return req, nil
}

// InvalidateAuthorizeCodeSession marks a code used. Only one caller can do that
// to a code, so of two racing exchanges the second gets ErrNotFound.
func (s *Storage) InvalidateAuthorizeCodeSession(ctx context.Context, signature string) error {
	res, err := s.writer(ctx).ExecContext(ctx,
		`UPDATE `+tableCodes+` SET active = 0 WHERE signature = ? AND active = 1`, signature)
	if err != nil {
		return fmt.Errorf("auth: invalidate code: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("auth: invalidate code: %w", err)
	} else if n == 0 {
		return fosite.ErrNotFound
	}
	return nil
}

// CreatePKCERequestSession stores a code's PKCE challenge, under the code's
// signature and with the code's expiry.
func (s *Storage) CreatePKCERequestSession(ctx context.Context, signature string, req fosite.Requester) error {
	return s.create(ctx, tablePKCE, fosite.AuthorizeCode, signature, req)
}

// GetPKCERequestSession returns a code's PKCE request.
func (s *Storage) GetPKCERequestSession(ctx context.Context, signature string, _ fosite.Session) (fosite.Requester, error) {
	req, _, err := s.load(ctx, tablePKCE, "1", signature)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// DeletePKCERequestSession deletes a code's PKCE request.
func (s *Storage) DeletePKCERequestSession(ctx context.Context, signature string) error {
	return s.delete(ctx, tablePKCE, signature)
}

// CreateAccessTokenSession stores an access token's request.
func (s *Storage) CreateAccessTokenSession(ctx context.Context, signature string, req fosite.Requester) error {
	return s.create(ctx, tableAccess, fosite.AccessToken, signature, req)
}

// GetAccessTokenSession returns an access token's request.
func (s *Storage) GetAccessTokenSession(ctx context.Context, signature string, _ fosite.Session) (fosite.Requester, error) {
	req, _, err := s.load(ctx, tableAccess, "1", signature)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// DeleteAccessTokenSession deletes one access token.
func (s *Storage) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	return s.delete(ctx, tableAccess, signature)
}

// CreateRefreshTokenSession stores a refresh token's request. Revocation goes by
// request id, so the access token's signature isn't kept.
func (s *Storage) CreateRefreshTokenSession(ctx context.Context, signature, _ string, req fosite.Requester) error {
	return s.create(ctx, tableRefresh, fosite.RefreshToken, signature, req)
}

// GetRefreshTokenSession returns the request, along with ErrInactiveToken if the
// token was rotated or revoked: fosite treats that as reuse and revokes the
// grant, using the request's id.
func (s *Storage) GetRefreshTokenSession(ctx context.Context, signature string, _ fosite.Session) (fosite.Requester, error) {
	req, active, err := s.load(ctx, tableRefresh, "active", signature)
	if err != nil {
		return nil, err
	}
	if !active {
		return req, fosite.ErrInactiveToken
	}
	return req, nil
}

// DeleteRefreshTokenSession deletes one refresh token.
func (s *Storage) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	return s.delete(ctx, tableRefresh, signature)
}

// RevokeRefreshToken deactivates every refresh token of a grant. The rows stay
// until they expire, so a later replay is still recognised.
func (s *Storage) RevokeRefreshToken(ctx context.Context, requestID string) error {
	if _, err := s.writer(ctx).ExecContext(ctx,
		`UPDATE `+tableRefresh+` SET active = 0 WHERE request_id = ?`, requestID); err != nil {
		return fmt.Errorf("auth: revoke refresh tokens: %w", err)
	}
	return nil
}

// RevokeAccessToken deletes every access token of a grant.
func (s *Storage) RevokeAccessToken(ctx context.Context, requestID string) error {
	if _, err := s.writer(ctx).ExecContext(ctx,
		`DELETE FROM `+tableAccess+` WHERE request_id = ?`, requestID); err != nil {
		return fmt.Errorf("auth: revoke access tokens: %w", err)
	}
	return nil
}

// RotateRefreshToken retires a grant's current refresh and access tokens, as the
// first step of issuing new ones. Only one caller can rotate a given token: of
// two racing refreshes, the second gets ErrSerializationFailure.
func (s *Storage) RotateRefreshToken(ctx context.Context, requestID, signature string) error {
	return s.inTx(ctx, func(ctx context.Context) error {
		res, err := s.writer(ctx).ExecContext(ctx,
			`UPDATE `+tableRefresh+` SET active = 0 WHERE signature = ? AND request_id = ? AND active = 1`, signature, requestID)
		if err != nil {
			return fmt.Errorf("auth: rotate refresh token: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("auth: rotate refresh token: %w", err)
		} else if n == 0 {
			return fosite.ErrSerializationFailure
		}
		if err := s.RevokeRefreshToken(ctx, requestID); err != nil {
			return err
		}
		return s.RevokeAccessToken(ctx, requestID)
	})
}

// Prune deletes rows past their expiry and returns how many. A used code or a
// rotated refresh token stays until then, for replay detection. Clients stay
// until they're evicted at the cap (RegisterClient).
func (s *Storage) Prune(ctx context.Context) (int64, error) {
	var total int64
	now := formatTime(s.now())
	err := s.inTx(ctx, func(ctx context.Context) error {
		for _, table := range []string{tableCodes, tableAccess, tableRefresh, tablePKCE} {
			res, err := s.writer(ctx).ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at <= ?`, now)
			if err != nil {
				return fmt.Errorf("auth: prune %s: %w", table, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("auth: prune %s: %w", table, err)
			}
			total += n
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
