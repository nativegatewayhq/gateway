CREATE TABLE image_charge_reconciliations (
    charge_id text PRIMARY KEY REFERENCES image_request_charges(id),
    outcome text NOT NULL CHECK (outcome IN ('KNOWN_SUCCESS','KNOWN_FAILURE','UNKNOWN')),
    reason text NOT NULL CHECK (reason IN ('response_unavailable','settlement_failed','executor_timeout','executor_connection_lost','provider_panic','legacy_unknown')),
    response_status integer,
    response_headers jsonb,
    response_body bytea,
    response_body_sha256 bytea,
    state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','LEASED','MANUAL_REVIEW','RESOLVED')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    last_error_category text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    CHECK (
        (outcome = 'UNKNOWN' AND response_status IS NULL AND response_headers IS NULL AND response_body IS NULL AND response_body_sha256 IS NULL)
        OR
        (outcome IN ('KNOWN_SUCCESS','KNOWN_FAILURE') AND response_status BETWEEN 100 AND 599
            AND jsonb_typeof(response_headers) = 'object' AND response_body IS NOT NULL AND octet_length(response_body_sha256) = 32)
    ),
    CHECK ((state = 'LEASED' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL) OR (state <> 'LEASED' AND lease_owner IS NULL AND lease_until IS NULL)),
    CHECK ((state = 'RESOLVED' AND resolved_at IS NOT NULL) OR (state <> 'RESOLVED' AND resolved_at IS NULL))
);
CREATE INDEX image_charge_reconciliations_due_idx ON image_charge_reconciliations(next_attempt_at, charge_id) WHERE state = 'PENDING';
CREATE INDEX image_charge_reconciliations_lease_idx ON image_charge_reconciliations(lease_until, charge_id) WHERE state = 'LEASED';

CREATE FUNCTION require_reconciling_charge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM image_request_charges WHERE id = NEW.charge_id AND state = 'RECONCILING') THEN
        RAISE EXCEPTION 'reconciliation requires a reconciling charge' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER image_charge_reconciliations_insert_guard BEFORE INSERT ON image_charge_reconciliations FOR EACH ROW EXECUTE FUNCTION require_reconciling_charge();

INSERT INTO image_charge_reconciliations(charge_id,outcome,reason,state)
SELECT id,'UNKNOWN','legacy_unknown','PENDING'
FROM image_request_charges
WHERE state='RECONCILING'
ON CONFLICT(charge_id) DO NOTHING;

CREATE FUNCTION enforce_reconciliation_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.charge_id,OLD.outcome,OLD.reason,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.created_at)
       IS DISTINCT FROM
       ROW(NEW.charge_id,NEW.outcome,NEW.reason,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.created_at) THEN
        RAISE EXCEPTION 'reconciliation observation is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.state = 'RESOLVED' AND ROW(OLD.state,OLD.attempt_count,OLD.next_attempt_at,OLD.lease_owner,OLD.lease_until,OLD.last_error_category,OLD.resolved_at)
       IS DISTINCT FROM ROW(NEW.state,NEW.attempt_count,NEW.next_attempt_at,NEW.lease_owner,NEW.lease_until,NEW.last_error_category,NEW.resolved_at) THEN
        RAISE EXCEPTION 'resolved reconciliation is immutable' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER image_charge_reconciliations_update_guard BEFORE UPDATE ON image_charge_reconciliations FOR EACH ROW EXECUTE FUNCTION enforce_reconciliation_update();

CREATE FUNCTION reject_reconciliation_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'reconciliations are append-only' USING ERRCODE = '55000'; END $$;
CREATE TRIGGER image_charge_reconciliations_no_delete BEFORE DELETE ON image_charge_reconciliations FOR EACH ROW EXECUTE FUNCTION reject_reconciliation_delete();
