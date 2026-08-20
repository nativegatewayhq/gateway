ALTER TABLE chat_request_charges
    ADD COLUMN candidate_id text,
    ADD COLUMN provider text,
    ADD COLUMN provider_model text,
    ADD COLUMN routing_policy text,
    ADD COLUMN route_rank integer,
    ADD COLUMN price_evaluated_at timestamptz,
    ADD COLUMN route_evidence_version text;

ALTER TABLE chat_request_charges ADD CONSTRAINT chat_route_evidence_check CHECK (
    (candidate_id IS NULL AND provider IS NULL AND provider_model IS NULL AND routing_policy IS NULL AND route_rank IS NULL AND price_evaluated_at IS NULL AND route_evidence_version IS NULL)
    OR
    (length(candidate_id) BETWEEN 1 AND 200 AND candidate_id=btrim(candidate_id)
     AND provider IN ('openai','xai')
     AND length(provider_model) BETWEEN 1 AND 200 AND provider_model=btrim(provider_model)
     AND routing_policy IN ('fixed','priority','weighted','lowest_cost')
     AND route_rank >= 0
     AND price_evaluated_at IS NOT NULL
     AND route_evidence_version='openai-chat-route-v1')
);

CREATE OR REPLACE FUNCTION enforce_chat_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.maximum_input_tokens,OLD.maximum_output_tokens,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.idempotency_key,OLD.request_fingerprint,OLD.delivery_mode,OLD.candidate_id,OLD.provider,OLD.provider_model,OLD.routing_policy,OLD.route_rank,OLD.price_evaluated_at,OLD.route_evidence_version,OLD.created_at) IS DISTINCT FROM ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.maximum_input_tokens,NEW.maximum_output_tokens,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.idempotency_key,NEW.request_fingerprint,NEW.delivery_mode,NEW.candidate_id,NEW.provider,NEW.provider_model,NEW.routing_policy,NEW.route_rank,NEW.price_evaluated_at,NEW.route_evidence_version,NEW.created_at) THEN RAISE EXCEPTION 'chat charge identity is immutable' USING ERRCODE='55000'; END IF;
 IF NOT (NEW.state=OLD.state OR (OLD.state IN ('RESERVED','RECONCILING') AND NEW.state IN ('CAPTURED','RELEASED','RECONCILING'))) THEN RAISE EXCEPTION 'invalid chat charge transition' USING ERRCODE='55000'; END IF;
 IF OLD.state IN ('CAPTURED','RELEASED') AND ROW(OLD.actual_cost,OLD.captured_sale,OLD.response_snapshot_version,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.response_completed_at) IS DISTINCT FROM ROW(NEW.actual_cost,NEW.captured_sale,NEW.response_snapshot_version,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.response_completed_at) THEN RAISE EXCEPTION 'terminal chat charge is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
