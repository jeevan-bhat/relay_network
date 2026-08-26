// Package protocol defines the typed message envelopes, payloads, and constants
// exchanged between the Relay Server, Windows Agent, and Controllers via WebSockets.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Message types exchanged over WebSockets.
const (
	TypeAuth            = "AUTH"
	TypeAuthAck         = "AUTH_ACK"
	TypeHeartbeat       = "HEARTBEAT"
	TypeHeartbeatAck    = "HEARTBEAT_ACK"
	TypeEnqueueCmd      = "ENQUEUE_CMD"
	TypeDispatchCmd     = "DISPATCH_CMD"
	TypeCmdResult       = "CMD_RESULT"
	TypeResultAck       = "RESULT_ACK"
	TypeSyncReq         = "SYNC_REQ"
	TypeSyncResp        = "SYNC_RESP"
	TypeDeviceStatus    = "DEVICE_STATUS"
	TypeGetDevices      = "GET_DEVICES"
	TypeDeviceList      = "DEVICE_LIST"
	TypeError           = "ERROR"

	// File Management
	TypeFileListReq     = "FILE_LIST_REQ"
	TypeFileListResp    = "FILE_LIST_RESP"
	TypeFileReadReq     = "FILE_READ_REQ"
	TypeFileReadResp    = "FILE_READ_RESP"
	TypeFileWriteReq    = "FILE_WRITE_REQ"
	TypeFileWriteResp   = "FILE_WRITE_RESP"
	TypeFileDeleteReq   = "FILE_DELETE_REQ"
	TypeFileDeleteResp  = "FILE_DELETE_RESP"

	// Process & Service Management
	TypeProcessListReq  = "PROCESS_LIST_REQ"
	TypeProcessListResp = "PROCESS_LIST_RESP"
	TypeProcessKillReq  = "PROCESS_KILL_REQ"
	TypeProcessKillResp = "PROCESS_KILL_RESP"

	// Audit Logs
	TypeAuditLog        = "AUDIT_LOG"
	TypeGetAuditLogs    = "GET_AUDIT_LOGS"
	TypeAuditLogsList   = "AUDIT_LOGS_LIST"
)

// Roles for authenticated connections.
const (
	RoleAgent      = "agent"
	RoleController = "controller"
)

// Device connection states.
const (
	StatusOnline   = "ONLINE"
	StatusDegraded = "DEGRADED"
	StatusOffline  = "OFFLINE"
)

// Envelope is the standard JSON wrapper for all WebSocket communications.
type Envelope struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	DeviceID  string          `json:"deviceId,omitempty"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope wraps a payload inside an Envelope with an auto-generated UUID and current timestamp.
func NewEnvelope(msgType, deviceID string, payload any) (Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, err
		}
		raw = b
	}
	return Envelope{
		Type:      msgType,
		ID:        uuid.NewString(),
		DeviceID:  deviceID,
		Timestamp: time.Now().Unix(),
		Payload:   raw,
	}, nil
}

// DecodePayload unmarshals the envelope's raw JSON payload into target.
func (e Envelope) DecodePayload(target any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, target)
}

// AuthPayload is sent immediately after WebSocket connection opens.
type AuthPayload struct {
	Role     string `json:"role"`               // "agent" or "controller"
	DeviceID string `json:"deviceId"`           // Unique device identifier
	Token    string `json:"token,omitempty"`    // Shared secret or token
	Hostname string `json:"hostname,omitempty"` // Host machine name
	OS       string `json:"os,omitempty"`       // Operating system name
	Version  string `json:"version,omitempty"`  // Client software version
}

// AuthAckPayload is the relay response to an AuthPayload.
type AuthAckPayload struct {
	Success    bool   `json:"success"`
	DeviceID   string `json:"deviceId,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Error      string `json:"error,omitempty"`
	ServerTime int64  `json:"serverTime"`
}

// HealthMetrics carries system telemetry.
type HealthMetrics struct {
	CPUPercent      float64 `json:"cpuPercent"`
	RAMPercent      float64 `json:"ramPercent"`
	RAMUsedBytes    uint64  `json:"ramUsedBytes"`
	RAMTotalBytes   uint64  `json:"ramTotalBytes"`
	DiskUsedBytes   uint64  `json:"diskUsedBytes"`
	DiskTotalBytes  uint64  `json:"diskTotalBytes"`
	UptimeSec       int64   `json:"uptimeSec"`
	ProcessCount    int     `json:"processCount"`
	PendingCmdCount int     `json:"pendingCmdCount"`
}

// HeartbeatPayload is sent periodically by the agent.
type HeartbeatPayload struct {
	DeviceID string        `json:"deviceId"`
	Metrics  HealthMetrics `json:"metrics"`
}

// HeartbeatAckPayload acknowledges a heartbeat.
type HeartbeatAckPayload struct {
	DeviceID   string `json:"deviceId"`
	ServerTime int64  `json:"serverTime"`
}

// CommandPayload encapsulates a command to execute.
type CommandPayload struct {
	CommandID  string `json:"commandId"`
	DeviceID   string `json:"deviceId"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
}

// ResultPayload carries the execution output of a command.
type ResultPayload struct {
	ResultID   string `json:"resultId,omitempty"`
	CommandID  string `json:"commandId"`
	DeviceID   string `json:"deviceId"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	ExecutedAt int64  `json:"executedAt"`
	DurationMs int64  `json:"durationMs,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
}

// ResultAckPayload acknowledges receipt of a command result by the relay.
type ResultAckPayload struct {
	CommandID string `json:"commandId"`
	ResultID  string `json:"resultId,omitempty"`
	Success   bool   `json:"success"`
}

// SyncReqPayload is sent by the agent when reconnecting to synchronize offline state.
type SyncReqPayload struct {
	DeviceID        string          `json:"deviceId"`
	UnsyncedResults []ResultPayload `json:"unsyncedResults,omitempty"`
}

// SyncRespPayload contains pending commands queued in the cloud while the agent was offline.
type SyncRespPayload struct {
	DeviceID        string           `json:"deviceId"`
	AckedResults    []string         `json:"ackedResults"`
	PendingCommands []CommandPayload `json:"pendingCommands"`
}

// DeviceInfo represents a registered device in the Relay.
type DeviceInfo struct {
	DeviceID      string        `json:"deviceId"`
	Hostname      string        `json:"hostname"`
	OS            string        `json:"os"`
	Status        string        `json:"status"` // ONLINE, DEGRADED, OFFLINE
	LastHeartbeat int64         `json:"lastHeartbeat"`
	ConnectedAt   int64         `json:"connectedAt"`
	Metrics       HealthMetrics `json:"metrics"`
}

// DeviceListPayload sends the current snapshot of all devices.
type DeviceListPayload struct {
	Devices []DeviceInfo `json:"devices"`
}

// ErrorPayload represents an error message.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Phase 4: File Management Payloads ---

// FileInfo represents a remote file or folder entry.
type FileInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	IsDir     bool   `json:"isDir"`
	ModTime   int64  `json:"modTime"`
	Mode      string `json:"mode"`
}

// FileListReqPayload requests a directory listing from an agent.
type FileListReqPayload struct {
	DeviceID string `json:"deviceId"`
	Path     string `json:"path"`
}

// FileListRespPayload returns a directory listing.
type FileListRespPayload struct {
	DeviceID string     `json:"deviceId"`
	Path     string     `json:"path"`
	Files    []FileInfo `json:"files"`
	Error    string     `json:"error,omitempty"`
}

// FileReadReqPayload requests file content to download.
type FileReadReqPayload struct {
	DeviceID string `json:"deviceId"`
	Path     string `json:"path"`
}

// FileReadRespPayload returns file content in Base64 encoding.
type FileReadRespPayload struct {
	DeviceID      string `json:"deviceId"`
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
	SizeBytes     int64  `json:"sizeBytes"`
	Error         string `json:"error,omitempty"`
}

// FileWriteReqPayload uploads a file to the agent.
type FileWriteReqPayload struct {
	DeviceID      string `json:"deviceId"`
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
	Overwrite     bool   `json:"overwrite"`
}

// FileWriteRespPayload acknowledges a file write.
type FileWriteRespPayload struct {
	DeviceID string `json:"deviceId"`
	Path     string `json:"path"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
}

// FileDeleteReqPayload requests deleting a file or directory.
type FileDeleteReqPayload struct {
	DeviceID string `json:"deviceId"`
	Path     string `json:"path"`
}

// FileDeleteRespPayload acknowledges file deletion.
type FileDeleteRespPayload struct {
	DeviceID string `json:"deviceId"`
	Path     string `json:"path"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
}

// --- Phase 4: Process & Service Payloads ---

// ProcessInfo represents a running OS process.
type ProcessInfo struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpuPercent"`
	MemoryMB   float64 `json:"memoryMb"`
	Status     string  `json:"status"`
}

// ProcessListReqPayload requests running processes.
type ProcessListReqPayload struct {
	DeviceID string `json:"deviceId"`
}

// ProcessListRespPayload returns running processes.
type ProcessListRespPayload struct {
	DeviceID  string        `json:"deviceId"`
	Processes []ProcessInfo `json:"processes"`
	Error     string        `json:"error,omitempty"`
}

// ProcessKillReqPayload requests terminating a process by PID.
type ProcessKillReqPayload struct {
	DeviceID string `json:"deviceId"`
	PID      int    `json:"pid"`
}

// ProcessKillRespPayload acknowledges process termination.
type ProcessKillRespPayload struct {
	DeviceID string `json:"deviceId"`
	PID      int    `json:"pid"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
}

// --- Phase 4: Audit Log Payloads ---

// AuditLogRecord represents an immutable audit entry in the relay.
type AuditLogRecord struct {
	LogID       string `json:"logId"`
	DeviceID    string `json:"deviceId"`
	Timestamp   int64  `json:"timestamp"`
	ActionType  string `json:"actionType"` // EXEC_CMD, FILE_READ, FILE_WRITE, FILE_DELETE, PROCESS_KILL, AUTH
	CommandText string `json:"commandText"`
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	Details     string `json:"details,omitempty"`
}

// AuditLogsListPayload returns historical audit logs.
type AuditLogsListPayload struct {
	Logs []AuditLogRecord `json:"logs"`
}
