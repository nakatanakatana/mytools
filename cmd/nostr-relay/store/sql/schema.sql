CREATE TABLE events (
    id TEXT PRIMARY KEY,
    pubkey TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    kind INTEGER NOT NULL,
    event_json TEXT NOT NULL
);
CREATE INDEX events_created_at_idx ON events(created_at);
CREATE INDEX events_pubkey_kind_idx ON events(pubkey, kind);

CREATE TABLE publisher_allowlist (
    pubkey TEXT PRIMARY KEY,
    reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE deletion_tombstones (
    owner_pubkey TEXT NOT NULL,
    target_event_id TEXT NOT NULL,
    deletion_event_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (owner_pubkey, target_event_id)
);

CREATE TABLE nip98_seen_events (
    event_id TEXT PRIMARY KEY,
    seen_at INTEGER NOT NULL
);
