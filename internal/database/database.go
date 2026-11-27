// Package database opens the SQLite store and applies its migrations.
package database

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, keeps CGO_ENABLED=0 builds
)

// migrations run in order; PRAGMA user_version records how many have run.
var migrations = []string{
	`CREATE TABLE logs (
		id          INTEGER PRIMARY KEY,
		ts          INTEGER NOT NULL,           -- unix ms
		level       TEXT    NOT NULL,
		kind        TEXT    NOT NULL DEFAULT 'app',
		direction   TEXT,                        -- inbound | NULL (system/admin)
		msg         TEXT    NOT NULL,
		client      TEXT, method TEXT, host TEXT, url TEXT,
		status      INTEGER, bytes_in INTEGER, bytes_out INTEGER, duration_ms INTEGER,
		source_ip   TEXT, route TEXT, trace_id TEXT,
		attrs       TEXT,                        -- JSON object of remaining attributes
		rq_headers  TEXT, rq_body BLOB, rq_size INTEGER,   -- captured request (RQH, RQB)
		rs_headers  TEXT, rs_body BLOB, rs_size INTEGER    -- captured response (RSH, RSB)
	);
	CREATE INDEX logs_ts     ON logs(ts);
	CREATE INDEX logs_kind   ON logs(kind, ts);
	CREATE INDEX logs_route  ON logs(route, ts);
	CREATE INDEX logs_client ON logs(client, ts);

	-- Every change to an API's records or contract, from the API, the console
	-- or a hand edit of the file: who, when, and the record before and after.
	CREATE TABLE audit (
		id          INTEGER PRIMARY KEY,
		ts          INTEGER NOT NULL,           -- unix ms
		api         TEXT    NOT NULL,           -- "v1/holidays"
		record_id   TEXT,                        -- NULL for contract changes
		action      TEXT    NOT NULL,           -- create | replace | patch | delete | revert | import | api_create | api_update | api_delete
		actor       TEXT    NOT NULL,           -- console user, API credential, or "file"
		via         TEXT    NOT NULL,           -- api | console | file
		source_ip   TEXT,
		request_id  TEXT,
		before      TEXT,                        -- JSON, NULL when created
		after       TEXT                         -- JSON, NULL when deleted
	);
	CREATE INDEX audit_ts     ON audit(ts);
	CREATE INDEX audit_api    ON audit(api, ts);
	CREATE INDEX audit_record ON audit(api, record_id, ts);`,
}

// DB is the SQLite database.
type DB struct {
	*sql.DB
	path string
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate") // take the write lock at BEGIN: no lock-upgrade deadlocks
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) and migrates the database at path.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	sdb, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(8)
	db := &DB{DB: sdb, path: path}
	if err := db.migrate(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return db, nil
}

// OpenReadOnly is used by CLI queries while the server is running.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	sdb, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	return &DB{DB: sdb, path: path}, sdb.Ping()
}

// Path is the database file.
func (db *DB) Path() string { return db.path }

func (db *DB) migrate() error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// NowMs is the current time in unix milliseconds.
func NowMs() int64 { return time.Now().UnixMilli() }

// NewID returns a time-sortable id: 12 hex chars of unix ms + 8 random hex chars.
func NewID() string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("%012x%s", NowMs(), hex.EncodeToString(b[:]))
}
