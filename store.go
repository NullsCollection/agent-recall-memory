package main

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Locked model + dim. Both are stamped on every row so a
// future model swap is `reindex`, not a migration.
const (
	embeddingModel = "qwen3-embedding:0.6b"
	embeddingDim   = 1024

	// Same-repo score multiplier. Tuned in Phase 2.
	repoBoost = 1.15

	metaLastIndexed = "last_indexed"
	metaLastAttempt = "last_attempt"
	metaLastError   = "last_error"
)

const schema = `
CREATE TABLE IF NOT EXISTS memories (
	id     TEXT PRIMARY KEY,
	text   TEXT NOT NULL,
	vector BLOB,
	dim    INTEGER,
	model  TEXT NOT NULL,
	cwd    TEXT,
	repo   TEXT,
	kind   TEXT,
	ts     INTEGER
);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT);
CREATE INDEX IF NOT EXISTS memories_by_repo  ON memories(repo);
CREATE INDEX IF NOT EXISTS memories_by_model ON memories(model);
`

// execer is satisfied by both *sql.DB and *sql.Tx, so upserts can run inside a
// transaction without duplicating the function.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// memory is one row. Vector may be nil for rows not yet embedded.
type memory struct {
	ID     string
	Text   string
	Vector []float32
	Cwd    string
	Repo   string
	Kind   string
	TS     int64
}

// dbPath returns ~/.claude/memdb.sqlite. Cross-platform: os.UserHomeDir works
// on Windows (%USERPROFILE%) and Linux ($HOME) alike.
func dbPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "memdb.sqlite"), nil
}

// openDB opens (creating the parent dir if needed) and ensures the schema.
// PRAGMAs are exec'd instead of stuffed in the DSN so Windows backslash paths
// don't collide with URI parsing.
func openDB() (*sql.DB, error) {
	p, err := dbPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return nil, err
	}
	// Single-writer CLI; a busy retry handles the rare concurrent hook+manual call.
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// upsert inserts or replaces a row by id. Deterministic ids make re-indexing
// idempotent.
func upsert(db execer, m memory) error {
	var vec []byte
	if m.Vector != nil {
		vec = encodeFloats(m.Vector)
	}
	_, err := db.Exec(`
		INSERT INTO memories (id, text, vector, dim, model, cwd, repo, kind, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			text   = excluded.text,
			vector = excluded.vector,
			dim    = excluded.dim,
			model  = excluded.model,
			cwd    = excluded.cwd,
			repo   = excluded.repo,
			kind   = excluded.kind,
			ts     = excluded.ts`,
		m.ID, m.Text, vec, len(m.Vector), embeddingModel, m.Cwd, m.Repo, m.Kind, m.TS)
	return err
}

// loadAll returns every row stamped with `model` that has a usable vector.
// The whole set fits in memory (~2 MB at 460 rows) — linear cosine scan follows.
func loadAll(db *sql.DB, model string) ([]memory, error) {
	rows, err := db.Query(`
		SELECT id, text, vector, cwd, repo, kind, ts
		FROM memories
		WHERE model = ? AND vector IS NOT NULL`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory
	for rows.Next() {
		var m memory
		var vec []byte
		if err := rows.Scan(&m.ID, &m.Text, &vec, &m.Cwd, &m.Repo, &m.Kind, &m.TS); err != nil {
			return nil, err
		}
		m.Vector = decodeFloats(vec)
		out = append(out, m)
	}
	return out, rows.Err()
}

// storedUnchanged reports whether id is already stored with this exact text
// and a vector on the current model, i.e. re-embedding it would be wasted work.
func storedUnchanged(db *sql.DB, id, text string) (bool, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE id = ? AND text = ? AND model = ? AND vector IS NOT NULL`,
		id, text, embeddingModel).Scan(&n)
	return n > 0, err
}

// setVector updates vector/dim/model for an existing row (used by reindex).
func setVector(db execer, id string, vec []float32) error {
	_, err := db.Exec(`
		UPDATE memories SET vector = ?, dim = ?, model = ? WHERE id = ?`,
		encodeFloats(vec), len(vec), embeddingModel, id)
	return err
}

// rowsNeedingEmbedding returns id+text for rows whose model isn't current.
// With force, every row qualifies.
func rowsNeedingEmbedding(db *sql.DB, force bool) (ids, texts []string, err error) {
	q := `SELECT id, text FROM memories`
	if !force {
		q += ` WHERE model != ?`
	}
	rows, err := db.Query(q, embeddingModel)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		texts = append(texts, text)
	}
	return ids, texts, rows.Err()
}

// --- float32 <-> BLOB (little-endian) ---

func encodeFloats(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeFloats(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// --- meta ---

func metaGet(db *sql.DB, key string) (string, error) {
	var v string
	err := db.QueryRow(`SELECT v FROM meta WHERE k = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func metaSet(db execer, key, value string) error {
	_, err := db.Exec(`
		INSERT INTO meta(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, value)
	return err
}
