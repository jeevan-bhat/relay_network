// Package store is the durable, crash-resilient command/result queue backed by
// SQLite. It is the persistence core of the agent: commands are enqueued as
// PENDING, atomically claimed for execution, and their results persisted for
// later sync back to the relay (Phase 3).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // pure-Go SQLite driver; registers the "sqlite" driver
)

// Command status values.
const (
	StatusPending   = "PENDING"
	StatusExecuting = "EXECUTING"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
)

// Payload is the JSON envelope stored in CommandQueue.Payload.
type Payload struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

// Command is a row from CommandQueue.
type Command struct {
	CommandID string
	Payload   string
	Status    string
	CreatedAt int64
	Attempts  int
}

// Decode parses the command's JSON payload.
func (c Command) Decode() (Payload, error) {
	var p Payload
	err := json.Unmarshal([]byte(c.Payload), &p)
	return p, err
}

// Result is a row from ResultQueue.
type Result struct {
	ResultID   string
	CommandID  string
	Stdout     string
	Stderr     string
	ExitCode   int
	ExecutedAt int64
	Synced     bool
}

// Store owns the SQLite connection and queue operations.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies pragmas,
// and runs migrations. The parent directory is created if missing.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %q: %w", dir, err)
		}
	}
	// modernc splits the DSN on the first '?', taking the left side verbatim as
	// the filename, so a Windows path ("C:\...") is not run through net/url.
	dsn := filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Serialize access within the process; cross-process safety comes from
	// SQLite file locking + busy_timeout.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS CommandQueue (
    CommandId TEXT PRIMARY KEY,
    Payload   TEXT NOT NULL,
    Status    TEXT NOT NULL CHECK(Status IN ('PENDING','EXECUTING','COMPLETED','FAILED')),
    CreatedAt INTEGER NOT NULL,
    Attempts  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cmd_status ON CommandQueue(Status, CreatedAt);

CREATE TABLE IF NOT EXISTS ResultQueue (
    ResultId   TEXT PRIMARY KEY,
    CommandId  TEXT NOT NULL,
    Stdout     TEXT,
    Stderr     TEXT,
    ExitCode   INTEGER,
    ExecutedAt INTEGER NOT NULL,
    Synced     INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(CommandId) REFERENCES CommandQueue(CommandId)
);
CREATE INDEX IF NOT EXISTS idx_result_synced ON ResultQueue(Synced);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Enqueue inserts a new PENDING command and returns its generated CommandId.
func (s *Store) Enqueue(p Payload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = s.db.Exec(
		`INSERT INTO CommandQueue (CommandId, Payload, Status, CreatedAt, Attempts)
		 VALUES (?, ?, ?, ?, 0)`,
		id, string(raw), StatusPending, time.Now().Unix())
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}
	return id, nil
}

// EnqueueWithID inserts a command with an existing ID (e.g. dispatched from Relay).
// If a command with this ID already exists, it is ignored.
func (s *Store) EnqueueWithID(id string, p Payload) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO CommandQueue (CommandId, Payload, Status, CreatedAt, Attempts)
		 VALUES (?, ?, ?, ?, 0)
		 ON CONFLICT(CommandId) DO NOTHING`,
		id, string(raw), StatusPending, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("enqueue with id: %w", err)
	}
	return nil
}

// ClaimNext atomically transitions the oldest eligible PENDING command
// (Attempts < maxAttempts) to EXECUTING, increments its attempt counter, and
// returns it. Returns (nil, nil) when there is nothing to claim.
func (s *Store) ClaimNext(maxAttempts int) (*Command, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var c Command
	err = tx.QueryRow(
		`SELECT CommandId, Payload, Status, CreatedAt, Attempts
		   FROM CommandQueue
		  WHERE Status = ? AND Attempts < ?
		  ORDER BY CreatedAt ASC, CommandId ASC
		  LIMIT 1`,
		StatusPending, maxAttempts).
		Scan(&c.CommandID, &c.Payload, &c.Status, &c.CreatedAt, &c.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	res, err := tx.Exec(
		`UPDATE CommandQueue SET Status = ?, Attempts = Attempts + 1
		  WHERE CommandId = ? AND Status = ?`,
		StatusExecuting, c.CommandID, StatusPending)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // lost a race with another claimer
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	c.Status = StatusExecuting
	c.Attempts++
	return &c, nil
}

// Complete sets a terminal status (COMPLETED or FAILED) for a command.
func (s *Store) Complete(commandID, status string) error {
	_, err := s.db.Exec(`UPDATE CommandQueue SET Status = ? WHERE CommandId = ?`, status, commandID)
	return err
}

// Requeue returns an EXECUTING command to PENDING for another attempt.
func (s *Store) Requeue(commandID string) error {
	_, err := s.db.Exec(`UPDATE CommandQueue SET Status = ? WHERE CommandId = ?`, StatusPending, commandID)
	return err
}

// SaveResult persists an execution result (Synced defaults to 0/false).
func (s *Store) SaveResult(r Result) error {
	if r.ResultID == "" {
		r.ResultID = uuid.NewString()
	}
	if r.ExecutedAt == 0 {
		r.ExecutedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(
		`INSERT INTO ResultQueue (ResultId, CommandId, Stdout, Stderr, ExitCode, ExecutedAt, Synced)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		r.ResultID, r.CommandID, r.Stdout, r.Stderr, r.ExitCode, r.ExecutedAt)
	if err != nil {
		return fmt.Errorf("save result: %w", err)
	}
	return nil
}

// RecoverStale is called at startup: any command left EXECUTING by a crash is
// returned to PENDING, unless it has exhausted its attempts, in which case it is
// marked FAILED. Returns the number of rows affected.
func (s *Store) RecoverStale(maxAttempts int) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE CommandQueue
		    SET Status = CASE WHEN Attempts >= ? THEN ? ELSE ? END
		  WHERE Status = ?`,
		maxAttempts, StatusFailed, StatusPending, StatusExecuting)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetCommand fetches a single command by id, or (nil, nil) if not found.
func (s *Store) GetCommand(id string) (*Command, error) {
	var c Command
	err := s.db.QueryRow(
		`SELECT CommandId, Payload, Status, CreatedAt, Attempts FROM CommandQueue WHERE CommandId = ?`, id).
		Scan(&c.CommandID, &c.Payload, &c.Status, &c.CreatedAt, &c.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCommands returns the most recent commands, newest first.
func (s *Store) ListCommands(limit int) ([]Command, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT CommandId, Payload, Status, CreatedAt, Attempts
		   FROM CommandQueue ORDER BY CreatedAt DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.CommandID, &c.Payload, &c.Status, &c.CreatedAt, &c.Attempts); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListResults returns results, optionally filtered to a single CommandId
// (empty = all), newest first.
func (s *Store) ListResults(commandID string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if commandID != "" {
		rows, err = s.db.Query(
			`SELECT ResultId, CommandId, Stdout, Stderr, ExitCode, ExecutedAt, Synced
			   FROM ResultQueue WHERE CommandId = ? ORDER BY ExecutedAt DESC LIMIT ?`,
			commandID, limit)
	} else {
		rows, err = s.db.Query(
			`SELECT ResultId, CommandId, Stdout, Stderr, ExitCode, ExecutedAt, Synced
			   FROM ResultQueue ORDER BY ExecutedAt DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		var synced int
		if err := rows.Scan(&r.ResultID, &r.CommandID, &r.Stdout, &r.Stderr, &r.ExitCode, &r.ExecutedAt, &synced); err != nil {
			return nil, err
		}
		r.Synced = synced != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUnsyncedResults returns all results that have Synced = 0 (false).
func (s *Store) ListUnsyncedResults(limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT ResultId, CommandId, Stdout, Stderr, ExitCode, ExecutedAt, Synced
		   FROM ResultQueue WHERE Synced = 0 ORDER BY ExecutedAt ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		var synced int
		if err := rows.Scan(&r.ResultID, &r.CommandID, &r.Stdout, &r.Stderr, &r.ExitCode, &r.ExecutedAt, &synced); err != nil {
			return nil, err
		}
		r.Synced = synced != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkResultSynced sets Synced = 1 for a CommandId.
func (s *Store) MarkResultSynced(commandID string) error {
	_, err := s.db.Exec(`UPDATE ResultQueue SET Synced = 1 WHERE CommandId = ?`, commandID)
	return err
}

