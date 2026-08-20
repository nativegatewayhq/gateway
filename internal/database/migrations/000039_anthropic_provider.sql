ALTER TABLE provider_channels DROP CONSTRAINT provider_channels_provider_check;
ALTER TABLE provider_channels ADD CONSTRAINT provider_channels_provider_check CHECK (
    provider IN ('google','openai','xai','replicate','fal','stability','anthropic')
);

INSERT INTO provider_channels(id,provider,name,status) VALUES
    ('channel_00000000000000000000000000000006','anthropic','built-in-anthropic','active')
ON CONFLICT(id) DO NOTHING;

ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (
    provider IN ('google','openai','xai','replicate','fal','anthropic')
);

ALTER TABLE provider_credential_events DROP CONSTRAINT provider_credential_events_provider_check;
ALTER TABLE provider_credential_events ADD CONSTRAINT provider_credential_events_provider_check CHECK (
    provider IN ('google','openai','xai','replicate','fal','anthropic')
);
