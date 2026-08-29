// Package store provides Supabase cloud-backed and SQLite persistent storage.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"terminalrelay/internal/protocol"
)

// SupabaseStore interacts directly with the Supabase PostgREST API.
type SupabaseStore struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewSupabaseStore creates a new Supabase-backed store.
func NewSupabaseStore(supabaseURL, apiKey string) (*SupabaseStore, error) {
	cleanURL := strings.TrimRight(strings.TrimSpace(supabaseURL), "/")
	if !strings.HasPrefix(cleanURL, "http://") && !strings.HasPrefix(cleanURL, "https://") {
		cleanURL = "https://" + cleanURL
	}
	cleanKey := strings.TrimSpace(apiKey)
	if cleanURL == "" || cleanKey == "" {
		return nil, fmt.Errorf("supabase URL and API key cannot be empty")
	}

	return &SupabaseStore{
		baseURL: cleanURL + "/rest/v1",
		apiKey:  cleanKey,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

func (s *SupabaseStore) req(method, endpoint string, body any, headers map[string]string) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}

	reqURL := s.baseURL + endpoint
	req, err := http.NewRequest(method, reqURL, bodyReader)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("apikey", s.apiKey)
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return respBytes, resp.StatusCode, nil
}

// User Models for Supabase
type sbUser struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	AuthToken string `json:"auth_token"`
	CreatedAt int64  `json:"created_at"`
}

// Device Models for Supabase
type sbDevice struct {
	DeviceID      string `json:"device_id"`
	UserID        string `json:"user_id"`
	Hostname      string `json:"hostname,omitempty"`
	OS            string `json:"os,omitempty"`
	Status        string `json:"status"`
	LastHeartbeat int64  `json:"last_heartbeat"`
	ConnectedAt   int64  `json:"connected_at"`
	HealthJSON    string `json:"health_json,omitempty"`
}

// Command Queue Model
type sbCommand struct {
	CommandID    string `json:"command_id"`
	DeviceID     string `json:"device_id"`
	Command      string `json:"command"`
	TimeoutSec   int    `json:"timeout_sec"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"created_at"`
	DispatchedAt int64  `json:"dispatched_at,omitempty"`
	CompletedAt  int64  `json:"completed_at,omitempty"`
}

// Result Queue Model
type sbResult struct {
	ResultID   string `json:"result_id"`
	CommandID  string `json:"command_id"`
	DeviceID   string `json:"device_id"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	ExecutedAt int64  `json:"executed_at"`
	ReceivedAt int64  `json:"received_at"`
}

// Audit Log Model
type sbAudit struct {
	LogID       string `json:"log_id"`
	DeviceID    string `json:"device_id"`
	Timestamp   int64  `json:"timestamp"`
	ActionType  string `json:"action_type"`
	CommandText string `json:"command_text,omitempty"`
	ExitCode    int    `json:"exit_code"`
	DurationMs  int64  `json:"duration_ms"`
	Details     string `json:"details,omitempty"`
}

// ==============================================================
// USER MANAGEMENT IN SUPABASE
// ==============================================================

func (s *SupabaseStore) CreateUser(username, password string) (*protocol.UserAccount, error) {
	if username == "" || password == "" {
		return nil, fmt.Errorf("username and password cannot be empty")
	}

	// Check if user already exists
	existing, _ := s.GetUserByUsername(username)
	if existing != nil {
		return nil, fmt.Errorf("username %q is already taken", username)
	}

	u := sbUser{
		UserID:    "usr_" + uuid.NewString()[:8],
		Username:  username,
		Password:  password,
		AuthToken: "usr_tok_" + uuid.NewString()[:12],
		CreatedAt: time.Now().Unix(),
	}

	resBytes, code, err := s.req("POST", "/users", u, map[string]string{
		"Prefer": "return=representation",
	})
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("supabase user create failed (%d): %s", code, string(resBytes))
	}

	return &protocol.UserAccount{
		UserID:    u.UserID,
		Username:  u.Username,
		Password:  u.Password,
		AuthToken: u.AuthToken,
		CreatedAt: u.CreatedAt,
	}, nil
}

func (s *SupabaseStore) AuthenticateUser(username, password string) (*protocol.UserAccount, error) {
	u, err := s.GetUserByUsername(username)
	if err != nil || u == nil {
		return nil, fmt.Errorf("invalid username or password")
	}
	if u.Password != password {
		return nil, fmt.Errorf("invalid username or password")
	}
	return u, nil
}

func (s *SupabaseStore) GetUserByToken(token string) (*protocol.UserAccount, error) {
	if token == "" {
		return nil, nil
	}
	endpoint := fmt.Sprintf("/users?auth_token=eq.%s&limit=1", url.QueryEscape(token))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbUser
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 {
		return nil, nil
	}

	return &protocol.UserAccount{
		UserID:    list[0].UserID,
		Username:  list[0].Username,
		Password:  list[0].Password,
		AuthToken: list[0].AuthToken,
		CreatedAt: list[0].CreatedAt,
	}, nil
}

func (s *SupabaseStore) GetUserByUsername(username string) (*protocol.UserAccount, error) {
	endpoint := fmt.Sprintf("/users?username=eq.%s&limit=1", url.QueryEscape(username))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbUser
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 {
		return nil, nil
	}

	return &protocol.UserAccount{
		UserID:    list[0].UserID,
		Username:  list[0].Username,
		Password:  list[0].Password,
		AuthToken: list[0].AuthToken,
		CreatedAt: list[0].CreatedAt,
	}, nil
}

// ==============================================================
// DEVICE MANAGEMENT IN SUPABASE
// ==============================================================

func (s *SupabaseStore) UpsertDevice(info protocol.DeviceInfo) error {
	healthBytes, _ := json.Marshal(info.Metrics)
	d := sbDevice{
		DeviceID:      info.DeviceID,
		UserID:        info.UserID,
		Hostname:      info.Hostname,
		OS:            info.OS,
		Status:        info.Status,
		LastHeartbeat: info.LastHeartbeat,
		ConnectedAt:   info.ConnectedAt,
		HealthJSON:    string(healthBytes),
	}

	_, _, err := s.req("POST", "/devices", d, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	return err
}

func (s *SupabaseStore) UpdateDeviceHeartbeat(deviceID string, metrics protocol.HealthMetrics, status string) error {
	healthBytes, _ := json.Marshal(metrics)
	payload := map[string]any{
		"last_heartbeat": time.Now().Unix(),
		"status":         status,
		"health_json":    string(healthBytes),
	}
	endpoint := fmt.Sprintf("/devices?device_id=eq.%s", url.QueryEscape(deviceID))
	_, _, err := s.req("PATCH", endpoint, payload, nil)
	return err
}

func (s *SupabaseStore) UpdateDeviceStatus(deviceID string, status string) error {
	payload := map[string]any{
		"status": status,
	}
	endpoint := fmt.Sprintf("/devices?device_id=eq.%s", url.QueryEscape(deviceID))
	_, _, err := s.req("PATCH", endpoint, payload, nil)
	return err
}

func (s *SupabaseStore) GetDevice(deviceID string) (*protocol.DeviceInfo, error) {
	endpoint := fmt.Sprintf("/devices?device_id=eq.%s&limit=1", url.QueryEscape(deviceID))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbDevice
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 {
		return nil, nil
	}

	d := list[0]
	dev := protocol.DeviceInfo{
		DeviceID:      d.DeviceID,
		UserID:        d.UserID,
		Hostname:      d.Hostname,
		OS:            d.OS,
		Status:        d.Status,
		LastHeartbeat: d.LastHeartbeat,
		ConnectedAt:   d.ConnectedAt,
	}
	if d.HealthJSON != "" {
		_ = json.Unmarshal([]byte(d.HealthJSON), &dev.Metrics)
	}
	if dev.Metrics.MACAddress != "" {
		dev.MACAddress = dev.Metrics.MACAddress
	}
	return &dev, nil
}

func (s *SupabaseStore) ListDevices() ([]protocol.DeviceInfo, error) {
	resBytes, code, err := s.req("GET", "/devices?order=last_heartbeat.desc", nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbDevice
	_ = json.Unmarshal(resBytes, &list)

	var result []protocol.DeviceInfo
	for _, d := range list {
		dev := protocol.DeviceInfo{
			DeviceID:      d.DeviceID,
			UserID:        d.UserID,
			Hostname:      d.Hostname,
			OS:            d.OS,
			Status:        d.Status,
			LastHeartbeat: d.LastHeartbeat,
			ConnectedAt:   d.ConnectedAt,
		}
		if d.HealthJSON != "" {
			_ = json.Unmarshal([]byte(d.HealthJSON), &dev.Metrics)
		}
		if dev.Metrics.MACAddress != "" {
			dev.MACAddress = dev.Metrics.MACAddress
		}
		result = append(result, dev)
	}
	return result, nil
}

func (s *SupabaseStore) ListDevicesForUser(userID string) ([]protocol.DeviceInfo, error) {
	if userID == "" {
		return s.ListDevices()
	}
	endpoint := fmt.Sprintf("/devices?user_id=eq.%s&order=last_heartbeat.desc", url.QueryEscape(userID))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbDevice
	_ = json.Unmarshal(resBytes, &list)

	var result []protocol.DeviceInfo
	for _, d := range list {
		dev := protocol.DeviceInfo{
			DeviceID:      d.DeviceID,
			UserID:        d.UserID,
			Hostname:      d.Hostname,
			OS:            d.OS,
			Status:        d.Status,
			LastHeartbeat: d.LastHeartbeat,
			ConnectedAt:   d.ConnectedAt,
		}
		if d.HealthJSON != "" {
			_ = json.Unmarshal([]byte(d.HealthJSON), &dev.Metrics)
		}
		if dev.Metrics.MACAddress != "" {
			dev.MACAddress = dev.Metrics.MACAddress
		}
		result = append(result, dev)
	}
	return result, nil
}

func (s *SupabaseStore) BindDeviceToUser(deviceID, userID string) error {
	payload := map[string]any{
		"user_id": userID,
	}
	endpoint := fmt.Sprintf("/devices?device_id=eq.%s", url.QueryEscape(deviceID))
	_, _, err := s.req("PATCH", endpoint, payload, nil)
	return err
}

// ==============================================================
// COMMAND QUEUE IN SUPABASE
// ==============================================================

func (s *SupabaseStore) EnqueueCommand(cmd protocol.CommandPayload) error {
	now := cmd.CreatedAt
	if now == 0 {
		now = time.Now().Unix()
	}
	c := sbCommand{
		CommandID:  cmd.CommandID,
		DeviceID:   cmd.DeviceID,
		Command:    cmd.Command,
		TimeoutSec: cmd.TimeoutSec,
		Status:     StatusPending,
		CreatedAt:  now,
	}
	_, _, err := s.req("POST", "/cloud_command_queue", c, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	return err
}

func (s *SupabaseStore) GetPendingCommands(deviceID string) ([]protocol.CommandPayload, error) {
	endpoint := fmt.Sprintf("/cloud_command_queue?device_id=eq.%s&status=eq.%s&order=created_at.asc", url.QueryEscape(deviceID), StatusPending)
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbCommand
	_ = json.Unmarshal(resBytes, &list)

	var result []protocol.CommandPayload
	for _, c := range list {
		result = append(result, protocol.CommandPayload{
			CommandID:  c.CommandID,
			DeviceID:   c.DeviceID,
			Command:    c.Command,
			TimeoutSec: c.TimeoutSec,
			CreatedAt:  c.CreatedAt,
		})
	}
	return result, nil
}

func (s *SupabaseStore) MarkCommandDispatched(commandID string) error {
	payload := map[string]any{
		"status":        StatusDispatched,
		"dispatched_at": time.Now().Unix(),
	}
	endpoint := fmt.Sprintf("/cloud_command_queue?command_id=eq.%s", url.QueryEscape(commandID))
	_, _, err := s.req("PATCH", endpoint, payload, nil)
	return err
}

func (s *SupabaseStore) MarkCommandCompleted(commandID, status string) error {
	payload := map[string]any{
		"status":       status,
		"completed_at": time.Now().Unix(),
	}
	endpoint := fmt.Sprintf("/cloud_command_queue?command_id=eq.%s", url.QueryEscape(commandID))
	_, _, err := s.req("PATCH", endpoint, payload, nil)
	return err
}

func (s *SupabaseStore) GetCommand(commandID string) (*CommandRecord, error) {
	endpoint := fmt.Sprintf("/cloud_command_queue?command_id=eq.%s&limit=1", url.QueryEscape(commandID))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbCommand
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 {
		return nil, nil
	}
	c := list[0]
	return &CommandRecord{
		CommandID:    c.CommandID,
		DeviceID:     c.DeviceID,
		Command:      c.Command,
		TimeoutSec:   c.TimeoutSec,
		Status:       c.Status,
		CreatedAt:    c.CreatedAt,
		DispatchedAt: c.DispatchedAt,
		CompletedAt:  c.CompletedAt,
	}, nil
}

// ==============================================================
// RESULTS & AUDIT LOGS IN SUPABASE
// ==============================================================

func (s *SupabaseStore) SaveResult(res protocol.ResultPayload) error {
	r := sbResult{
		ResultID:   uuid.NewString(),
		CommandID:  res.CommandID,
		DeviceID:   res.DeviceID,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		ExecutedAt: res.ExecutedAt,
		ReceivedAt: time.Now().Unix(),
	}
	_, _, err := s.req("POST", "/cloud_result_queue", r, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	_ = s.MarkCommandCompleted(res.CommandID, StatusCompleted)
	return err
}

func (s *SupabaseStore) SaveSandboxResult(res protocol.ResultPayload) error {
	return s.SaveResult(res)
}

func (s *SupabaseStore) GetResult(commandID string) (*protocol.ResultPayload, error) {
	endpoint := fmt.Sprintf("/cloud_result_queue?command_id=eq.%s&limit=1", url.QueryEscape(commandID))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbResult
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 {
		return nil, nil
	}
	r := list[0]
	return &protocol.ResultPayload{
		CommandID:  r.CommandID,
		DeviceID:   r.DeviceID,
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		ExitCode:   r.ExitCode,
		ExecutedAt: r.ExecutedAt,
	}, nil
}

func (s *SupabaseStore) ListResults(deviceID string, limit int) ([]protocol.ResultPayload, error) {
	if limit <= 0 {
		limit = 50
	}
	endpoint := fmt.Sprintf("/cloud_result_queue?device_id=eq.%s&order=executed_at.desc&limit=%d", url.QueryEscape(deviceID), limit)
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbResult
	_ = json.Unmarshal(resBytes, &list)

	var result []protocol.ResultPayload
	for _, r := range list {
		result = append(result, protocol.ResultPayload{
			CommandID:  r.CommandID,
			DeviceID:   r.DeviceID,
			Stdout:     r.Stdout,
			Stderr:     r.Stderr,
			ExitCode:   r.ExitCode,
			ExecutedAt: r.ExecutedAt,
		})
	}
	return result, nil
}

func (s *SupabaseStore) RecordAuditLog(entry protocol.AuditLogRecord) error {
	a := sbAudit{
		LogID:       entry.LogID,
		DeviceID:    entry.DeviceID,
		Timestamp:   entry.Timestamp,
		ActionType:  entry.ActionType,
		CommandText: entry.CommandText,
		ExitCode:    entry.ExitCode,
		DurationMs:  entry.DurationMs,
		Details:     entry.Details,
	}
	_, _, err := s.req("POST", "/audit_logs", a, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	return err
}

func (s *SupabaseStore) ListAuditLogs(deviceID string, limit int) ([]protocol.AuditLogRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	endpoint := fmt.Sprintf("/audit_logs?order=timestamp.desc&limit=%d", limit)
	if deviceID != "" {
		endpoint = fmt.Sprintf("/audit_logs?device_id=eq.%s&order=timestamp.desc&limit=%d", url.QueryEscape(deviceID), limit)
	}
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}

	var list []sbAudit
	_ = json.Unmarshal(resBytes, &list)

	var result []protocol.AuditLogRecord
	for _, a := range list {
		result = append(result, protocol.AuditLogRecord{
			LogID:       a.LogID,
			DeviceID:    a.DeviceID,
			Timestamp:   a.Timestamp,
			ActionType:  a.ActionType,
			CommandText: a.CommandText,
			ExitCode:    a.ExitCode,
			DurationMs:  a.DurationMs,
			Details:     a.Details,
		})
	}
	return result, nil
}

// Telemetry & File Cache in Supabase

func (s *SupabaseStore) RecordTelemetry(point protocol.TelemetryPoint) error {
	payload := map[string]any{
		"device_id":        point.DeviceID,
		"timestamp":        point.Timestamp,
		"cpu_percent":      point.CPUPercent,
		"ram_percent":      point.RAMPercent,
		"ram_used_bytes":   point.RAMUsedBytes,
		"ram_total_bytes":  point.RAMTotalBytes,
		"disk_used_bytes":  point.DiskUsedBytes,
		"disk_total_bytes": point.DiskTotalBytes,
		"process_count":    point.ProcessCount,
		"ping_latency_ms":  point.PingLatencyMs,
	}
	_, _, err := s.req("POST", "/telemetry_history", payload, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	return err
}

func (s *SupabaseStore) GetTelemetryHistory(deviceID string, limit int) ([]protocol.TelemetryPoint, error) {
	if limit <= 0 {
		limit = 60
	}
	endpoint := fmt.Sprintf("/telemetry_history?device_id=eq.%s&order=timestamp.desc&limit=%d", url.QueryEscape(deviceID), limit)
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, err
	}
	var list []struct {
		DeviceID       string  `json:"device_id"`
		Timestamp      int64   `json:"timestamp"`
		CPUPercent     float64 `json:"cpu_percent"`
		RAMPercent     float64 `json:"ram_percent"`
		RAMUsedBytes   uint64  `json:"ram_used_bytes"`
		RAMTotalBytes  uint64  `json:"ram_total_bytes"`
		DiskUsedBytes  uint64  `json:"disk_used_bytes"`
		DiskTotalBytes uint64  `json:"disk_total_bytes"`
		ProcessCount   int     `json:"process_count"`
		PingLatencyMs  int64   `json:"ping_latency_ms"`
	}
	_ = json.Unmarshal(resBytes, &list)
	var points []protocol.TelemetryPoint
	for i := len(list) - 1; i >= 0; i-- {
		p := list[i]
		points = append(points, protocol.TelemetryPoint{
			DeviceID:       p.DeviceID,
			Timestamp:      p.Timestamp,
			CPUPercent:     p.CPUPercent,
			RAMPercent:     p.RAMPercent,
			RAMUsedBytes:   p.RAMUsedBytes,
			RAMTotalBytes:  p.RAMTotalBytes,
			DiskUsedBytes:  p.DiskUsedBytes,
			DiskTotalBytes: p.DiskTotalBytes,
			ProcessCount:   p.ProcessCount,
			PingLatencyMs:  p.PingLatencyMs,
		})
	}
	return points, nil
}

func (s *SupabaseStore) CacheDirectoryListing(deviceID, path string, files []protocol.FileInfo) error {
	bytes, _ := json.Marshal(files)
	payload := map[string]any{
		"device_id":  deviceID,
		"path":       path,
		"is_dir":     1,
		"files_json": string(bytes),
		"updated_at": time.Now().Unix(),
	}
	_, _, err := s.req("POST", "/cloud_file_cache", payload, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	return err
}

func (s *SupabaseStore) GetCachedDirectoryListing(deviceID, path string) ([]protocol.FileInfo, bool) {
	endpoint := fmt.Sprintf("/cloud_file_cache?device_id=eq.%s&path=eq.%s&is_dir=eq.1&limit=1", url.QueryEscape(deviceID), url.QueryEscape(path))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return nil, false
	}
	var list []struct {
		FilesJSON string `json:"files_json"`
	}
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 || list[0].FilesJSON == "" {
		return nil, false
	}
	var files []protocol.FileInfo
	if err := json.Unmarshal([]byte(list[0].FilesJSON), &files); err != nil {
		return nil, false
	}
	return files, true
}

func (s *SupabaseStore) CacheFileContent(deviceID, path, contentBase64 string, sizeBytes int64) error {
	payload := map[string]any{
		"device_id":      deviceID,
		"path":           path,
		"is_dir":         0,
		"size_bytes":     sizeBytes,
		"content_base64": contentBase64,
		"updated_at":     time.Now().Unix(),
	}
	_, _, err := s.req("POST", "/cloud_file_cache", payload, map[string]string{
		"Prefer": "resolution=merge-duplicates",
	})
	return err
}

func (s *SupabaseStore) GetCachedFileContent(deviceID, path string) (string, int64, bool) {
	endpoint := fmt.Sprintf("/cloud_file_cache?device_id=eq.%s&path=eq.%s&is_dir=eq.0&limit=1", url.QueryEscape(deviceID), url.QueryEscape(path))
	resBytes, code, err := s.req("GET", endpoint, nil, nil)
	if err != nil || code >= 300 {
		return "", 0, false
	}
	var list []struct {
		ContentBase64 string `json:"content_base64"`
		SizeBytes     int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(resBytes, &list); err != nil || len(list) == 0 {
		return "", 0, false
	}
	return list[0].ContentBase64, list[0].SizeBytes, true
}
