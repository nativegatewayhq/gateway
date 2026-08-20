ALTER TABLE chat_token_prices DROP CONSTRAINT chat_token_prices_operation_check;
ALTER TABLE chat_token_prices ADD CONSTRAINT chat_token_prices_operation_check CHECK (operation IN ('chat.completions','responses.create'));
ALTER TABLE chat_token_price_publications ADD COLUMN operation text NOT NULL DEFAULT 'chat.completions';
ALTER TABLE chat_token_price_publications ALTER COLUMN operation DROP DEFAULT;
ALTER TABLE chat_token_price_publications ADD CONSTRAINT chat_token_price_publications_operation_check CHECK (operation IN ('chat.completions','responses.create'));
ALTER TABLE chat_token_price_publications DROP CONSTRAINT chat_token_price_publications_pkey;
ALTER TABLE chat_token_price_publications ADD PRIMARY KEY(operation,publication_key);
ALTER TABLE chat_request_charges DROP CONSTRAINT chat_request_charges_operation_check;
ALTER TABLE chat_request_charges ADD CONSTRAINT chat_request_charges_operation_check CHECK (operation IN ('chat.completions','responses.create'));
ALTER TABLE chat_usage_evidence DROP CONSTRAINT chat_usage_evidence_schema_version_check;
ALTER TABLE chat_usage_evidence ADD CONSTRAINT chat_usage_evidence_schema_version_check CHECK (schema_version IN ('openai-chat-usage-v1','openai-responses-usage-v1'));

ALTER TABLE cost_quota_policies DROP CONSTRAINT cost_quota_policies_dimension_check;
ALTER TABLE cost_quota_policies ADD CONSTRAINT cost_quota_policies_dimension_check CHECK ((protocol IS NULL AND operation IS NULL AND model IS NULL) OR (((protocol='openai' AND operation IN ('image.generate','image.edit','chat.completions','responses.create')) OR (protocol IN ('gemini','replicate','fal') AND operation='image.generate')) AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model)));

ALTER TABLE chat_charge_reconciliations DROP CONSTRAINT chat_charge_reconciliations_reason_check;
ALTER TABLE chat_charge_reconciliations ADD CONSTRAINT chat_charge_reconciliations_reason_check CHECK (reason IN ('executor_timeout','executor_connection_lost','response_unavailable','usage_invalid','settlement_failed','provider_panic','client_disconnect','stream_protocol_invalid','stream_usage_missing','stream_write_failed'));
