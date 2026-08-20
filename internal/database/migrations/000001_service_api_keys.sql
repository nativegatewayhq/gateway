CREATE TABLE IF NOT EXISTS service_api_keys (
    id text PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    key_digest bytea NOT NULL UNIQUE CHECK (octet_length(key_digest) = 32),
    key_prefix text NOT NULL,
    hash_algorithm text NOT NULL DEFAULT 'sha256',
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS service_api_keys_active_digest_idx
    ON service_api_keys (key_digest)
    WHERE status = 'active';
