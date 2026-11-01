package backup

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const fakeToken = "fake-turso-token-s3cr3t"

// fakeTurso speaks Hrana's HTTP pipeline over a real SQLite file. It enforces
// the bearer token and baton rules, and closing a stream rolls back an open
// transaction.
type fakeTurso struct {
	t     *testing.T
	srv   *httptest.Server
	db    *sql.DB
	token string

	mu     sync.Mutex
	conn   *sql.Conn // the open stream's connection
	baton  string
	nextID int

	// failInsert, if positive, fails the nth INSERT with a statement error,
	// whose message is failMessage if set.
	failInsert  int
	failMessage string
	inserts     int
	// loseStreamOn, if set, closes the stream (rolling back) after a statement
	// it matches and answers with a null baton.
	loseStreamOn func(query string) bool
	// executed records every statement run, in order.
	executed []string
	// failHTTP, if set, answers every request with a 500 quoting the auth header.
	failHTTP bool
	// corrupt, if set, rewrites blobs on their way out.
	corrupt func([]byte) []byte
}

func newFakeTurso(t *testing.T) *fakeTurso {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "turso.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTurso{t: t, db: db, token: fakeToken}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() {
		f.srv.Close()
		if f.conn != nil {
			f.conn.Close()
		}
		db.Close()
	})
	return f
}

func (f *fakeTurso) client(t *testing.T) *Turso {
	t.Helper()
	c, err := NewTurso(f.srv.URL, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeTurso) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failHTTP {
		http.Error(w, "boom at "+f.srv.URL+" with "+r.Header.Get("Authorization"), http.StatusInternalServerError)
		return
	}
	if r.URL.Path != "/v2/pipeline" || r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Baton    *string `json:"baton"`
		Requests []struct {
			Type string `json:"type"`
			Stmt *struct {
				SQL  string  `json:"sql"`
				Args []value `json:"args"`
			} `json:"stmt"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	if req.Baton != nil && (*req.Baton != f.baton || f.conn == nil) {
		http.Error(w, "bad baton", http.StatusBadRequest)
		return
	}
	if req.Baton == nil {
		f.closeStream()
		conn, err := f.db.Conn(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.conn = conn
	}
	results := []map[string]any{}
	closed := false
	for _, sr := range req.Requests {
		switch sr.Type {
		case "close":
			f.closeStream()
			closed = true
			results = append(results, map[string]any{"type": "ok", "response": map[string]any{"type": "close"}})
		case "execute":
			f.executed = append(f.executed, sr.Stmt.SQL)
			rows, err := f.execute(ctx, sr.Stmt.SQL, sr.Stmt.Args)
			if f.loseStreamOn != nil && f.loseStreamOn(sr.Stmt.SQL) {
				f.closeStream()
				closed = true
			}
			if err != nil {
				results = append(results, map[string]any{"type": "error", "error": map[string]any{"message": err.Error()}})
				continue
			}
			results = append(results, map[string]any{"type": "ok", "response": map[string]any{
				"type": "execute", "result": map[string]any{"rows": rows},
			}})
		default:
			http.Error(w, "bad request type", http.StatusBadRequest)
			return
		}
	}
	resp := map[string]any{"baton": nil, "base_url": nil, "results": results}
	if !closed {
		f.nextID++
		f.baton = "baton-" + strconv.Itoa(f.nextID)
		resp["baton"] = f.baton
	}
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeTurso) closeStream() {
	if f.conn != nil {
		f.conn.ExecContext(context.Background(), "ROLLBACK") // fails harmlessly with no transaction open
		f.conn.Close()
		f.conn = nil
	}
}

func (f *fakeTurso) execute(ctx context.Context, query string, args []value) ([][]map[string]any, error) {
	if strings.HasPrefix(query, "INSERT") {
		f.inserts++
		if f.inserts == f.failInsert {
			if f.failMessage != "" {
				return nil, errors.New(f.failMessage)
			}
			return nil, errors.New("disk I/O error")
		}
	}
	bound := make([]any, len(args))
	for i, a := range args {
		switch a.Type {
		case "integer":
			n, err := a.integer()
			if err != nil {
				return nil, err
			}
			bound[i] = n
		case "text":
			bound[i] = a.text()
		case "blob":
			b, err := a.blob()
			if err != nil {
				return nil, err
			}
			if b == nil {
				b = []byte{}
			}
			bound[i] = b
		default:
			return nil, fmt.Errorf("bad arg type %q", a.Type)
		}
	}
	rows, err := f.conn.QueryContext(ctx, query, bound...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := [][]map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]map[string]any, len(cols))
		for i, v := range vals {
			switch v := v.(type) {
			case int64:
				row[i] = map[string]any{"type": "integer", "value": strconv.FormatInt(v, 10)}
			case string:
				row[i] = map[string]any{"type": "text", "value": v}
			case []byte:
				if f.corrupt != nil {
					v = f.corrupt(v)
				}
				row[i] = map[string]any{"type": "blob", "base64": base64.RawStdEncoding.EncodeToString(v)}
			case nil:
				row[i] = map[string]any{"type": "null"}
			default:
				return nil, fmt.Errorf("bad column type %T", v)
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// rowCount reads the stored chunk rows directly.
func (f *fakeTurso) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM backups").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fakeTurso) streamOpen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conn != nil
}

func (f *fakeTurso) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.executed...)
}
