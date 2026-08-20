ALTER TABLE service_api_keys
    ADD COLUMN network_access_mode text NOT NULL DEFAULT 'all'
    CHECK (network_access_mode IN ('all', 'allowlist'));

CREATE TABLE service_api_key_network_prefixes (
    api_key_id text NOT NULL REFERENCES service_api_keys(id) ON DELETE CASCADE,
    prefix cidr NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (api_key_id, prefix)
);
