ALTER TABLE image_request_charges
    ADD COLUMN idempotency_key text,
    ADD COLUMN request_fingerprint bytea,
    ADD COLUMN response_snapshot_version smallint NOT NULL DEFAULT 0,
    ADD COLUMN response_status integer,
    ADD COLUMN response_headers jsonb,
    ADD COLUMN response_body bytea,
    ADD COLUMN response_body_sha256 bytea,
    ADD COLUMN response_completed_at timestamptz,
    ADD CHECK (idempotency_key IS NULL OR (length(idempotency_key) BETWEEN 1 AND 200 AND idempotency_key = btrim(idempotency_key))),
    ADD CHECK ((idempotency_key IS NULL AND request_fingerprint IS NULL) OR (idempotency_key IS NOT NULL AND octet_length(request_fingerprint) = 32)),
    ADD CHECK (response_snapshot_version IN (0,1)),
    ADD CHECK (
        (response_snapshot_version = 0 AND response_status IS NULL AND response_headers IS NULL AND response_body IS NULL AND response_body_sha256 IS NULL AND response_completed_at IS NULL)
        OR
        (response_snapshot_version = 1 AND state IN ('CAPTURED','RELEASED') AND response_status BETWEEN 100 AND 599
            AND jsonb_typeof(response_headers) = 'object' AND response_body IS NOT NULL
            AND octet_length(response_body_sha256) = 32 AND response_completed_at IS NOT NULL)
    );

CREATE UNIQUE INDEX image_request_charges_idempotency_idx
    ON image_request_charges(organization_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE OR REPLACE FUNCTION enforce_image_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.quantity,OLD.size,OLD.quality,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.created_at,OLD.idempotency_key,OLD.request_fingerprint)
       IS DISTINCT FROM
       ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.quantity,NEW.size,NEW.quality,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.created_at,NEW.idempotency_key,NEW.request_fingerprint) THEN
        RAISE EXCEPTION 'image charge identity and estimate are immutable' USING ERRCODE = '55000';
    END IF;
    IF NOT (
        NEW.state = OLD.state OR
        (OLD.state IN ('RESERVING','RESERVED','RECONCILING') AND NEW.state IN ('RESERVED','CAPTURED','RELEASED','RECONCILING'))
    ) THEN
        RAISE EXCEPTION 'invalid image charge transition' USING ERRCODE = '55000';
    END IF;
    IF OLD.response_snapshot_version = 1 AND
       ROW(OLD.response_snapshot_version,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.response_completed_at)
       IS DISTINCT FROM
       ROW(NEW.response_snapshot_version,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.response_completed_at) THEN
        RAISE EXCEPTION 'terminal response snapshot is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.state IN ('CAPTURED','RELEASED') AND
       ROW(OLD.actual_cost,OLD.captured_sale) IS DISTINCT FROM ROW(NEW.actual_cost,NEW.captured_sale) THEN
        RAISE EXCEPTION 'terminal charge settlement is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.response_snapshot_version = 0 AND NEW.response_snapshot_version = 1 AND NEW.state NOT IN ('CAPTURED','RELEASED') THEN
        RAISE EXCEPTION 'response snapshot requires terminal charge' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $$;
