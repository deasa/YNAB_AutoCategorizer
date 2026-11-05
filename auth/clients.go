package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ory/fosite"
)

// MaxClients caps the registered DCR clients. POST /oauth/register is
// unauthenticated, so this bound has to hold in storage.
const MaxClients = 500

// ErrTooManyClients means every one of MaxClients clients has a live grant.
var ErrTooManyClients = errors.New("auth: too many registered clients")

// RegisterClient stores a new public client. At the cap it first evicts the
// oldest clients with no live auth code, access token or refresh token, along
// with their dead rows. A client with a live grant is never evicted; if all of
// them have one, it returns ErrTooManyClients.
func (s *Storage) RegisterClient(ctx context.Context, c *fosite.DefaultClient, name string) error {
	if c.ID == "" {
		return errors.New("auth: register client: empty id")
	}
	if !c.Public || len(c.Secret) != 0 {
		return errors.New("auth: register client: only public clients are supported")
	}
	lists := make([][]byte, 4)
	for i, v := range [][]string{c.RedirectURIs, c.GrantTypes, c.ResponseTypes, c.Scopes} {
		if v == nil {
			v = []string{}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("auth: encode client: %w", err)
		}
		lists[i] = b
	}
	now := s.now()
	return s.inTx(ctx, func(ctx context.Context) error {
		if err := s.makeRoom(ctx, now); err != nil {
			return err
		}
		_, err := s.writer(ctx).ExecContext(ctx,
			`INSERT INTO oauth_clients (client_id, name, redirect_uris, grant_types, response_types, scopes, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.ID, name, string(lists[0]), string(lists[1]), string(lists[2]), string(lists[3]), formatTime(now))
		if err != nil {
			return fmt.Errorf("auth: register client: %w", err)
		}
		return nil
	})
}

// makeRoom leaves space for one more client, or returns ErrTooManyClients.
func (s *Storage) makeRoom(ctx context.Context, now time.Time) error {
	q := s.writer(ctx)
	var count int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM oauth_clients`).Scan(&count); err != nil {
		return fmt.Errorf("auth: count clients: %w", err)
	}
	excess := count - MaxClients + 1
	if excess <= 0 {
		return nil
	}
	res, err := q.ExecContext(ctx, `
		DELETE FROM oauth_clients WHERE client_id IN (
			SELECT c.client_id FROM oauth_clients c
			WHERE NOT EXISTS (SELECT 1 FROM oauth_auth_codes x
			                  WHERE x.client_id = c.client_id AND x.active = 1 AND x.expires_at > ?1)
			  AND NOT EXISTS (SELECT 1 FROM oauth_access_tokens x
			                  WHERE x.client_id = c.client_id AND x.expires_at > ?1)
			  AND NOT EXISTS (SELECT 1 FROM oauth_refresh_tokens x
			                  WHERE x.client_id = c.client_id AND x.active = 1 AND x.expires_at > ?1)
			ORDER BY c.created_at, c.rowid
			LIMIT ?2)`, formatTime(now), excess)
	if err != nil {
		return fmt.Errorf("auth: evict clients: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: evict clients: %w", err)
	}
	if n < excess {
		return ErrTooManyClients
	}
	return nil
}

// GetClient implements fosite's ClientManager.
func (s *Storage) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	var redirectURIs, grantTypes, responseTypes, scopes string
	err := s.reader(ctx).QueryRowContext(ctx,
		`SELECT redirect_uris, grant_types, response_types, scopes FROM oauth_clients WHERE client_id = ?`, id).
		Scan(&redirectURIs, &grantTypes, &responseTypes, &scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: get client: %w", err)
	}
	c := &fosite.DefaultClient{ID: id, Public: true}
	for _, f := range []struct {
		raw string
		dst *[]string
	}{{redirectURIs, &c.RedirectURIs}, {grantTypes, &c.GrantTypes}, {responseTypes, &c.ResponseTypes}, {scopes, &c.Scopes}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return nil, errors.New("auth: decode client: bad stored list")
		}
	}
	return c, nil
}

// ClientName returns the client_name given at registration, for the consent page.
func (s *Storage) ClientName(ctx context.Context, id string) (string, error) {
	var name string
	err := s.reader(ctx).QueryRowContext(ctx, `SELECT name FROM oauth_clients WHERE client_id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fosite.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("auth: get client name: %w", err)
	}
	return name, nil
}

// ClientAssertionJWTValid is part of fosite's ClientManager. Every client is
// public, so JWT client authentication isn't offered and any assertion is
// refused as if its JTI had been seen.
func (s *Storage) ClientAssertionJWTValid(context.Context, string) error {
	return fosite.ErrJTIKnown
}

// SetClientAssertionJWT refuses, as ClientAssertionJWTValid does.
func (s *Storage) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return fosite.ErrJTIKnown
}
