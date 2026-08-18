-- Google Photos Ambient API support. One ambient device (created via
-- photosambient.googleapis.com devices.create) per local frame, each with its
-- own OAuth authorization: the ambient flow is the OAuth device-code flow for
-- TVs/limited-input devices, so tokens can't share the singleton google_auth
-- row used by the picker integration, and each frame gets its own photo
-- selection (made by the user in the Google Photos app, not in this UI).
CREATE TABLE IF NOT EXISTS ambient_devices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id INTEGER NOT NULL,
    -- UUIDv4 sent as the device-code `state` requestId and reused on retries so
    -- devices.create stays idempotent.
    request_id TEXT NOT NULL DEFAULT '',
    google_device_id TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    -- Deep link into the Google Photos app for choosing/changing which photos
    -- feed this frame; kept as a fallback even in the streamlined QR flow.
    settings_uri TEXT NOT NULL DEFAULT '',
    media_sources_set INTEGER NOT NULL DEFAULT 0,
    -- Server-provided polling interval for devices.get, in seconds.
    poll_interval_seconds INTEGER NOT NULL DEFAULT 0,
    account_email TEXT NOT NULL DEFAULT '',
    access_token TEXT NOT NULL DEFAULT '',
    refresh_token TEXT NOT NULL DEFAULT '',
    expiry DATETIME,
    -- mediaItems.list is capped at 240 requests per device per day; the counter
    -- resets when list_calls_date rolls over (UTC date, YYYY-MM-DD).
    list_calls_date TEXT NOT NULL DEFAULT '',
    list_calls_count INTEGER NOT NULL DEFAULT 0,
    last_sync_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME,
    updated_at DATETIME,
    FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ambient_devices_device ON ambient_devices(device_id);
