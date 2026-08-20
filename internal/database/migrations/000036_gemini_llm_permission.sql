ALTER TABLE service_api_key_model_permissions DROP CONSTRAINT service_api_key_model_permissions_operation_check;
ALTER TABLE service_api_key_model_permissions ADD CONSTRAINT service_api_key_model_permissions_operation_check CHECK (
    (protocol='openai' AND operation IN ('image.generate','image.edit','chat.completions','responses.create')) OR
    (protocol='gemini' AND operation IN ('image.generate','chat.completions')) OR
    (protocol IN ('replicate','fal') AND operation='image.generate')
);
