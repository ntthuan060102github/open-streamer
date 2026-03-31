-- Open Streamer persistence (PostgreSQL).
-- Domain documents are stored as JSONB for forward compatibility with the API model.

CREATE TABLE IF NOT EXISTS streams (
    id         TEXT PRIMARY KEY,
    data       JSONB        NOT NULL,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS recordings (
    id         TEXT PRIMARY KEY,
    stream_id  TEXT         NOT NULL,
    data       JSONB        NOT NULL,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS recordings_stream_id_idx ON recordings (stream_id);

CREATE TABLE IF NOT EXISTS hooks (
    id         TEXT PRIMARY KEY,
    data       JSONB        NOT NULL,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
