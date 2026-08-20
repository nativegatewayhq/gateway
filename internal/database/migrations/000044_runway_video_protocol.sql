ALTER TABLE provider_channels DROP CONSTRAINT provider_channels_provider_check;
ALTER TABLE provider_channels ADD CONSTRAINT provider_channels_provider_check CHECK (
    provider IN ('google','openai','xai','replicate','fal','stability','anthropic','runway')
);

INSERT INTO provider_channels(id,provider,name,status) VALUES
    ('channel_00000000000000000000000000000007','runway','built-in-runway','active')
ON CONFLICT(id) DO NOTHING;

ALTER TABLE service_api_key_model_permissions DROP CONSTRAINT service_api_key_model_permissions_protocol_check;
ALTER TABLE service_api_key_model_permissions ADD CONSTRAINT service_api_key_model_permissions_protocol_check CHECK (protocol IN ('openai','gemini','replicate','fal','anthropic','runway'));
ALTER TABLE service_api_key_model_permissions DROP CONSTRAINT service_api_key_model_permissions_operation_check;
ALTER TABLE service_api_key_model_permissions ADD CONSTRAINT service_api_key_model_permissions_operation_check CHECK (
    (protocol='openai' AND operation IN ('image.generate','image.edit','chat.completions','responses.create')) OR
    (protocol='gemini' AND operation IN ('image.generate','chat.completions')) OR
    (protocol IN ('replicate','fal') AND operation='image.generate') OR
    (protocol='anthropic' AND operation='messages.create') OR
    (protocol='runway' AND operation='video.generate')
);

ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (provider IN ('google','openai','xai','replicate','fal','anthropic','runway'));
ALTER TABLE provider_credential_events DROP CONSTRAINT provider_credential_events_provider_check;
ALTER TABLE provider_credential_events ADD CONSTRAINT provider_credential_events_provider_check CHECK (provider IN ('google','openai','xai','replicate','fal','anthropic','runway'));
