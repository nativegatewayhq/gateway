ALTER TABLE chat_token_prices DROP CONSTRAINT chat_token_prices_protocol_check;
ALTER TABLE chat_token_prices ADD CONSTRAINT chat_token_prices_protocol_check CHECK (protocol IN ('openai','gemini'));
ALTER TABLE chat_token_prices ADD CONSTRAINT chat_token_prices_protocol_operation_check CHECK (protocol='openai' OR operation='chat.completions');

ALTER TABLE chat_token_price_publications ADD COLUMN protocol text NOT NULL DEFAULT 'openai';
ALTER TABLE chat_token_price_publications ALTER COLUMN protocol DROP DEFAULT;
ALTER TABLE chat_token_price_publications ADD CONSTRAINT chat_token_price_publications_protocol_check CHECK (protocol IN ('openai','gemini'));
ALTER TABLE chat_token_price_publications ADD CONSTRAINT chat_token_price_publications_protocol_operation_check CHECK (protocol='openai' OR operation='chat.completions');
ALTER TABLE chat_token_price_publications DROP CONSTRAINT chat_token_price_publications_pkey;
ALTER TABLE chat_token_price_publications ADD PRIMARY KEY(protocol,operation,publication_key);

ALTER TABLE chat_request_charges DROP CONSTRAINT chat_request_charges_protocol_check;
ALTER TABLE chat_request_charges ADD CONSTRAINT chat_request_charges_protocol_check CHECK (protocol IN ('openai','gemini'));
ALTER TABLE chat_request_charges ADD CONSTRAINT chat_request_charges_protocol_operation_check CHECK (protocol='openai' OR operation='chat.completions');
ALTER TABLE chat_request_charges DROP CONSTRAINT chat_request_charges_organization_id_request_id_key;
ALTER TABLE chat_request_charges ADD CONSTRAINT chat_request_charges_request_identity_key UNIQUE(organization_id,protocol,operation,request_id);
DROP INDEX chat_request_charges_idempotency_idx;
CREATE UNIQUE INDEX chat_request_charges_idempotency_idx ON chat_request_charges(organization_id,protocol,operation,idempotency_key) WHERE idempotency_key IS NOT NULL;

ALTER TABLE chat_usage_evidence DROP CONSTRAINT chat_usage_evidence_schema_version_check;
ALTER TABLE chat_usage_evidence ADD CONSTRAINT chat_usage_evidence_schema_version_check CHECK (schema_version IN ('openai-chat-usage-v1','openai-responses-usage-v1','openai-responses-stream-usage-v1','gemini-usage-v1'));
ALTER TABLE chat_usage_evidence ADD COLUMN tool_use_prompt_tokens bigint NOT NULL DEFAULT 0 CHECK (tool_use_prompt_tokens >= 0 AND tool_use_prompt_tokens <= prompt_tokens);
ALTER TABLE chat_usage_evidence ADD COLUMN thoughts_tokens bigint NOT NULL DEFAULT 0 CHECK (thoughts_tokens >= 0 AND thoughts_tokens <= completion_tokens);

ALTER TABLE chat_charge_reconciliations ADD COLUMN tool_use_prompt_tokens bigint;
ALTER TABLE chat_charge_reconciliations ADD COLUMN thoughts_tokens bigint;
ALTER TABLE chat_charge_reconciliations ADD CONSTRAINT chat_charge_reconciliations_gemini_usage_check CHECK (
    (tool_use_prompt_tokens IS NULL AND thoughts_tokens IS NULL) OR
    (reason='settlement_failed' AND prompt_tokens IS NOT NULL AND completion_tokens IS NOT NULL AND tool_use_prompt_tokens BETWEEN 0 AND prompt_tokens AND thoughts_tokens BETWEEN 0 AND completion_tokens)
);
CREATE OR REPLACE FUNCTION enforce_chat_reconciliation_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.charge_id,OLD.reason,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.prompt_tokens,OLD.cached_input_tokens,OLD.completion_tokens,OLD.tool_use_prompt_tokens,OLD.thoughts_tokens,OLD.terminal_event_sha256,OLD.created_at) IS DISTINCT FROM ROW(NEW.charge_id,NEW.reason,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.prompt_tokens,NEW.cached_input_tokens,NEW.completion_tokens,NEW.tool_use_prompt_tokens,NEW.thoughts_tokens,NEW.terminal_event_sha256,NEW.created_at) THEN RAISE EXCEPTION 'chat reconciliation evidence is immutable' USING ERRCODE='55000'; END IF;
 IF OLD.state='RESOLVED' AND ROW(OLD.state,OLD.attempt_count,OLD.next_attempt_at,OLD.lease_owner,OLD.lease_until,OLD.last_error_category,OLD.resolved_at) IS DISTINCT FROM ROW(NEW.state,NEW.attempt_count,NEW.next_attempt_at,NEW.lease_owner,NEW.lease_until,NEW.last_error_category,NEW.resolved_at) THEN RAISE EXCEPTION 'resolved chat reconciliation is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;

ALTER TABLE cost_quota_policies DROP CONSTRAINT cost_quota_policies_dimension_check;
ALTER TABLE cost_quota_policies ADD CONSTRAINT cost_quota_policies_dimension_check CHECK ((protocol IS NULL AND operation IS NULL AND model IS NULL) OR (((protocol='openai' AND operation IN ('image.generate','image.edit','chat.completions','responses.create')) OR (protocol='gemini' AND operation IN ('image.generate','chat.completions')) OR (protocol IN ('replicate','fal') AND operation='image.generate')) AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model)));
