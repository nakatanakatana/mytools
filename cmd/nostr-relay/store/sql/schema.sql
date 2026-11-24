CREATE TABLE events (
    id TEXT PRIMARY KEY,
    pubkey TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    kind INTEGER NOT NULL,
    event_json TEXT NOT NULL
);
CREATE INDEX events_created_at_id_idx ON events(created_at DESC, id);
CREATE INDEX events_pubkey_kind_created_at_idx ON events(pubkey, kind, created_at DESC, id);
CREATE INDEX events_kind_created_at_idx ON events(kind, created_at DESC, id);

CREATE TABLE event_tags (
    event_id TEXT NOT NULL,
    tag_name TEXT NOT NULL,
    tag_value TEXT NOT NULL,
    PRIMARY KEY (event_id, tag_name, tag_value),
    FOREIGN KEY (event_id) REFERENCES events(id) ON DELETE CASCADE
);
CREATE INDEX event_tags_name_value_event_idx ON event_tags(tag_name, tag_value, event_id);

CREATE TABLE relay_meta (
    "key" TEXT PRIMARY KEY,
    "value" TEXT NOT NULL
);

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
