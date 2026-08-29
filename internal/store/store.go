// Package store provides Supabase cloud-backed and SQLite persistent storage for the Relay server.
// It manages registered devices, cloud offline command queues, and historical results.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
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

// Store owns the persistence connection for the relay server (SQLite or Supabase Cloud).
type Store struct {
	db       *sql.DB
	supabase *SupabaseStore
}

// Open opens (or creates) the relay database. If SUPABASE_URL and SUPABASE_KEY are set,
// it uses Supabase Cloud directly. Otherwise, it uses local SQLite.
func Open(path string) (*Store, error) {
	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_KEY")
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	}
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_ANON_KEY")
	}

	if supabaseURL != "" && supabaseKey != "" {
		sb, err := NewSupabaseStore(supabaseURL, supabaseKey)
		if err == nil {
			return &Store{supabase: sb}, nil
		}
	}

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

// OpenWithConfig opens the store with explicit Supabase or SQLite parameters.
func OpenWithConfig(path, supabaseURL, supabaseKey string) (*Store, error) {
	if supabaseURL != "" && supabaseKey != "" {
		sb, err := NewSupabaseStore(supabaseURL, supabaseKey)
		if err == nil {
			return &Store{supabase: sb}, nil
		}
	}
	return Open(path)
}

// IsSupabase returns true if the store is backed by Supabase Cloud.
func (s *Store) IsSupabase() bool {
	return s.supabase != nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS Users (
    UserId       TEXT PRIMARY KEY,
    Username     TEXT UNIQUE NOT NULL,
    Password     TEXT NOT NULL,
    AuthToken    TEXT UNIQUE NOT NULL,
    CreatedAt    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_token ON Users(AuthToken);

CREATE TABLE IF NOT EXISTS Devices (
    DeviceId      TEXT PRIMARY KEY,
    UserId        TEXT NOT NULL DEFAULT '',
    Hostname      TEXT,
    OS            TEXT,
    Status        TEXT NOT NULL DEFAULT 'OFFLINE',
    LastHeartbeat INTEGER NOT NULL DEFAULT 0,
    ConnectedAt   INTEGER NOT NULL DEFAULT 0,
    HealthJSON    TEXT
);
CREATE INDEX IF NOT EXISTS idx_dev_user ON Devices(UserId);

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

CREATE TABLE IF NOT EXISTS TelemetryHistory (
    Id             INTEGER PRIMARY KEY AUTOINCREMENT,
    DeviceId       TEXT NOT NULL,
    Timestamp      INTEGER NOT NULL,
    CPUPercent     REAL NOT NULL DEFAULT 0,
    RAMPercent     REAL NOT NULL DEFAULT 0,
    RAMUsedBytes   INTEGER NOT NULL DEFAULT 0,
    RAMTotalBytes  INTEGER NOT NULL DEFAULT 0,
    DiskUsedBytes  INTEGER NOT NULL DEFAULT 0,
    DiskTotalBytes INTEGER NOT NULL DEFAULT 0,
    ProcessCount   INTEGER NOT NULL DEFAULT 0,
    PingLatencyMs  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_telemetry_dev_time ON TelemetryHistory(DeviceId, Timestamp DESC);

CREATE TABLE IF NOT EXISTS CloudFileCache (
    DeviceId      TEXT NOT NULL,
    Path          TEXT NOT NULL,
    IsDir         INTEGER NOT NULL DEFAULT 0,
    SizeBytes     INTEGER NOT NULL DEFAULT 0,
    ContentBase64 TEXT,
    FilesJSON     TEXT,
    UpdatedAt     INTEGER NOT NULL,
    PRIMARY KEY(DeviceId, Path)
);
CREATE INDEX IF NOT EXISTS idx_filecache_dev ON CloudFileCache(DeviceId, Path);
`

func (s *Store) migrate() error {
	if s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	_, _ = s.db.Exec(`ALTER TABLE Devices ADD COLUMN UserId TEXT NOT NULL DEFAULT '';`)
	return nil
}

// UpsertDevice inserts or updates a device registration.
func (s *Store) UpsertDevice(info protocol.DeviceInfo) error {
	if s.supabase != nil {
		return s.supabase.UpsertDevice(info)
	}
	healthBytes, _ := json.Marshal(info.Metrics)
	_, err := s.db.Exec(`
		INSERT INTO Devices (DeviceId, UserId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(DeviceId) DO UPDATE SET
			UserId = CASE WHEN excluded.UserId != '' THEN excluded.UserId ELSE Devices.UserId END,
			Hostname = excluded.Hostname,
			OS = excluded.OS,
			Status = excluded.Status,
			LastHeartbeat = excluded.LastHeartbeat,
			ConnectedAt = excluded.ConnectedAt,
			HealthJSON = excluded.HealthJSON
	`, info.DeviceID, info.UserID, info.Hostname, info.OS, info.Status, info.LastHeartbeat, info.ConnectedAt, string(healthBytes))
	return err
}

// UpdateDeviceHeartbeat updates heartbeat time, telemetry metrics, and status for a device.
func (s *Store) UpdateDeviceHeartbeat(deviceID string, metrics protocol.HealthMetrics, status string) error {
	if s.supabase != nil {
		return s.supabase.UpdateDeviceHeartbeat(deviceID, metrics, status)
	}
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
	if s.supabase != nil {
		return s.supabase.UpdateDeviceStatus(deviceID, status)
	}
	_, err := s.db.Exec(`UPDATE Devices SET Status = ? WHERE DeviceId = ?`, status, deviceID)
	return err
}

// GetDevice retrieves a device by DeviceID.
func (s *Store) GetDevice(deviceID string) (*protocol.DeviceInfo, error) {
	if s.supabase != nil {
		return s.supabase.GetDevice(deviceID)
	}
	var (
		d           protocol.DeviceInfo
		userID      sql.NullString
		healthStr   sql.NullString
		lastHB      int64
		connectedAt int64
	)
	err := s.db.QueryRow(`
		SELECT DeviceId, UserId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON
		  FROM Devices WHERE DeviceId = ?
	`, deviceID).Scan(&d.DeviceID, &userID, &d.Hostname, &d.OS, &d.Status, &lastHB, &connectedAt, &healthStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if userID.Valid {
		d.UserID = userID.String
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
	if s.supabase != nil {
		return s.supabase.ListDevices()
	}
	rows, err := s.db.Query(`
		SELECT DeviceId, UserId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON
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
			userID    sql.NullString
			healthStr sql.NullString
		)
		if err := rows.Scan(&d.DeviceID, &userID, &d.Hostname, &d.OS, &d.Status, &d.LastHeartbeat, &d.ConnectedAt, &healthStr); err != nil {
			return nil, err
		}
		if userID.Valid {
			d.UserID = userID.String
		}
		if healthStr.Valid && healthStr.String != "" {
			_ = json.Unmarshal([]byte(healthStr.String), &d.Metrics)
		}
		if d.Metrics.MACAddress != "" {
			d.MACAddress = d.Metrics.MACAddress
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// --- User Management & Scoped Devices ---

// CreateUser registers a new user with plain/viewable password and generates a unique AuthToken.
func (s *Store) CreateUser(username, password string) (*protocol.UserAccount, error) {
	if s.supabase != nil {
		return s.supabase.CreateUser(username, password)
	}
	if username == "" || password == "" {
		return nil, fmt.Errorf("username and password cannot be empty")
	}
	userID := "usr_" + uuid.NewString()[:8]
	authToken := "usr_tok_" + uuid.NewString()[:12]
	now := time.Now().Unix()

	_, err := s.db.Exec(`
		INSERT INTO Users (UserId, Username, Password, AuthToken, CreatedAt)
		VALUES (?, ?, ?, ?, ?)
	`, userID, username, password, authToken, now)
	if err != nil {
		return nil, fmt.Errorf("username %q is already taken", username)
	}

	return &protocol.UserAccount{
		UserID:    userID,
		Username:  username,
		Password:  password,
		AuthToken: authToken,
		CreatedAt: now,
	}, nil
}

// AuthenticateUser checks username and password and returns the user record.
func (s *Store) AuthenticateUser(username, password string) (*protocol.UserAccount, error) {
	if s.supabase != nil {
		return s.supabase.AuthenticateUser(username, password)
	}
	var u protocol.UserAccount
	err := s.db.QueryRow(`
		SELECT UserId, Username, Password, AuthToken, CreatedAt
		  FROM Users
		 WHERE Username = ? AND Password = ?
	`, username, password).Scan(&u.UserID, &u.Username, &u.Password, &u.AuthToken, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("invalid username or password")
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByToken finds a user by their AuthToken.
func (s *Store) GetUserByToken(token string) (*protocol.UserAccount, error) {
	if token == "" {
		return nil, nil
	}
	if s.supabase != nil {
		return s.supabase.GetUserByToken(token)
	}
	var u protocol.UserAccount
	err := s.db.QueryRow(`
		SELECT UserId, Username, Password, AuthToken, CreatedAt
		  FROM Users
		 WHERE AuthToken = ?
	`, token).Scan(&u.UserID, &u.Username, &u.Password, &u.AuthToken, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByUsername finds a user by username.
func (s *Store) GetUserByUsername(username string) (*protocol.UserAccount, error) {
	if s.supabase != nil {
		return s.supabase.GetUserByUsername(username)
	}
	var u protocol.UserAccount
	err := s.db.QueryRow(`
		SELECT UserId, Username, Password, AuthToken, CreatedAt
		  FROM Users
		 WHERE Username = ?
	`, username).Scan(&u.UserID, &u.Username, &u.Password, &u.AuthToken, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ListDevicesForUser returns devices belonging strictly to the specified user.
func (s *Store) ListDevicesForUser(userID string) ([]protocol.DeviceInfo, error) {
	if s.supabase != nil {
		return s.supabase.ListDevicesForUser(userID)
	}
	if userID == "" {
		return s.ListDevices()
	}
	rows, err := s.db.Query(`
		SELECT DeviceId, UserId, Hostname, OS, Status, LastHeartbeat, ConnectedAt, HealthJSON
		  FROM Devices
		 WHERE UserId = ?
		 ORDER BY LastHeartbeat DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []protocol.DeviceInfo
	for rows.Next() {
		var (
			d         protocol.DeviceInfo
			uID       sql.NullString
			healthStr sql.NullString
		)
		if err := rows.Scan(&d.DeviceID, &uID, &d.Hostname, &d.OS, &d.Status, &d.LastHeartbeat, &d.ConnectedAt, &healthStr); err != nil {
			return nil, err
		}
		if uID.Valid {
			d.UserID = uID.String
		}
		if healthStr.Valid && healthStr.String != "" {
			_ = json.Unmarshal([]byte(healthStr.String), &d.Metrics)
		}
		if d.Metrics.MACAddress != "" {
			d.MACAddress = d.Metrics.MACAddress
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// BindDeviceToUser associates a device with a user.
func (s *Store) BindDeviceToUser(deviceID, userID string) error {
	if s.supabase != nil {
		return s.supabase.BindDeviceToUser(deviceID, userID)
	}
	_, err := s.db.Exec(`UPDATE Devices SET UserId = ? WHERE DeviceId = ?`, userID, deviceID)
	return err
}

// EnqueueCommand stores a new command for a device with Status = PENDING.
func (s *Store) EnqueueCommand(cmd protocol.CommandPayload) error {
	if s.supabase != nil {
		return s.supabase.EnqueueCommand(cmd)
	}
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
	if s.supabase != nil {
		return s.supabase.GetPendingCommands(deviceID)
	}
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
	if s.supabase != nil {
		return nil
	}
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
	if s.supabase != nil {
		return s.supabase.MarkCommandDispatched(commandID)
	}
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
	if s.supabase != nil {
		return s.supabase.MarkCommandCompleted(commandID, status)
	}
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
	if s.supabase != nil {
		return s.supabase.GetCommand(commandID)
	}
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
	if s.supabase != nil {
		return s.supabase.SaveResult(res)
	}
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
	if s.supabase != nil {
		return s.supabase.SaveSandboxResult(res)
	}
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
	if s.supabase != nil {
		return s.supabase.GetResult(commandID)
	}
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
	if s.supabase != nil {
		return s.supabase.ListResults(deviceID, limit)
	}
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
	if s.supabase != nil {
		return s.supabase.RecordAuditLog(log)
	}
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
	if s.supabase != nil {
		return s.supabase.ListAuditLogs(deviceID, limit)
	}
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

// RecordTelemetry records a periodic telemetry sample for a device.
func (s *Store) RecordTelemetry(point protocol.TelemetryPoint) error {
	if s.supabase != nil {
		return s.supabase.RecordTelemetry(point)
	}
	if point.Timestamp == 0 {
		point.Timestamp = time.Now().Unix()
	}
	_, err := s.db.Exec(`
		INSERT INTO TelemetryHistory (DeviceId, Timestamp, CPUPercent, RAMPercent, RAMUsedBytes, RAMTotalBytes, DiskUsedBytes, DiskTotalBytes, ProcessCount, PingLatencyMs)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, point.DeviceID, point.Timestamp, point.CPUPercent, point.RAMPercent, point.RAMUsedBytes, point.RAMTotalBytes, point.DiskUsedBytes, point.DiskTotalBytes, point.ProcessCount, point.PingLatencyMs)
	return err
}

// GetTelemetryHistory returns recent telemetry points for a device, ordered chronologically.
func (s *Store) GetTelemetryHistory(deviceID string, limit int) ([]protocol.TelemetryPoint, error) {
	if s.supabase != nil {
		return s.supabase.GetTelemetryHistory(deviceID, limit)
	}
	if limit <= 0 {
		limit = 60
	}
	rows, err := s.db.Query(`
		SELECT DeviceId, Timestamp, CPUPercent, RAMPercent, RAMUsedBytes, RAMTotalBytes, DiskUsedBytes, DiskTotalBytes, ProcessCount, PingLatencyMs
		  FROM (
			SELECT DeviceId, Timestamp, CPUPercent, RAMPercent, RAMUsedBytes, RAMTotalBytes, DiskUsedBytes, DiskTotalBytes, ProcessCount, PingLatencyMs
			  FROM TelemetryHistory
			 WHERE DeviceId = ?
			 ORDER BY Timestamp DESC
			 LIMIT ?
		  ) ORDER BY Timestamp ASC
	`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []protocol.TelemetryPoint
	for rows.Next() {
		var p protocol.TelemetryPoint
		if err := rows.Scan(&p.DeviceID, &p.Timestamp, &p.CPUPercent, &p.RAMPercent, &p.RAMUsedBytes, &p.RAMTotalBytes, &p.DiskUsedBytes, &p.DiskTotalBytes, &p.ProcessCount, &p.PingLatencyMs); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// CacheDirectoryListing saves directory file listings for offline browsing.
func (s *Store) CacheDirectoryListing(deviceID, path string, files []protocol.FileInfo) error {
	if s.supabase != nil {
		return s.supabase.CacheDirectoryListing(deviceID, path, files)
	}
	bytes, _ := json.Marshal(files)
	_, err := s.db.Exec(`
		INSERT INTO CloudFileCache (DeviceId, Path, IsDir, SizeBytes, FilesJSON, UpdatedAt)
		VALUES (?, ?, 1, 0, ?, ?)
		ON CONFLICT(DeviceId, Path) DO UPDATE SET
			FilesJSON = excluded.FilesJSON,
			UpdatedAt = excluded.UpdatedAt
	`, deviceID, path, string(bytes), time.Now().Unix())
	return err
}

// GetCachedDirectoryListing retrieves cached files for offline browsing.
func (s *Store) GetCachedDirectoryListing(deviceID, path string) ([]protocol.FileInfo, bool) {
	if s.supabase != nil {
		return s.supabase.GetCachedDirectoryListing(deviceID, path)
	}
	var filesJSON sql.NullString
	err := s.db.QueryRow(`
		SELECT FilesJSON FROM CloudFileCache WHERE DeviceId = ? AND Path = ? AND IsDir = 1
	`, deviceID, path).Scan(&filesJSON)
	if err != nil || !filesJSON.Valid || filesJSON.String == "" {
		return nil, false
	}
	var files []protocol.FileInfo
	if err := json.Unmarshal([]byte(filesJSON.String), &files); err != nil {
		return nil, false
	}
	return files, true
}

// CacheFileContent stores a snapshot of a file's base64 content in cloud storage.
func (s *Store) CacheFileContent(deviceID, path, contentBase64 string, sizeBytes int64) error {
	if s.supabase != nil {
		return s.supabase.CacheFileContent(deviceID, path, contentBase64, sizeBytes)
	}
	_, err := s.db.Exec(`
		INSERT INTO CloudFileCache (DeviceId, Path, IsDir, SizeBytes, ContentBase64, UpdatedAt)
		VALUES (?, ?, 0, ?, ?, ?)
		ON CONFLICT(DeviceId, Path) DO UPDATE SET
			SizeBytes = excluded.SizeBytes,
			ContentBase64 = excluded.ContentBase64,
			UpdatedAt = excluded.UpdatedAt
	`, deviceID, path, sizeBytes, contentBase64, time.Now().Unix())
	return err
}

// GetCachedFileContent returns a cached file's content and size.
func (s *Store) GetCachedFileContent(deviceID, path string) (string, int64, bool) {
	if s.supabase != nil {
		return s.supabase.GetCachedFileContent(deviceID, path)
	}
	var (
		content sql.NullString
		size    int64
	)
	err := s.db.QueryRow(`
		SELECT ContentBase64, SizeBytes FROM CloudFileCache WHERE DeviceId = ? AND Path = ? AND IsDir = 0
	`, deviceID, path).Scan(&content, &size)
	if err != nil || !content.Valid {
		return "", 0, false
	}
	return content.String, size, true
}

