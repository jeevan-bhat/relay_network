// Package store provides SQLite-backed persistent storage for the Relay server.
// It manages registered devices, cloud offline command queues, and historical results.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"terminalrelay/internal/protocol"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver
)

// Command statuses in the relay cloud queue.
const (
	StatusPending    = "PENDING"
	StatusDispatched = "DISPATCHED"
	StatusCompleted  = "COMPLETED"
	StatusFailed     = "FAILED"
)

// CommandRecord represents a command tracked in the Relay.
type CommandRecord struct {
	CommandID    string
	DeviceID     string
	Command      string
	TimeoutSec   int
	Status       string
	CreatedAt    int64
	DispatchedAt int64
	CompletedAt  int64
}

// Store owns the SQLite connection for the relay server.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the relay SQLite database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %q: %w", dir, err)
		}
	}
	dsn := filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
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

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS Devices (
    DeviceId      TEXT PRIMARY KEY,
    Hostname      TEXT,
    OS            TEXT,
    Status        TEXT NOT NULL DEFAULT 'OFFLINE',
    LastHeartbeat INTEGER NOT NULL DEFAULT 0,
    ConnectedAt   INTEGER NOT NULL DEFAULT 0,
    HealthJSON    TEXT
);

CREATE TABLE IF NOT EXISTS CloudCommandQueue (
    CommandId    TEXT PRIMARY KEY,
    DeviceId     TEXT NOT NULL,
    Command      TEXT NOT NULL,
    TimeoutSec   INTEGER DEFAULT 0,
    Status       TEXT NOT NULL CHECK(Status IN ('PENDING','DISPATCHED','COMPLETED','FAILED')),
    CreatedAt    INTEGER NOT NULL,
    DispatchedAt INTEGER DEFAULT 0,
    CompletedAt  INTEGER DEFAULT 0,
    FOREIGN KEY(DeviceId) REFERENCES Devices(DeviceId)
);
CREATE INDEX IF NOT EXISTS idx_cloud_cmd ON CloudCommandQueue(DeviceId, Status, CreatedAt);

CREATE TABLE IF NOT EXISTS CloudResultQueue (
    ResultId   TEXT PRIMARY KEY,
    CommandId  TEXT NOT NULL,
    DeviceId   TEXT NOT NULL,
    Stdout     TEXT,
    Stderr     TEXT,
    ExitCode   INTEGER,
    ExecutedAt INTEGER NOT NULL,
    ReceivedAt INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cloud_res_cmd ON CloudResultQueue(CommandId);
CREATE INDEX IF NOT EXISTS idx_cloud_res_dev ON CloudResultQueue(DeviceId, ExecutedAt DESC);

CREATE TABLE IF NOT EXISTS AuditLogs (
    LogId       TEXT PRIMARY KEY,
    DeviceId    TEXT NOT NULL,
    Timestamp   INTEGER NOT NULL,
    ActionType  TEXT NOT NULL,
    CommandText TEXT,
    ExitCode    INTEGER DEFAULT 0,
    DurationMs  INTEGER DEFAULT 0,
    Details     TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_dev ON AuditLogs(DeviceId, Timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_time ON AuditLogs(Timestamp DESC);
`

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

// UpsertDevice inserts or updates a device registration.
func (s *Store) UpsertDevice(info protocol.DeviceInfo) error {
	healthBytes, _ := json.Marshal(info.Metrics)
	_, err := s.db.Exec(`
		INSERT INTO Devices (DeviceId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(DeviceId) DO UPDATE SET
			Hostname = excluded.Hostname,
			OS = excluded.OS,
			Status = excluded.Status,
			LastHeartbeat = excluded.LastHeartbeat,
			ConnectedAt = excluded.ConnectedAt,
			HealthJSON = excluded.HealthJSON
	`, info.DeviceID, info.Hostname, info.OS, info.Status, info.LastHeartbeat, info.ConnectedAt, string(healthBytes))
	return err
}

// UpdateDeviceHeartbeat updates heartbeat time, telemetry metrics, and status for a device.
func (s *Store) UpdateDeviceHeartbeat(deviceID string, metrics protocol.HealthMetrics, status string) error {
	healthBytes, _ := json.Marshal(metrics)
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE Devices
		   SET LastHeartbeat = ?, HealthJSON = ?, Status = ?
		 WHERE DeviceId = ?
	`, now, string(healthBytes), status, deviceID)
	return err
}

// UpdateDeviceStatus updates the connection status (ONLINE, DEGRADED, OFFLINE) of a device.
func (s *Store) UpdateDeviceStatus(deviceID string, status string) error {
	_, err := s.db.Exec(`UPDATE Devices SET Status = ? WHERE DeviceId = ?`, status, deviceID)
	return err
}

// GetDevice retrieves a device by DeviceID.
func (s *Store) GetDevice(deviceID string) (*protocol.DeviceInfo, error) {
	var (
		d           protocol.DeviceInfo
		healthStr   sql.NullString
		lastHB      int64
		connectedAt int64
	)
	err := s.db.QueryRow(`
		SELECT DeviceId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON
		  FROM Devices WHERE DeviceId = ?
	`, deviceID).Scan(&d.DeviceID, &d.Hostname, &d.OS, &d.Status, &lastHB, &connectedAt, &healthStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.LastHeartbeat = lastHB
	d.ConnectedAt = connectedAt
	if healthStr.Valid && healthStr.String != "" {
		_ = json.Unmarshal([]byte(healthStr.String), &d.Metrics)
	}
	return &d, nil
}

// ListDevices returns all registered devices.
func (s *Store) ListDevices() ([]protocol.DeviceInfo, error) {
	rows, err := s.db.Query(`
		SELECT DeviceId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON
		  FROM Devices ORDER BY LastHeartbeat DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []protocol.DeviceInfo
	for rows.Next() {
		var (
			d         protocol.DeviceInfo
			healthStr sql.NullString
		)
		if err := rows.Scan(&d.DeviceID, &d.Hostname, &d.OS, &d.Status, &d.LastHeartbeat, &d.ConnectedAt, &healthStr); err != nil {
			return nil, err
		}
		if healthStr.Valid && healthStr.String != "" {
			_ = json.Unmarshal([]byte(healthStr.String), &d.Metrics)
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// EnqueueCommand stores a new command for a device with Status = PENDING.
func (s *Store) EnqueueCommand(cmd protocol.CommandPayload) error {
	now := cmd.CreatedAt
	if now == 0 {
		now = time.Now().Unix()
	}
	// Ensure device exists
	_, _ = s.db.Exec(`INSERT OR IGNORE INTO Devices (DeviceId, Status) VALUES (?, 'OFFLINE')`, cmd.DeviceID)

	_, err := s.db.Exec(`
		INSERT INTO CloudCommandQueue (CommandId, DeviceId, Command, TimeoutSec, Status, CreatedAt)
		VALUES (?, ?, ?, ?, ?, ?)
	`, cmd.CommandID, cmd.DeviceID, cmd.Command, cmd.TimeoutSec, StatusPending, now)
	return err
}

// GetPendingCommands returns all PENDING commands for a device in FIFO order.
func (s *Store) GetPendingCommands(deviceID string) ([]protocol.CommandPayload, error) {
	rows, err := s.db.Query(`
		SELECT CommandId, DeviceId, Command, TimeoutSec, CreatedAt
		  FROM CloudCommandQueue
		 WHERE DeviceId = ? AND Status = ?
		 ORDER BY CreatedAt ASC, CommandId ASC
	`, deviceID, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cmds []protocol.CommandPayload
	for rows.Next() {
		var c protocol.CommandPayload
		if err := rows.Scan(&c.CommandID, &c.DeviceID, &c.Command, &c.TimeoutSec, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

// CancelPendingCommands marks all PENDING commands for a device as FAILED/CANCELLED.
func (s *Store) CancelPendingCommands(deviceID string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE CloudCommandQueue
		   SET Status = ?, CompletedAt = ?
		 WHERE DeviceId = ? AND Status = ?
	`, StatusFailed, now, deviceID, StatusPending)
	return err
}

// MarkCommandDispatched sets a command's status to DISPATCHED.
func (s *Store) MarkCommandDispatched(commandID string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE CloudCommandQueue
		   SET Status = ?, DispatchedAt = ?
		 WHERE CommandId = ? AND Status = ?
	`, StatusDispatched, now, commandID, StatusPending)
	return err
}

// MarkCommandCompleted marks a command as COMPLETED or FAILED.
func (s *Store) MarkCommandCompleted(commandID string, status string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE CloudCommandQueue
		   SET Status = ?, CompletedAt = ?
		 WHERE CommandId = ?
	`, status, now, commandID)
	return err
}

// GetCommand retrieves a command record by CommandID.
func (s *Store) GetCommand(commandID string) (*CommandRecord, error) {
	var c CommandRecord
	err := s.db.QueryRow(`
		SELECT CommandId, DeviceId, Command, TimeoutSec, Status, CreatedAt, DispatchedAt, CompletedAt
		  FROM CloudCommandQueue WHERE CommandId = ?
	`, commandID).Scan(&c.CommandID, &c.DeviceID, &c.Command, &c.TimeoutSec, &c.Status, &c.CreatedAt, &c.DispatchedAt, &c.CompletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCommands returns recent commands for a device (or all devices if deviceID is empty).
func (s *Store) ListCommands(deviceID string, limit int) ([]CommandRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if deviceID != "" {
		rows, err = s.db.Query(`
			SELECT CommandId, DeviceId, Command, TimeoutSec, Status, CreatedAt, DispatchedAt, CompletedAt
			  FROM CloudCommandQueue WHERE DeviceId = ? ORDER BY CreatedAt DESC LIMIT ?
		`, deviceID, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT CommandId, DeviceId, Command, TimeoutSec, Status, CreatedAt, DispatchedAt, CompletedAt
			  FROM CloudCommandQueue ORDER BY CreatedAt DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []CommandRecord
	for rows.Next() {
		var c CommandRecord
		if err := rows.Scan(&c.CommandID, &c.DeviceID, &c.Command, &c.TimeoutSec, &c.Status, &c.CreatedAt, &c.DispatchedAt, &c.CompletedAt); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

// SaveResult saves an execution result and updates the command status.
func (s *Store) SaveResult(res protocol.ResultPayload) error {
	now := time.Now().Unix()
	if res.ExecutedAt == 0 {
		res.ExecutedAt = now
	}
	if res.ResultID == "" {
		res.ResultID = res.CommandID
	}
	_, err := s.db.Exec(`
		INSERT INTO CloudResultQueue (ResultId, CommandId, DeviceId, Stdout, Stderr, ExitCode, ExecutedAt, ReceivedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ResultId) DO UPDATE SET
			Stdout = excluded.Stdout,
			Stderr = excluded.Stderr,
			ExitCode = excluded.ExitCode,
			ExecutedAt = excluded.ExecutedAt,
			ReceivedAt = excluded.ReceivedAt
	`, res.ResultID, res.CommandID, res.DeviceID, res.Stdout, res.Stderr, res.ExitCode, res.ExecutedAt, now)
	if err != nil {
		return err
	}

	status := StatusCompleted
	if res.ExitCode != 0 || res.TimedOut {
		status = StatusFailed
	}
	_ = s.MarkCommandCompleted(res.CommandID, status)
	return nil
}

// SaveSandboxResult saves a result to CloudResultQueue without changing CloudCommandQueue status from PENDING.
func (s *Store) SaveSandboxResult(res protocol.ResultPayload) error {
	now := time.Now().Unix()
	if res.ExecutedAt == 0 {
		res.ExecutedAt = now
	}
	if res.ResultID == "" {
		res.ResultID = res.CommandID
	}
	_, err := s.db.Exec(`
		INSERT INTO CloudResultQueue (ResultId, CommandId, DeviceId, Stdout, Stderr, ExitCode, ExecutedAt, ReceivedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ResultId) DO UPDATE SET
			Stdout = excluded.Stdout,
			Stderr = excluded.Stderr,
			ExitCode = excluded.ExitCode,
			ExecutedAt = excluded.ExecutedAt,
			ReceivedAt = excluded.ReceivedAt
	`, res.ResultID, res.CommandID, res.DeviceID, res.Stdout, res.Stderr, res.ExitCode, res.ExecutedAt, now)
	return err
}

// GetResult retrieves the execution result for a CommandID.
func (s *Store) GetResult(commandID string) (*protocol.ResultPayload, error) {
	var r protocol.ResultPayload
	err := s.db.QueryRow(`
		SELECT ResultId, CommandId, DeviceId, Stdout, Stderr, ExitCode, ExecutedAt
		  FROM CloudResultQueue WHERE CommandId = ?
	`, commandID).Scan(&r.ResultID, &r.CommandID, &r.DeviceID, &r.Stdout, &r.Stderr, &r.ExitCode, &r.ExecutedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListResults returns historical results for a device (or all devices if deviceID is empty).
func (s *Store) ListResults(deviceID string, limit int) ([]protocol.ResultPayload, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if deviceID != "" {
		rows, err = s.db.Query(`
			SELECT ResultId, CommandId, DeviceId, Stdout, Stderr, ExitCode, ExecutedAt
			  FROM CloudResultQueue WHERE DeviceId = ? ORDER BY ExecutedAt DESC LIMIT ?
		`, deviceID, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT ResultId, CommandId, DeviceId, Stdout, Stderr, ExitCode, ExecutedAt
			  FROM CloudResultQueue ORDER BY ExecutedAt DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []protocol.ResultPayload
	for rows.Next() {
		var r protocol.ResultPayload
		if err := rows.Scan(&r.ResultID, &r.CommandID, &r.DeviceID, &r.Stdout, &r.Stderr, &r.ExitCode, &r.ExecutedAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// RecordAuditLog inserts an immutable audit log entry.
func (s *Store) RecordAuditLog(log protocol.AuditLogRecord) error {
	if log.Timestamp == 0 {
		log.Timestamp = time.Now().Unix()
	}
	_, err := s.db.Exec(`
		INSERT INTO AuditLogs (LogId, DeviceId, Timestamp, ActionType, CommandText, ExitCode, DurationMs, Details)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, log.LogID, log.DeviceID, log.Timestamp, log.ActionType, log.CommandText, log.ExitCode, log.DurationMs, log.Details)
	return err
}

// ListAuditLogs returns recent audit log entries, newest first.
func (s *Store) ListAuditLogs(deviceID string, limit int) ([]protocol.AuditLogRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if deviceID != "" {
		rows, err = s.db.Query(`
			SELECT LogId, DeviceId, Timestamp, ActionType, CommandText, ExitCode, DurationMs, Details
			  FROM AuditLogs WHERE DeviceId = ? ORDER BY Timestamp DESC LIMIT ?
		`, deviceID, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT LogId, DeviceId, Timestamp, ActionType, CommandText, ExitCode, DurationMs, Details
			  FROM AuditLogs ORDER BY Timestamp DESC LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []protocol.AuditLogRecord
	for rows.Next() {
		var l protocol.AuditLogRecord
		var cmdText, details sql.NullString
		if err := rows.Scan(&l.LogID, &l.DeviceID, &l.Timestamp, &l.ActionType, &cmdText, &l.ExitCode, &l.DurationMs, &details); err != nil {
			return nil, err
		}
		if cmdText.Valid {
			l.CommandText = cmdText.String
		}
		if details.Valid {
			l.Details = details.String
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

