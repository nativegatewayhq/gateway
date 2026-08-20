ALTER TABLE service_api_keys
    ADD COLUMN model_access_mode text NOT NULL DEFAULT 'all'
    CHECK (model_access_mode IN ('all', 'allowlist'));

CREATE TABLE service_api_key_model_permissions (
    api_key_id text NOT NULL REFERENCES service_api_keys(id) ON DELETE CASCADE,
    protocol text NOT NULL CHECK (protocol IN ('openai', 'gemini')),
    operation text NOT NULL CHECK (
        (protocol = 'openai' AND operation IN ('image.generate', 'image.edit')) OR
        (protocol = 'gemini' AND operation = 'image.generate')
    ),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model = btrim(model)),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (api_key_id, protocol, operation, model)
);
