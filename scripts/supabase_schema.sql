-- Supabase PostgreSQL Schema for Terminal App Remote Relay
-- Run this script in the Supabase SQL Editor (https://supabase.com/dashboard/project/_/sql)

-- 1. Users Table (Credentials, Account Profiles & Pairing Tokens)
CREATE TABLE IF NOT EXISTS public.users (
    user_id       TEXT PRIMARY KEY,
    username      TEXT UNIQUE NOT NULL,
    password      TEXT NOT NULL,
    auth_token    TEXT UNIQUE NOT NULL,
    created_at    BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_users_token ON public.users(auth_token);
CREATE INDEX IF NOT EXISTS idx_users_username ON public.users(username);

-- 2. Devices Table (Registered Laptops & Live Telemetry)
CREATE TABLE IF NOT EXISTS public.devices (
    device_id      TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL DEFAULT '',
    hostname       TEXT,
    os             TEXT,
    status         TEXT NOT NULL DEFAULT 'OFFLINE',
    last_heartbeat BIGINT NOT NULL DEFAULT 0,
    connected_at   BIGINT NOT NULL DEFAULT 0,
    health_json    TEXT
);

CREATE INDEX IF NOT EXISTS idx_devices_user ON public.devices(user_id);
CREATE INDEX IF NOT EXISTS idx_devices_status ON public.devices(status);

-- 3. Cloud Command Queue (Offline Task Synchronization)
CREATE TABLE IF NOT EXISTS public.cloud_command_queue (
    command_id    TEXT PRIMARY KEY,
    device_id     TEXT NOT NULL,
    command       TEXT NOT NULL,
    timeout_sec   INTEGER DEFAULT 0,
    status        TEXT NOT NULL CHECK(status IN ('PENDING','DISPATCHED','COMPLETED','FAILED')),
    created_at    BIGINT NOT NULL,
    dispatched_at BIGINT DEFAULT 0,
    completed_at  BIGINT DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_cloud_cmd_dev_status ON public.cloud_command_queue(device_id, status, created_at);

-- 4. Cloud Result Queue (Historical Command Output)
CREATE TABLE IF NOT EXISTS public.cloud_result_queue (
    result_id   TEXT PRIMARY KEY,
    command_id  TEXT NOT NULL,
    device_id   TEXT NOT NULL,
    stdout      TEXT,
    stderr      TEXT,
    exit_code   INTEGER,
    executed_at BIGINT NOT NULL,
    received_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cloud_res_cmd ON public.cloud_result_queue(command_id);
CREATE INDEX IF NOT EXISTS idx_cloud_res_dev ON public.cloud_result_queue(device_id, executed_at DESC);

-- 5. Immutable Audit Logs
CREATE TABLE IF NOT EXISTS public.audit_logs (
    log_id       TEXT PRIMARY KEY,
    device_id    TEXT NOT NULL,
    timestamp    BIGINT NOT NULL,
    action_type  TEXT NOT NULL,
    command_text TEXT,
    exit_code    INTEGER DEFAULT 0,
    duration_ms  BIGINT DEFAULT 0,
    details      TEXT
);

CREATE INDEX IF NOT EXISTS idx_audit_dev ON public.audit_logs(device_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_time ON public.audit_logs(timestamp DESC);

-- Disable Row Level Security (RLS) or enable public access for backend API service role key
ALTER TABLE public.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.cloud_command_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.cloud_result_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.audit_logs ENABLE ROW LEVEL SECURITY;

-- Allow full access to authenticated service role
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE policyname = 'service_role_all_users') THEN
        CREATE POLICY service_role_all_users ON public.users FOR ALL USING (true) WITH CHECK (true);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE policyname = 'service_role_all_devices') THEN
        CREATE POLICY service_role_all_devices ON public.devices FOR ALL USING (true) WITH CHECK (true);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE policyname = 'service_role_all_cmd') THEN
        CREATE POLICY service_role_all_cmd ON public.cloud_command_queue FOR ALL USING (true) WITH CHECK (true);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE policyname = 'service_role_all_res') THEN
        CREATE POLICY service_role_all_res ON public.cloud_result_queue FOR ALL USING (true) WITH CHECK (true);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE policyname = 'service_role_all_audit') THEN
        CREATE POLICY service_role_all_audit ON public.audit_logs FOR ALL USING (true) WITH CHECK (true);
    END IF;
END $$;
