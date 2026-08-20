-- Kept separate because 000019 may already be applied by rolling deployments.
ALTER TABLE async_jobs ADD COLUMN IF NOT EXISTS settlement_state text NOT NULL DEFAULT 'NONE';
ALTER TABLE async_jobs DROP CONSTRAINT IF EXISTS async_jobs_settlement_state_check;
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_settlement_state_check CHECK (settlement_state IN ('NONE','PENDING','SETTLED','MANUAL_REVIEW'));
ALTER TABLE async_jobs DROP CONSTRAINT IF EXISTS async_jobs_terminal_settlement_check;
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_terminal_settlement_check CHECK (
    (status IN ('SUCCEEDED','FAILED','CANCELED') AND settlement_state IN ('PENDING','SETTLED','MANUAL_REVIEW')) OR
    (status NOT IN ('SUCCEEDED','FAILED','CANCELED') AND settlement_state='NONE')
);

CREATE OR REPLACE FUNCTION enforce_async_job_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.id,OLD.request_id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.protocol,OLD.operation,OLD.model,OLD.provider,OLD.channel_id,OLD.charge_id,OLD.idempotency_key,OLD.request_fingerprint,OLD.created_at)
       IS DISTINCT FROM ROW(NEW.id,NEW.request_id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.protocol,NEW.operation,NEW.model,NEW.provider,NEW.channel_id,NEW.charge_id,NEW.idempotency_key,NEW.request_fingerprint,NEW.created_at) THEN
        RAISE EXCEPTION 'async job identity is immutable' USING ERRCODE='55000';
    END IF;
    IF OLD.status IN ('SUCCEEDED','FAILED','CANCELED') AND ROW(OLD.status,OLD.failure_category,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.completed_at)
       IS DISTINCT FROM ROW(NEW.status,NEW.failure_category,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.completed_at) THEN
        RAISE EXCEPTION 'terminal async job is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
