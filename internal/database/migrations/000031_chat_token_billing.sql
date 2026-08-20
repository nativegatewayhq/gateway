CREATE TABLE chat_token_prices (
    id text PRIMARY KEY CHECK (id ~ '^ctp_[a-f0-9]{32}$'),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    protocol text NOT NULL CHECK (protocol = 'openai'),
    operation text NOT NULL CHECK (operation = 'chat.completions'),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model = btrim(model)),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    input_cost_per_million bigint NOT NULL CHECK (input_cost_per_million >= 0),
    input_sale_per_million bigint NOT NULL CHECK (input_sale_per_million > 0),
    cached_input_cost_per_million bigint NOT NULL CHECK (cached_input_cost_per_million >= 0),
    cached_input_sale_per_million bigint NOT NULL CHECK (cached_input_sale_per_million > 0),
    output_cost_per_million bigint NOT NULL CHECK (output_cost_per_million >= 0),
    output_sale_per_million bigint NOT NULL CHECK (output_sale_per_million > 0),
    effective_from timestamptz NOT NULL,
    effective_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_until IS NULL OR effective_until > effective_from),
    EXCLUDE USING gist (channel_id WITH =, protocol WITH =, operation WITH =, model WITH =, tstzrange(effective_from,effective_until,'[)') WITH &&)
);
CREATE INDEX chat_token_prices_lookup_idx ON chat_token_prices(channel_id,protocol,operation,model,effective_from DESC);
CREATE TABLE chat_token_price_publications (
    publication_key text PRIMARY KEY CHECK (length(publication_key) BETWEEN 1 AND 200 AND publication_key=btrim(publication_key)),
    price_id text NOT NULL UNIQUE REFERENCES chat_token_prices(id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION reject_chat_token_price_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'chat token prices are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER chat_token_prices_no_update BEFORE UPDATE ON chat_token_prices FOR EACH ROW EXECUTE FUNCTION reject_chat_token_price_mutation();
CREATE TRIGGER chat_token_prices_no_delete BEFORE DELETE ON chat_token_prices FOR EACH ROW EXECUTE FUNCTION reject_chat_token_price_mutation();

CREATE TABLE chat_request_charges (
    id text PRIMARY KEY CHECK (id ~ '^chc_[a-f0-9]{32}$'),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text NOT NULL,
    api_key_id text NOT NULL,
    protocol text NOT NULL CHECK (protocol='openai'),
    operation text NOT NULL CHECK (operation='chat.completions'),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model=btrim(model)),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    price_id text NOT NULL REFERENCES chat_token_prices(id),
    maximum_input_tokens bigint NOT NULL CHECK (maximum_input_tokens > 0),
    maximum_output_tokens bigint NOT NULL CHECK (maximum_output_tokens > 0),
    currency text NOT NULL CHECK (currency='USD_TICKS'),
    estimated_cost bigint NOT NULL CHECK (estimated_cost >= 0),
    reserved_sale bigint NOT NULL CHECK (reserved_sale > 0),
    actual_cost bigint CHECK (actual_cost >= 0),
    captured_sale bigint NOT NULL DEFAULT 0 CHECK (captured_sale >= 0 AND captured_sale <= reserved_sale),
    reservation_id text NOT NULL UNIQUE REFERENCES wallet_reservations(id),
    state text NOT NULL CHECK (state IN ('RESERVED','CAPTURED','RELEASED','RECONCILING')),
    idempotency_key text,
    request_fingerprint bytea,
    response_snapshot_version smallint NOT NULL DEFAULT 0 CHECK (response_snapshot_version IN (0,1)),
    response_status integer,
    response_headers jsonb,
    response_body bytea,
    response_body_sha256 bytea,
    response_completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(organization_id,request_id),
    FOREIGN KEY(project_id,organization_id) REFERENCES projects(id,organization_id),
    FOREIGN KEY(api_key_id,project_id) REFERENCES service_api_keys(id,project_id),
    CHECK ((idempotency_key IS NULL AND request_fingerprint IS NULL) OR (idempotency_key IS NOT NULL AND length(idempotency_key) BETWEEN 1 AND 200 AND octet_length(request_fingerprint)=32)),
    CHECK ((state='CAPTURED' AND actual_cost IS NOT NULL AND captured_sale > 0) OR (state<>'CAPTURED' AND actual_cost IS NULL AND captured_sale=0)),
    CHECK ((response_snapshot_version=0 AND response_status IS NULL AND response_headers IS NULL AND response_body IS NULL AND response_body_sha256 IS NULL AND response_completed_at IS NULL) OR (response_snapshot_version=1 AND state IN ('CAPTURED','RELEASED') AND response_status BETWEEN 100 AND 599 AND response_body IS NOT NULL AND octet_length(response_body_sha256)=32 AND response_completed_at IS NOT NULL))
);
CREATE UNIQUE INDEX chat_request_charges_idempotency_idx ON chat_request_charges(organization_id,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX chat_request_charges_state_idx ON chat_request_charges(state,updated_at) WHERE state IN ('RESERVED','RECONCILING');
CREATE FUNCTION enforce_chat_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.maximum_input_tokens,OLD.maximum_output_tokens,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.idempotency_key,OLD.request_fingerprint,OLD.created_at) IS DISTINCT FROM ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.maximum_input_tokens,NEW.maximum_output_tokens,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.idempotency_key,NEW.request_fingerprint,NEW.created_at) THEN RAISE EXCEPTION 'chat charge identity is immutable' USING ERRCODE='55000'; END IF;
 IF NOT (NEW.state=OLD.state OR (OLD.state IN ('RESERVED','RECONCILING') AND NEW.state IN ('CAPTURED','RELEASED','RECONCILING'))) THEN RAISE EXCEPTION 'invalid chat charge transition' USING ERRCODE='55000'; END IF;
 IF OLD.state IN ('CAPTURED','RELEASED') AND ROW(OLD.actual_cost,OLD.captured_sale,OLD.response_snapshot_version,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.response_completed_at) IS DISTINCT FROM ROW(NEW.actual_cost,NEW.captured_sale,NEW.response_snapshot_version,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.response_completed_at) THEN RAISE EXCEPTION 'terminal chat charge is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER chat_request_charges_update_guard BEFORE UPDATE ON chat_request_charges FOR EACH ROW EXECUTE FUNCTION enforce_chat_charge_update();
CREATE FUNCTION reject_chat_charge_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'chat charges are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER chat_request_charges_no_delete BEFORE DELETE ON chat_request_charges FOR EACH ROW EXECUTE FUNCTION reject_chat_charge_delete();

ALTER TABLE cost_quota_allocations DROP CONSTRAINT cost_quota_allocations_charge_id_fkey;
ALTER TABLE provider_channel_spend_allocations DROP CONSTRAINT provider_channel_spend_allocations_charge_id_fkey;
CREATE FUNCTION require_known_charge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM image_request_charges WHERE id=NEW.charge_id) AND NOT EXISTS(SELECT 1 FROM chat_request_charges WHERE id=NEW.charge_id) THEN RAISE EXCEPTION 'allocation requires a known charge' USING ERRCODE='23503'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cost_quota_allocation_charge_guard BEFORE INSERT ON cost_quota_allocations FOR EACH ROW EXECUTE FUNCTION require_known_charge();
CREATE TRIGGER provider_spend_allocation_charge_guard BEFORE INSERT ON provider_channel_spend_allocations FOR EACH ROW EXECUTE FUNCTION require_known_charge();
ALTER TABLE cost_quota_policies DROP CONSTRAINT cost_quota_policies_dimension_check;
ALTER TABLE cost_quota_policies ADD CONSTRAINT cost_quota_policies_dimension_check CHECK ((protocol IS NULL AND operation IS NULL AND model IS NULL) OR (((protocol='openai' AND operation IN ('image.generate','image.edit','chat.completions')) OR (protocol IN ('gemini','replicate','fal') AND operation='image.generate')) AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model)));

CREATE TABLE chat_usage_evidence (
    charge_id text PRIMARY KEY REFERENCES chat_request_charges(id),
    prompt_tokens bigint NOT NULL CHECK (prompt_tokens >= 0),
    cached_input_tokens bigint NOT NULL CHECK (cached_input_tokens >= 0 AND cached_input_tokens <= prompt_tokens),
    completion_tokens bigint NOT NULL CHECK (completion_tokens >= 0),
    schema_version text NOT NULL CHECK (schema_version='openai-chat-usage-v1'),
    body_sha256 bytea NOT NULL CHECK (octet_length(body_sha256)=32),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION reject_chat_usage_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'chat usage evidence is immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER chat_usage_no_update BEFORE UPDATE ON chat_usage_evidence FOR EACH ROW EXECUTE FUNCTION reject_chat_usage_mutation();
CREATE TRIGGER chat_usage_no_delete BEFORE DELETE ON chat_usage_evidence FOR EACH ROW EXECUTE FUNCTION reject_chat_usage_mutation();

CREATE TABLE chat_charge_reconciliations (
    charge_id text PRIMARY KEY REFERENCES chat_request_charges(id),
    reason text NOT NULL CHECK (reason IN ('executor_timeout','executor_connection_lost','response_unavailable','usage_invalid','settlement_failed','provider_panic')),
    response_status integer, response_headers jsonb, response_body bytea, response_body_sha256 bytea,
    prompt_tokens bigint, cached_input_tokens bigint, completion_tokens bigint,
    state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','LEASED','MANUAL_REVIEW','RESOLVED')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0), next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text, lease_until timestamptz, last_error_category text,
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), resolved_at timestamptz,
    CHECK ((response_status IS NULL AND response_headers IS NULL AND response_body IS NULL AND response_body_sha256 IS NULL) OR (response_status BETWEEN 100 AND 599 AND response_body IS NOT NULL AND octet_length(response_body_sha256)=32)),
    CHECK ((prompt_tokens IS NULL AND cached_input_tokens IS NULL AND completion_tokens IS NULL) OR (reason='settlement_failed' AND prompt_tokens >= 0 AND cached_input_tokens >= 0 AND cached_input_tokens <= prompt_tokens AND completion_tokens >= 0)),
    CHECK ((state='LEASED' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL) OR (state<>'LEASED' AND lease_owner IS NULL AND lease_until IS NULL)),
    CHECK ((state='RESOLVED' AND resolved_at IS NOT NULL) OR (state<>'RESOLVED' AND resolved_at IS NULL))
);
CREATE INDEX chat_charge_reconciliations_due_idx ON chat_charge_reconciliations(next_attempt_at,charge_id) WHERE state='PENDING';
CREATE FUNCTION enforce_chat_reconciliation_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.charge_id,OLD.reason,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.prompt_tokens,OLD.cached_input_tokens,OLD.completion_tokens,OLD.created_at) IS DISTINCT FROM ROW(NEW.charge_id,NEW.reason,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.prompt_tokens,NEW.cached_input_tokens,NEW.completion_tokens,NEW.created_at) THEN RAISE EXCEPTION 'chat reconciliation evidence is immutable' USING ERRCODE='55000'; END IF;
 IF OLD.state='RESOLVED' AND ROW(OLD.state,OLD.attempt_count,OLD.next_attempt_at,OLD.lease_owner,OLD.lease_until,OLD.last_error_category,OLD.resolved_at) IS DISTINCT FROM ROW(NEW.state,NEW.attempt_count,NEW.next_attempt_at,NEW.lease_owner,NEW.lease_until,NEW.last_error_category,NEW.resolved_at) THEN RAISE EXCEPTION 'resolved chat reconciliation is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER chat_charge_reconciliations_update_guard BEFORE UPDATE ON chat_charge_reconciliations FOR EACH ROW EXECUTE FUNCTION enforce_chat_reconciliation_update();
CREATE FUNCTION reject_chat_reconciliation_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'chat reconciliations are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER chat_charge_reconciliations_no_delete BEFORE DELETE ON chat_charge_reconciliations FOR EACH ROW EXECUTE FUNCTION reject_chat_reconciliation_delete();
