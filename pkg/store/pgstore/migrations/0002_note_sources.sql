-- Note provenance: which surface handed out a note's xsec_token, and the
-- referrer that surface would have sent (issue #7, WS4; issue #10 for why the
-- pair has to come from one record rather than from an agent's guess).
--
-- This is a cache with a short window, not a log: the lookup filters on
-- seen_at so a record past its window is never returned, and the retention
-- sweep deletes it. One row per note per account, overwritten whenever the
-- note is seen again, so the primary key is all the index the lookup needs.

CREATE TABLE IF NOT EXISTS note_sources (
    account_id text NOT NULL,
    feed_id    text NOT NULL,
    source     text NOT NULL,            -- pc_feed | pc_search | pc_note
    referrer   text NOT NULL DEFAULT '',
    seen_at    timestamptz NOT NULL,     -- when the token was handed to us
    PRIMARY KEY (account_id, feed_id)
);

-- The sweep deletes by age across every account, so the index is on age alone.
CREATE INDEX IF NOT EXISTS note_sources_seen_at_idx ON note_sources (seen_at);
