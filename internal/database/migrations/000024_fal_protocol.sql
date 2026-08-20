INSERT INTO provider_channels(id,provider,name,status) VALUES
    ('channel_00000000000000000000000000000005','fal','built-in-fal','active')
ON CONFLICT(id) DO NOTHING;

ALTER TABLE service_api_key_model_permissions DROP CONSTRAINT service_api_key_model_permissions_protocol_check;
ALTER TABLE service_api_key_model_permissions ADD CONSTRAINT service_api_key_model_permissions_protocol_check CHECK (protocol IN ('openai','gemini','replicate','fal'));
ALTER TABLE service_api_key_model_permissions DROP CONSTRAINT service_api_key_model_permissions_operation_check;
ALTER TABLE service_api_key_model_permissions ADD CONSTRAINT service_api_key_model_permissions_operation_check CHECK (
    (protocol='openai' AND operation IN ('image.generate','image.edit')) OR
    (protocol IN ('gemini','replicate','fal') AND operation='image.generate')
);

ALTER TABLE image_request_charges DROP CONSTRAINT image_request_charges_protocol_check;
ALTER TABLE image_request_charges ADD CONSTRAINT image_request_charges_protocol_check CHECK (protocol IN ('openai','gemini','replicate','fal'));

ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_provider_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_provider_check CHECK (provider IN ('google','openai','xai','replicate','fal'));
ALTER TABLE provider_credential_events DROP CONSTRAINT provider_credential_events_provider_check;
ALTER TABLE provider_credential_events ADD CONSTRAINT provider_credential_events_provider_check CHECK (provider IN ('google','openai','xai','replicate','fal'));

ALTER TABLE cost_quota_policies DROP CONSTRAINT cost_quota_policies_dimension_check;
ALTER TABLE cost_quota_policies ADD CONSTRAINT cost_quota_policies_dimension_check CHECK (
    (protocol IS NULL AND operation IS NULL AND model IS NULL) OR
    ((((protocol='openai' AND operation IN ('image.generate','image.edit')) OR (protocol IN ('gemini','replicate','fal') AND operation='image.generate'))
        AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model)))
);
