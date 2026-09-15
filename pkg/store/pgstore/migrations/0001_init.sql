-- Initial schema for the document cache and the two history logs.
--
-- Boundary rule: a typed column exists only because some query filters, joins
-- or sorts on it. Everything else lives in the payload, which nothing looks
-- inside. That is why the payload columns are `json` and not `jsonb`: jsonb
-- exists to be indexed, no index here reaches into a payload, and `json` keeps
-- the exact bytes the tool returned while still rejecting invalid JSON.
--
-- schema_migrations is created by the runner, not here, because the runner has
-- to read it before it can apply this file.

CREATE TABLE IF NOT EXISTS accounts (
    account_id text PRIMARY KEY,          -- XHS user id, not the fingerprint seed
    seed       bigint NOT NULL,           -- fingerprint seed active when last seen
    nickname   text NOT NULL DEFAULT '',
    first_seen timestamptz NOT NULL DEFAULT now(),
    last_seen  timestamptz NOT NULL DEFAULT now()
);

-- Startup recovery asks for the account most recently seen under one seed.
CREATE INDEX IF NOT EXISTS accounts_seed_idx ON accounts (seed, last_seen DESC);

CREATE TABLE IF NOT EXISTS documents (
    account_id text NOT NULL,
    kind       text NOT NULL,             -- note | note_full | profile | my_profile | feed | search | notifications | unread
    key        text NOT NULL,
    payload    json NOT NULL,             -- what the tool would have returned
    meta       json NOT NULL DEFAULT '{}',-- read in Go to decide whether a hit is usable; never queried
    fetched_at timestamptz NOT NULL,      -- when the site was read, not when the row was written
    PRIMARY KEY (account_id, kind, key)
);

-- Retention deletes by age across every account, so the index is on the age
-- alone.
CREATE INDEX IF NOT EXISTS documents_fetched_at_idx ON documents (fetched_at);

CREATE TABLE IF NOT EXISTS notifications (
    seq             bigserial PRIMARY KEY,
    account_id      text NOT NULL,
    tab             text NOT NULL,
    notification_id text NOT NULL,
    from_user_id    text NOT NULL DEFAULT '',
    note_id         text NOT NULL DEFAULT '',
    comment_id      text NOT NULL DEFAULT '',
    payload         json NOT NULL,
    first_seen      timestamptz NOT NULL DEFAULT now(),
    -- The tab is part of the identity: notification-id uniqueness across tabs
    -- is unconfirmed, and merging two tabs on that assumption would lose rows.
    UNIQUE (account_id, tab, notification_id)
);

CREATE INDEX IF NOT EXISTS notifications_cursor_idx ON notifications (account_id, tab, seq);
CREATE INDEX IF NOT EXISTS notifications_comment_idx ON notifications (account_id, comment_id) WHERE comment_id <> '';

CREATE TABLE IF NOT EXISTS comments (
    seq        bigserial PRIMARY KEY,
    account_id text NOT NULL,
    note_id    text NOT NULL,
    comment_id text NOT NULL,
    parent_id  text NOT NULL DEFAULT '',  -- a reply is its own row, never nested in its parent
    author_id  text NOT NULL DEFAULT '',
    payload    json NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, note_id, comment_id)
);

CREATE INDEX IF NOT EXISTS comments_note_idx ON comments (account_id, note_id, seq);
