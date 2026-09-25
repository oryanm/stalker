-- Times are INTEGER unix milliseconds UTC, 0 means the zero time.

-- AUTOINCREMENT so a deleted follow's id is never reused by a stale page or SSE client.
CREATE TABLE follows (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    url             TEXT    NOT NULL,
    feed_url        TEXT    NOT NULL UNIQUE,
    title           TEXT    NOT NULL,
    feed_title      TEXT    NOT NULL,
    description     TEXT    NOT NULL,
    photo_url       TEXT    NOT NULL,
    importance      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    edited_at       INTEGER NOT NULL,
    etag            TEXT    NOT NULL,
    last_modified   TEXT    NOT NULL,
    last_fetched_at INTEGER NOT NULL,
    next_fetch_at   INTEGER NOT NULL,
    last_error      TEXT    NOT NULL,
    error_count     INTEGER NOT NULL,
    last_post_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX follows_next_fetch_at ON follows (next_fetch_at);

CREATE TABLE follow_tags (
    follow_id INTEGER NOT NULL REFERENCES follows (id),
    tag       TEXT    NOT NULL,
    PRIMARY KEY (follow_id, tag)
) STRICT, WITHOUT ROWID;

CREATE INDEX follow_tags_tag ON follow_tags (tag);

CREATE TABLE posts (
    id            INTEGER PRIMARY KEY,
    follow_id     INTEGER NOT NULL REFERENCES follows (id),
    guid          TEXT    NOT NULL,
    url           TEXT    NOT NULL,
    title         TEXT    NOT NULL,
    author        TEXT    NOT NULL,
    published_at  INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    first_seen_at INTEGER NOT NULL,
    UNIQUE (follow_id, guid)
) STRICT;

-- id breaks ties between posts of one fetch that share a date (undated feeds).
CREATE INDEX posts_follow_published ON posts (follow_id, published_at DESC, id DESC);

CREATE TABLE settings (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
) STRICT, WITHOUT ROWID;
