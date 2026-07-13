CREATE TABLE IF NOT EXISTS venue_credentials (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    venue_type TEXT NOT NULL CHECK (venue_type IN ('binance','mgbx')),
    api_key_cipher BYTEA NOT NULL,
    api_secret_cipher BYTEA NOT NULL,
    api_key_last4 TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by BIGINT REFERENCES users(id),
    updated_by BIGINT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS venue_credentials_venue_type_idx ON venue_credentials(venue_type,enabled);

INSERT INTO permissions(code,name) VALUES ('secrets:manage','管理交易所凭证') ON CONFLICT(code) DO NOTHING;
INSERT INTO role_permissions(role_id,permission_code)
SELECT id,'secrets:manage' FROM roles WHERE code='super_admin'
ON CONFLICT DO NOTHING;

