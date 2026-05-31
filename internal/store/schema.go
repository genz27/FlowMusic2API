package store

const sqliteSchema = `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = NORMAL;
PRAGMA cache_size = -8000;
PRAGMA temp_store = MEMORY;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS admin_config (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    api_key TEXT NOT NULL,
    debug_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    error_ban_threshold INTEGER NOT NULL DEFAULT 3,
    guest_trial_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    max_daily_guest_uses INTEGER NOT NULL DEFAULT 3,
    guest_global_daily_limit INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cache_config (
    id INTEGER PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    timeout INTEGER NOT NULL DEFAULT 7200,
    base_url TEXT NOT NULL DEFAULT '',
    storage_mode TEXT NOT NULL DEFAULT 'local',
    s3_endpoint TEXT NOT NULL DEFAULT '',
    s3_region TEXT NOT NULL DEFAULT '',
    s3_bucket TEXT NOT NULL DEFAULT '',
    s3_access_key TEXT NOT NULL DEFAULT '',
    s3_secret_key TEXT NOT NULL DEFAULT '',
    s3_use_ssl BOOLEAN NOT NULL DEFAULT TRUE,
    s3_force_path_style BOOLEAN NOT NULL DEFAULT FALSE,
    s3_prefix TEXT NOT NULL DEFAULT '',
    s3_public_base_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS generation_config (
    id INTEGER PRIMARY KEY,
    timeout INTEGER NOT NULL DEFAULT 600,
    max_retries INTEGER NOT NULL DEFAULT 3,
    image_timeout INTEGER NOT NULL DEFAULT 600,
    video_timeout INTEGER NOT NULL DEFAULT 600,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS token_refresh_config (
    id INTEGER PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    at_auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    refresh_interval_minutes INTEGER NOT NULL DEFAULT 60,
    refresh_before_expiry_seconds INTEGER NOT NULL DEFAULT 600,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_config (
    id INTEGER PRIMARY KEY,
    proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    proxy_url TEXT NOT NULL DEFAULT '',
    media_proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    media_proxy_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS call_logic_config (
    id INTEGER PRIMARY KEY,
    call_mode TEXT NOT NULL DEFAULT 'default',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    protocol_mode TEXT NOT NULL DEFAULT 'refresh_token',
    refresh_token TEXT NOT NULL DEFAULT '',
    access_token TEXT NOT NULL DEFAULT '',
    provider_token TEXT NOT NULL DEFAULT '',
    provider_refresh_token TEXT NOT NULL DEFAULT '',
    flow_bearer TEXT NOT NULL DEFAULT '',
    cookies TEXT NOT NULL DEFAULT '',
    login_account TEXT NOT NULL DEFAULT '',
    login_password TEXT NOT NULL DEFAULT '',
    proxy_url TEXT NOT NULL DEFAULT '',
    auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    refresh_interval_minutes INTEGER NOT NULL DEFAULT 60,
    expires_at TIMESTAMP NULL,
    at_expires TIMESTAMP NULL,
    last_refresh_at TIMESTAMP NULL,
    last_refresh_result TEXT NOT NULL DEFAULT '',
    last_used_at TIMESTAMP NULL,
    credits INTEGER NOT NULL DEFAULT 0,
    tokens_remaining REAL NOT NULL DEFAULT 0,
    subscription_tier TEXT NOT NULL DEFAULT '',
    use_count INTEGER NOT NULL DEFAULT 0,
    music_count INTEGER NOT NULL DEFAULT 0,
    today_music_count INTEGER NOT NULL DEFAULT 0,
    image_count INTEGER NOT NULL DEFAULT 0,
    video_count INTEGER NOT NULL DEFAULT 0,
    error_count INTEGER NOT NULL DEFAULT 0,
    today_error_count INTEGER NOT NULL DEFAULT 0,
    consecutive_error_count INTEGER NOT NULL DEFAULT 0,
    today_date TEXT NOT NULL DEFAULT '',
    image_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    video_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    upscale_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    image_concurrency INTEGER NOT NULL DEFAULT -1,
    video_concurrency INTEGER NOT NULL DEFAULT -1,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS request_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id INTEGER NULL REFERENCES accounts(id) ON DELETE SET NULL,
    operation TEXT NOT NULL,
    request_body TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    response_body_excerpt TEXT NOT NULL DEFAULT '',
    status_code INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    status_text TEXT NOT NULL DEFAULT '',
    progress INTEGER NOT NULL DEFAULT 0,
    error_summary TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS guest_usage (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    client_ip TEXT NOT NULL,
    date TEXT NOT NULL,
    use_count INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(client_ip, date)
);

CREATE INDEX IF NOT EXISTS idx_accounts_active_used ON accounts(is_active, last_used_at, id);
CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_guest_usage_date ON guest_usage(date);
`

const postgresSchema = `
CREATE TABLE IF NOT EXISTS admin_config (
    id BIGINT PRIMARY KEY,
    username TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    api_key TEXT NOT NULL,
    debug_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    error_ban_threshold INTEGER NOT NULL DEFAULT 3,
    guest_trial_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    max_daily_guest_uses INTEGER NOT NULL DEFAULT 3,
    guest_global_daily_limit INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cache_config (
    id BIGINT PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    timeout INTEGER NOT NULL DEFAULT 7200,
    base_url TEXT NOT NULL DEFAULT '',
    storage_mode TEXT NOT NULL DEFAULT 'local',
    s3_endpoint TEXT NOT NULL DEFAULT '',
    s3_region TEXT NOT NULL DEFAULT '',
    s3_bucket TEXT NOT NULL DEFAULT '',
    s3_access_key TEXT NOT NULL DEFAULT '',
    s3_secret_key TEXT NOT NULL DEFAULT '',
    s3_use_ssl BOOLEAN NOT NULL DEFAULT TRUE,
    s3_force_path_style BOOLEAN NOT NULL DEFAULT FALSE,
    s3_prefix TEXT NOT NULL DEFAULT '',
    s3_public_base_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS generation_config (
    id BIGINT PRIMARY KEY,
    timeout INTEGER NOT NULL DEFAULT 600,
    max_retries INTEGER NOT NULL DEFAULT 3,
    image_timeout INTEGER NOT NULL DEFAULT 600,
    video_timeout INTEGER NOT NULL DEFAULT 600,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS token_refresh_config (
    id BIGINT PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    at_auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    refresh_interval_minutes INTEGER NOT NULL DEFAULT 60,
    refresh_before_expiry_seconds INTEGER NOT NULL DEFAULT 600,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_config (
    id BIGINT PRIMARY KEY,
    proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    proxy_url TEXT NOT NULL DEFAULT '',
    media_proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    media_proxy_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS call_logic_config (
    id BIGINT PRIMARY KEY,
    call_mode TEXT NOT NULL DEFAULT 'default',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS accounts (
    id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    protocol_mode TEXT NOT NULL DEFAULT 'refresh_token',
    refresh_token TEXT NOT NULL DEFAULT '',
    access_token TEXT NOT NULL DEFAULT '',
    provider_token TEXT NOT NULL DEFAULT '',
    provider_refresh_token TEXT NOT NULL DEFAULT '',
    flow_bearer TEXT NOT NULL DEFAULT '',
    cookies TEXT NOT NULL DEFAULT '',
    login_account TEXT NOT NULL DEFAULT '',
    login_password TEXT NOT NULL DEFAULT '',
    proxy_url TEXT NOT NULL DEFAULT '',
    auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    refresh_interval_minutes INTEGER NOT NULL DEFAULT 60,
    expires_at TIMESTAMPTZ NULL,
    at_expires TIMESTAMPTZ NULL,
    last_refresh_at TIMESTAMPTZ NULL,
    last_refresh_result TEXT NOT NULL DEFAULT '',
    last_used_at TIMESTAMPTZ NULL,
    credits INTEGER NOT NULL DEFAULT 0,
    tokens_remaining DOUBLE PRECISION NOT NULL DEFAULT 0,
    subscription_tier TEXT NOT NULL DEFAULT '',
    use_count INTEGER NOT NULL DEFAULT 0,
    music_count INTEGER NOT NULL DEFAULT 0,
    today_music_count INTEGER NOT NULL DEFAULT 0,
    image_count INTEGER NOT NULL DEFAULT 0,
    video_count INTEGER NOT NULL DEFAULT 0,
    error_count INTEGER NOT NULL DEFAULT 0,
    today_error_count INTEGER NOT NULL DEFAULT 0,
    consecutive_error_count INTEGER NOT NULL DEFAULT 0,
    today_date TEXT NOT NULL DEFAULT '',
    image_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    video_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    upscale_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    image_concurrency INTEGER NOT NULL DEFAULT -1,
    video_concurrency INTEGER NOT NULL DEFAULT -1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS request_logs (
    id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    account_id BIGINT NULL REFERENCES accounts(id) ON DELETE SET NULL,
    operation TEXT NOT NULL,
    request_body TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    response_body_excerpt TEXT NOT NULL DEFAULT '',
    status_code INTEGER NOT NULL DEFAULT 0,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    status_text TEXT NOT NULL DEFAULT '',
    progress INTEGER NOT NULL DEFAULT 0,
    error_summary TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS guest_usage (
    id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    client_ip TEXT NOT NULL,
    date TEXT NOT NULL,
    use_count INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(client_ip, date)
);

CREATE INDEX IF NOT EXISTS idx_accounts_active_used ON accounts(is_active, last_used_at, id);
CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_guest_usage_date ON guest_usage(date);
`
