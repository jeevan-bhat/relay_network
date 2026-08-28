package store_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"terminalrelay/internal/protocol"
	"terminalrelay/internal/store"
)

func TestSupabaseStoreAdapter(t *testing.T) {
	var (
		mu       sync.Mutex
		users    = make(map[string]map[string]any)
		devices  = make(map[string]map[string]any)
		commands = make(map[string]map[string]any)
		results  = make(map[string]map[string]any)
		audits   = make(map[string]map[string]any)
	)

	// Mock Supabase PostgREST Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Verify Auth headers
		if r.Header.Get("apikey") != "test-api-key" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer test-api-key") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
			return
		}

		mu.Lock()
		defer mu.Unlock()

		path := r.URL.Path

		switch {
		case strings.HasPrefix(path, "/rest/v1/users"):
			if r.Method == http.MethodPost {
				var u map[string]any
				_ = json.NewDecoder(r.Body).Decode(&u)
				username, _ := u["username"].(string)
				users[username] = u
				w.WriteHeader(http.StatusCreated)
				b, _ := json.Marshal([]any{u})
				_, _ = w.Write(b)
				return
			}
			if r.Method == http.MethodGet {
				var list []any
				for _, u := range users {
					username, _ := u["username"].(string)
					token, _ := u["auth_token"].(string)
					if strings.Contains(r.URL.RawQuery, "username=eq."+username) || strings.Contains(r.URL.RawQuery, "auth_token=eq."+token) {
						list = append(list, u)
					}
				}
				b, _ := json.Marshal(list)
				_, _ = w.Write(b)
				return
			}

		case strings.HasPrefix(path, "/rest/v1/devices"):
			if r.Method == http.MethodPost {
				var d map[string]any
				_ = json.NewDecoder(r.Body).Decode(&d)
				id, _ := d["device_id"].(string)
				devices[id] = d
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.Method == http.MethodPatch {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.Method == http.MethodGet {
				var list []any
				for _, d := range devices {
					list = append(list, d)
				}
				b, _ := json.Marshal(list)
				_, _ = w.Write(b)
				return
			}

		case strings.HasPrefix(path, "/rest/v1/cloud_command_queue"):
			if r.Method == http.MethodPost {
				var c map[string]any
				_ = json.NewDecoder(r.Body).Decode(&c)
				id, _ := c["command_id"].(string)
				commands[id] = c
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.Method == http.MethodPatch {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.Method == http.MethodGet {
				var list []any
				for _, c := range commands {
					list = append(list, c)
				}
				b, _ := json.Marshal(list)
				_, _ = w.Write(b)
				return
			}

		case strings.HasPrefix(path, "/rest/v1/cloud_result_queue"):
			if r.Method == http.MethodPost {
				var res map[string]any
				_ = json.NewDecoder(r.Body).Decode(&res)
				id, _ := res["result_id"].(string)
				results[id] = res
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.Method == http.MethodGet {
				var list []any
				for _, res := range results {
					list = append(list, res)
				}
				b, _ := json.Marshal(list)
				_, _ = w.Write(b)
				return
			}

		case strings.HasPrefix(path, "/rest/v1/audit_logs"):
			if r.Method == http.MethodPost {
				var a map[string]any
				_ = json.NewDecoder(r.Body).Decode(&a)
				id, _ := a["log_id"].(string)
				audits[id] = a
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if r.Method == http.MethodGet {
				var list []any
				for _, a := range audits {
					list = append(list, a)
				}
				b, _ := json.Marshal(list)
				_, _ = w.Write(b)
				return
			}
		}

		http.NotFound(w, r)
	}))
	defer ts.Close()

	// Initialize Supabase Store with mock URL
	st, err := store.OpenWithConfig("", ts.URL, "test-api-key")
	if err != nil {
		t.Fatalf("OpenWithConfig Supabase failed: %v", err)
	}
	defer st.Close()

	if !st.IsSupabase() {
		t.Fatalf("expected IsSupabase to be true")
	}

	// 1. Test User Management
	u, err := st.CreateUser("jeevan", "mypassword")
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if u.Username != "jeevan" || u.AuthToken == "" {
		t.Fatalf("unexpected user: %+v", u)
	}

	authU, err := st.AuthenticateUser("jeevan", "mypassword")
	if err != nil || authU.UserID != u.UserID {
		t.Fatalf("AuthenticateUser failed: %v", err)
	}

	tokU, err := st.GetUserByToken(u.AuthToken)
	if err != nil || tokU == nil || tokU.Username != "jeevan" {
		t.Fatalf("GetUserByToken failed: %v", err)
	}

	// 2. Test Device Management
	err = st.UpsertDevice(protocol.DeviceInfo{
		DeviceID:      "asus_laptop",
		UserID:        u.UserID,
		Hostname:      "ASUS-ROG",
		OS:            "windows/amd64",
		Status:        protocol.StatusOnline,
		LastHeartbeat: time.Now().Unix(),
		Metrics: protocol.HealthMetrics{
			CPUPercent: 15.5,
		},
	})
	if err != nil {
		t.Fatalf("UpsertDevice failed: %v", err)
	}

	devs, err := st.ListDevicesForUser(u.UserID)
	if err != nil || len(devs) != 1 || devs[0].DeviceID != "asus_laptop" {
		t.Fatalf("ListDevicesForUser failed: %v, got=%+v", err, devs)
	}
	if devs[0].Metrics.CPUPercent != 15.5 {
		t.Fatalf("CPUPercent mismatch: %f", devs[0].Metrics.CPUPercent)
	}

	// 3. Test Command Queue
	err = st.EnqueueCommand(protocol.CommandPayload{
		CommandID: "cmd_1",
		DeviceID:  "asus_laptop",
		Command:   "Get-Date",
	})
	if err != nil {
		t.Fatalf("EnqueueCommand failed: %v", err)
	}

	pending, err := st.GetPendingCommands("asus_laptop")
	if err != nil || len(pending) != 1 {
		t.Fatalf("GetPendingCommands failed: %v, got=%+v", err, pending)
	}

	// 4. Test Result & Audit
	err = st.SaveResult(protocol.ResultPayload{
		CommandID:  "cmd_1",
		DeviceID:   "asus_laptop",
		Stdout:     "2026-08-28",
		ExitCode:   0,
		ExecutedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("SaveResult failed: %v", err)
	}

	err = st.RecordAuditLog(protocol.AuditLogRecord{
		LogID:       "log_1",
		DeviceID:    "asus_laptop",
		ActionType:  "EXEC_CMD",
		CommandText: "Get-Date",
		Timestamp:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("RecordAuditLog failed: %v", err)
	}

	logs, err := st.ListAuditLogs("asus_laptop", 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("ListAuditLogs failed: %v, got=%+v", err, logs)
	}
}
