-- +goose Up
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    business_id   BIGINT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;