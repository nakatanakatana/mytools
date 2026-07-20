CREATE TABLE bridge_events (
    provider TEXT NOT NULL,
    source_account TEXT NOT NULL,
    source_uri TEXT NOT NULL,
    nostr_event_id TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    author_pubkey TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(provider, source_account, source_uri)
);
CREATE INDEX bridge_events_author_pubkey_idx ON bridge_events(author_pubkey);

CREATE TABLE publisher_registrations (
    pubkey TEXT PRIMARY KEY,
    registered_at INTEGER NOT NULL
);

CREATE TABLE publisher_purges (
    pubkey TEXT PRIMARY KEY,
    deletion_event_id TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE sync_targets (
    provider TEXT NOT NULL,
    source_account TEXT NOT NULL,
    target TEXT NOT NULL,
    PRIMARY KEY(provider, source_account, target)
);

CREATE TABLE outbox_sequences (
    aggregate_key TEXT PRIMARY KEY,
    last_sequence INTEGER NOT NULL
);

CREATE TABLE outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    aggregate_key TEXT NOT NULL,
    `sequence` INTEGER NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('allow-publisher', 'publish-event', 'unallow-publisher')),
    pubkey TEXT NOT NULL,
    payload TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    available_at INTEGER NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    claim_token TEXT NOT NULL DEFAULT '',
    claimed_until INTEGER NOT NULL DEFAULT 0,
    UNIQUE(aggregate_key, `sequence`)
);
CREATE INDEX outbox_claim_idx ON outbox(available_at, claim_token, claimed_until, id);
CREATE INDEX outbox_aggregate_sequence_idx ON outbox(aggregate_key, `sequence`);

CREATE TABLE sync_cursors (
    provider TEXT NOT NULL,
    source_account TEXT NOT NULL,
    name TEXT NOT NULL,
    `value` TEXT NOT NULL,
    PRIMARY KEY(provider, source_account, name)
);

CREATE TABLE oauth_sessions (
    provider TEXT NOT NULL,
    source_account TEXT NOT NULL,
    state TEXT NOT NULL,
    encrypted_payload BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    PRIMARY KEY(provider, source_account, state)
);

CREATE TABLE oauth_tokens (
    provider TEXT NOT NULL,
    source_account TEXT NOT NULL,
    account_did TEXT NOT NULL,
    encrypted_payload BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(provider, source_account, account_did)
);

CREATE TABLE source_operations (
    provider TEXT NOT NULL,
    source_account TEXT NOT NULL,
    source_uri TEXT NOT NULL,
    `identity` TEXT NOT NULL,
    PRIMARY KEY(provider, source_account, source_uri)
);
