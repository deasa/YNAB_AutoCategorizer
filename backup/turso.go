package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// requestTimeout bounds one HTTP round trip to Turso.
	requestTimeout = 60 * time.Second
	// maxResponse caps a response body: one base64 chunk plus JSON framing.
	maxResponse = 16 << 20
	// errBodyLimit caps how much of a server error reaches an error message.
	errBodyLimit = 200
	// errBodyRead caps how much of an error body is read. It is far above
	// errBodyLimit so that a secret is scrubbed whole before the text is cut.
	errBodyRead = 64 << 10
)

const createTable = `CREATE TABLE IF NOT EXISTS backups (
	id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	chunk INTEGER NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY (id, chunk)
)`

// Info describes one stored backup. Size and SHA256 are the ciphertext's.
type Info struct {
	ID        string
	CreatedAt string
	Size      int64
	SHA256    string
	Chunks    int
}

// Turso stores backups in a Turso database over Hrana's HTTP pipeline. It
// ignores a response's base_url: Turso sends none, and a stream that needed
// one would fail and roll back rather than store a partial backup.
//
// Every error it returns is scrubbed of the token and the database URL.
type Turso struct {
	endpoint string
	token    string
	secrets  []string
	client   *http.Client
}

// NewTurso returns a client for rawURL (libsql://, https:// or http://).
func NewTurso(rawURL, token string) (*Turso, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("turso: database URL is not valid")
	}
	switch u.Scheme {
	case "libsql":
		u.Scheme = "https"
	case "https", "http":
	default:
		return nil, errors.New("turso: database URL must be libsql://, https:// or http://")
	}
	secrets := []string{token, rawURL, u.Host}
	if u.User != nil {
		secrets = append(secrets, u.User.String())
	}
	if u.RawQuery != "" {
		secrets = append(secrets, u.RawQuery)
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	endpoint := strings.TrimRight(u.String(), "/") + "/v2/pipeline"
	return &Turso{
		endpoint: endpoint,
		token:    token,
		secrets:  append(secrets, endpoint),
		client:   &http.Client{Timeout: requestTimeout},
	}, nil
}

// scrub removes the token and URL parts from s.
func (t *Turso) scrub(s string) string {
	for _, secret := range t.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<secret>")
		}
	}
	return s
}

// errText scrubs server-supplied text, then cuts it to errBodyLimit. The order
// matters: a secret cut in two would no longer match and its prefix would leak.
func (t *Turso) errText(s string) string {
	s = t.scrub(s)
	if len(s) > errBodyLimit {
		s = s[:errBodyLimit]
	}
	return s
}

// clean rebuilds err as a scrubbed message. net/http errors quote the URL, so
// only the cause of a *url.Error is kept.
func (t *Turso) clean(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	return errors.New("turso: " + t.scrub(err.Error()))
}

type value struct {
	Type   string  `json:"type"`
	Value  any     `json:"value"`
	Base64 *string `json:"base64"`
}

func (v value) text() string {
	s, _ := v.Value.(string)
	return s
}

// integer reads a Hrana integer, which JSON carries as a string.
func (v value) integer() (int64, error) {
	n, err := strconv.ParseInt(v.text(), 10, 64)
	if err != nil {
		return 0, errors.New("response has a malformed integer")
	}
	return n, nil
}

func (v value) blob() ([]byte, error) {
	if v.Base64 == nil {
		return nil, nil // a zero-length blob can come back as null
	}
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(*v.Base64, "="))
	if err != nil {
		return nil, errors.New("response has a malformed blob")
	}
	return data, nil
}

func textArg(s string) map[string]any { return map[string]any{"type": "text", "value": s} }

func intArg(n int64) map[string]any {
	return map[string]any{"type": "integer", "value": strconv.FormatInt(n, 10)}
}

func blobArg(b []byte) map[string]any {
	return map[string]any{"type": "blob", "base64": base64.RawStdEncoding.EncodeToString(b)}
}

type stmt struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"`
}

func newStmt(sql string, args ...any) stmt {
	if args == nil {
		args = []any{}
	}
	return stmt{SQL: sql, Args: args}
}

type streamRequest struct {
	Type string `json:"type"`
	Stmt *stmt  `json:"stmt,omitempty"`
}

type pipelineResponse struct {
	Baton   *string `json:"baton"`
	Results []struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Response struct {
			Result struct {
				Rows [][]value `json:"rows"`
			} `json:"result"`
		} `json:"response"`
	} `json:"results"`
}

// pipeline sends stmts on the stream named by baton (nil opens a new one) and
// returns each statement's rows and the stream's next baton. With closeStream
// the stream ends after stmts. The server runs every request in a pipeline
// even after one fails, so a statement that must not follow a failure has to
// go in a later call.
//
// The returned baton is always the latest known, errors included: the server's
// new one when it answered, otherwise the one passed in. A nil baton means the
// server closed the stream, and passing it back would open a new stream.
func (t *Turso) pipeline(ctx context.Context, baton *string, closeStream bool, stmts ...stmt) ([][][]value, *string, error) {
	reqs := make([]streamRequest, 0, len(stmts)+1)
	for i := range stmts {
		reqs = append(reqs, streamRequest{Type: "execute", Stmt: &stmts[i]})
	}
	if closeStream {
		reqs = append(reqs, streamRequest{Type: "close"})
	}
	body, err := json.Marshal(map[string]any{"baton": baton, "requests": reqs})
	if err != nil {
		return nil, baton, t.clean(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, baton, t.clean(err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, baton, t.clean(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyRead))
		return nil, baton, t.clean(fmt.Errorf("status %d: %s", resp.StatusCode, t.errText(string(msg))))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, baton, t.clean(err)
	}
	if len(raw) > maxResponse {
		return nil, baton, t.clean(errors.New("response is too large"))
	}
	var out pipelineResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, baton, t.clean(errors.New("response is not valid JSON"))
	}
	if len(out.Results) != len(reqs) {
		return nil, out.Baton, t.clean(fmt.Errorf("got %d results for %d requests", len(out.Results), len(reqs)))
	}
	rows := make([][][]value, len(stmts))
	for i, r := range out.Results {
		if r.Type != "ok" {
			msg := "request failed"
			if r.Error != nil {
				msg = t.errText(r.Error.Message)
			}
			return nil, out.Baton, t.clean(errors.New(msg))
		}
		if i < len(stmts) {
			rows[i] = r.Response.Result.Rows
		}
	}
	return rows, out.Baton, nil
}

// exec runs stmts on a stream of their own.
func (t *Turso) exec(ctx context.Context, stmts ...stmt) ([][][]value, error) {
	rows, _, err := t.pipeline(ctx, nil, true, stmts...)
	return rows, err
}

// Put stores r as one backup, split into chunkSize chunks, in one transaction:
// either every chunk commits or none does. It returns the chunk count. An empty
// r stores one empty chunk, so the backup still has a row.
func (t *Turso) Put(ctx context.Context, info Info, r io.Reader, chunkSize int) (int, error) {
	if _, err := t.exec(ctx, newStmt(createTable)); err != nil {
		return 0, err
	}
	_, baton, err := t.pipeline(ctx, nil, false, newStmt("BEGIN IMMEDIATE"))
	committed := false
	defer func() {
		if committed || baton == nil {
			return
		}
		// Closing the stream rolls back. A background context so a cancelled
		// ctx can't skip it; if this fails too, Turso rolls back the idle stream.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = t.pipeline(closeCtx, baton, true)
	}()
	if err != nil {
		return 0, err
	}
	if err := t.requireStream(baton); err != nil {
		return 0, err
	}

	n, err := eachChunk(r, chunkSize, func(i int, chunk []byte) error {
		var err error
		_, baton, err = t.pipeline(ctx, baton, false, newStmt(
			"INSERT INTO backups (id, created_at, size, sha256, chunk, data) VALUES (?, ?, ?, ?, ?, ?)",
			textArg(info.ID), textArg(info.CreatedAt), intArg(info.Size), textArg(info.SHA256), intArg(int64(i)), blobArg(chunk)))
		if err != nil {
			return err
		}
		return t.requireStream(baton)
	})
	if err != nil {
		return 0, err
	}
	if _, baton, err = t.pipeline(ctx, baton, true, newStmt("COMMIT")); err != nil {
		return 0, err
	}
	committed = true
	return n, nil
}

// requireStream fails if the server closed the stream mid-transaction, which
// rolled the transaction back. Carrying on would open a new stream, where the
// remaining chunks would each autocommit and leave a partial backup.
func (t *Turso) requireStream(baton *string) error {
	if baton == nil || *baton == "" {
		return t.clean(errors.New("stream closed during the transaction"))
	}
	return nil
}

// eachChunk calls fn with each size-byte piece of r (the last may be shorter)
// and returns how many it passed. An empty r yields one empty chunk.
func eachChunk(r io.Reader, size int, fn func(i int, chunk []byte) error) (int, error) {
	buf := make([]byte, size)
	for n := 0; ; n++ {
		m, err := io.ReadFull(r, buf)
		switch {
		case errors.Is(err, io.EOF):
			if n > 0 {
				return n, nil
			}
			if err := fn(0, buf[:0]); err != nil {
				return 0, err
			}
			return 1, nil
		case errors.Is(err, io.ErrUnexpectedEOF):
			if err := fn(n, buf[:m]); err != nil {
				return n, err
			}
			return n + 1, nil
		case err != nil:
			return n, err
		}
		if err := fn(n, buf); err != nil {
			return n, err
		}
	}
}

// List returns every backup, newest first, without reading any data.
func (t *Turso) List(ctx context.Context) ([]Info, error) {
	rows, err := t.exec(ctx, newStmt(createTable), newStmt(
		`SELECT id, MAX(created_at), MAX(size), MAX(sha256), COUNT(*) FROM backups
		 GROUP BY id ORDER BY MAX(created_at) DESC, id DESC`))
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(rows[1]))
	for _, row := range rows[1] {
		if len(row) != 5 {
			return nil, t.clean(errors.New("list: unexpected columns"))
		}
		size, err := row[2].integer()
		if err != nil {
			return nil, t.clean(err)
		}
		chunks, err := row[4].integer()
		if err != nil {
			return nil, t.clean(err)
		}
		infos = append(infos, Info{ID: row[0].text(), CreatedAt: row[1].text(), Size: size, SHA256: row[3].text(), Chunks: int(chunks)})
	}
	return infos, nil
}

// Fetch writes info's chunks to w in order, one request per chunk.
func (t *Turso) Fetch(ctx context.Context, info Info, w io.Writer) error {
	for i := range info.Chunks {
		rows, err := t.exec(ctx, newStmt("SELECT data FROM backups WHERE id = ? AND chunk = ?", textArg(info.ID), intArg(int64(i))))
		if err != nil {
			return err
		}
		if len(rows[0]) != 1 || len(rows[0][0]) != 1 {
			return t.clean(fmt.Errorf("backup %s: chunk %d is missing", info.ID, i))
		}
		data, err := rows[0][0][0].blob()
		if err != nil {
			return t.clean(err)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("backup %s: write chunk %d: %w", info.ID, i, err)
		}
	}
	return nil
}

// Prune deletes all but the newest keep backups, in one statement.
func (t *Turso) Prune(ctx context.Context, keep int) error {
	_, err := t.exec(ctx, newStmt(
		`DELETE FROM backups WHERE id NOT IN (
			SELECT id FROM backups GROUP BY id ORDER BY MAX(created_at) DESC, id DESC LIMIT ?
		)`, intArg(int64(keep))))
	return err
}
