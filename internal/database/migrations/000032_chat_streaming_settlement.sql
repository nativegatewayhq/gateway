ALTER TABLE chat_request_charges
    ADD COLUMN delivery_mode text NOT NULL DEFAULT 'non_stream' CHECK (delivery_mode IN ('non_stream','stream')),
    ADD COLUMN stream_completed boolean NOT NULL DEFAULT false,
    ADD CHECK ((delivery_mode='non_stream' AND NOT stream_completed) OR delivery_mode='stream');

ALTER TABLE chat_usage_evidence
    ADD COLUMN delivery_mode text NOT NULL DEFAULT 'non_stream' CHECK (delivery_mode IN ('non_stream','stream')),
    ADD COLUMN terminal_event_sha256 bytea CHECK (terminal_event_sha256 IS NULL OR octet_length(terminal_event_sha256)=32),
    ADD CHECK ((delivery_mode='non_stream' AND terminal_event_sha256 IS NULL) OR (delivery_mode='stream' AND terminal_event_sha256 IS NOT NULL));

ALTER TABLE chat_charge_reconciliations
    DROP CONSTRAINT chat_charge_reconciliations_reason_check,
    ADD CONSTRAINT chat_charge_reconciliations_reason_check CHECK (reason IN ('executor_timeout','executor_connection_lost','response_unavailable','usage_invalid','settlement_failed','provider_panic','client_disconnect','stream_protocol_invalid','stream_usage_missing','stream_write_failed')),
    ADD COLUMN disconnect_side text CHECK (disconnect_side IS NULL OR disconnect_side IN ('client','provider')),
    ADD COLUMN terminal_category text CHECK (terminal_category IS NULL OR terminal_category IN ('complete','missing_usage','invalid_usage','missing_done','write_failed','provider_error','client_disconnect')),
    ADD COLUMN terminal_event_sha256 bytea CHECK (terminal_event_sha256 IS NULL OR octet_length(terminal_event_sha256)=32);

CREATE OR REPLACE FUNCTION enforce_chat_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.maximum_input_tokens,OLD.maximum_output_tokens,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.idempotency_key,OLD.request_fingerprint,OLD.delivery_mode,OLD.created_at) IS DISTINCT FROM ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.maximum_input_tokens,NEW.maximum_output_tokens,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.idempotency_key,NEW.request_fingerprint,NEW.delivery_mode,NEW.created_at) THEN RAISE EXCEPTION 'chat charge identity is immutable' USING ERRCODE='55000'; END IF;
 IF NOT (NEW.state=OLD.state OR (OLD.state IN ('RESERVED','RECONCILING') AND NEW.state IN ('CAPTURED','RELEASED','RECONCILING'))) THEN RAISE EXCEPTION 'invalid chat charge transition' USING ERRCODE='55000'; END IF;
 IF OLD.state IN ('CAPTURED','RELEASED') AND ROW(OLD.actual_cost,OLD.captured_sale,OLD.response_snapshot_version,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.response_completed_at,OLD.stream_completed) IS DISTINCT FROM ROW(NEW.actual_cost,NEW.captured_sale,NEW.response_snapshot_version,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.response_completed_at,NEW.stream_completed) THEN RAISE EXCEPTION 'terminal chat charge is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION enforce_chat_reconciliation_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.charge_id,OLD.reason,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.prompt_tokens,OLD.cached_input_tokens,OLD.completion_tokens,OLD.disconnect_side,OLD.terminal_category,OLD.terminal_event_sha256,OLD.created_at) IS DISTINCT FROM ROW(NEW.charge_id,NEW.reason,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.prompt_tokens,NEW.cached_input_tokens,NEW.completion_tokens,NEW.disconnect_side,NEW.terminal_category,NEW.terminal_event_sha256,NEW.created_at) THEN RAISE EXCEPTION 'chat reconciliation evidence is immutable' USING ERRCODE='55000'; END IF;
 IF OLD.state='RESOLVED' AND ROW(OLD.state,OLD.attempt_count,OLD.next_attempt_at,OLD.lease_owner,OLD.lease_until,OLD.last_error_category,OLD.resolved_at) IS DISTINCT FROM ROW(NEW.state,NEW.attempt_count,NEW.next_attempt_at,NEW.lease_owner,NEW.lease_until,NEW.last_error_category,NEW.resolved_at) THEN RAISE EXCEPTION 'resolved chat reconciliation is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
