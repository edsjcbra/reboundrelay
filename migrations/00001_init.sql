-- +goose Up
CREATE TABLE businesses (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    twilio_number TEXT UNIQUE,
    forward_to  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE contacts (
    id           BIGSERIAL PRIMARY KEY,
    business_id  BIGINT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
    phone        TEXT NOT NULL,
    opted_out    BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (business_id, phone)
);

CREATE TABLE messages (
    id           BIGSERIAL PRIMARY KEY,
    business_id  BIGINT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
    contact_id   BIGINT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    direction    TEXT NOT NULL CHECK (direction IN ('inbound','outbound')),
    body         TEXT NOT NULL,
    twilio_sid   TEXT UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_messages_contact ON messages(business_id, contact_id, created_at);

-- +goose Down
DROP TABLE messages;
DROP TABLE contacts;
DROP TABLE businesses;