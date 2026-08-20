CREATE TABLE async_jobs (
    id text PRIMARY KEY CHECK (id ~ '^job_[a-f0-9]{32}$'),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text NOT NULL,
    api_key_id text NOT NULL REFERENCES service_api_keys(id),
    protocol text NOT NULL CHECK (length(protocol) BETWEEN 1 AND 40 AND protocol = lower(protocol)),
    operation text NOT NULL CHECK (length(operation) BETWEEN 1 AND 80 AND operation = lower(operation)),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model = btrim(model)),
    provider text NOT NULL CHECK (length(provider) BETWEEN 1 AND 40 AND provider = lower(provider)),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    charge_id text UNIQUE REFERENCES image_request_charges(id),
    idempotency_key text,
    request_fingerprint bytea,
    status text NOT NULL CHECK (status IN ('PENDING','QUEUED','PROCESSING','SUCCEEDED','FAILED','CANCELED','RECONCILING')),
    settlement_state text NOT NULL DEFAULT 'NONE' CHECK (settlement_state IN ('NONE','PENDING','SETTLED','MANUAL_REVIEW')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    failure_category text CHECK (length(failure_category) BETWEEN 1 AND 80),
    response_status integer,
    response_headers jsonb,
    response_body bytea,
    response_body_sha256 bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE (organization_id, request_id),
    FOREIGN KEY (project_id, organization_id) REFERENCES projects(id, organization_id),
    CHECK ((idempotency_key IS NULL AND request_fingerprint IS NULL) OR
           (length(idempotency_key) BETWEEN 1 AND 256 AND octet_length(request_fingerprint)=32)),
    CHECK ((status IN ('SUCCEEDED','FAILED') AND response_status BETWEEN 100 AND 599 AND jsonb_typeof(response_headers)='object' AND response_body IS NOT NULL AND octet_length(response_body_sha256)=32)
        OR (status NOT IN ('SUCCEEDED','FAILED') AND response_status IS NULL AND response_headers IS NULL AND response_body IS NULL AND response_body_sha256 IS NULL)),
    CHECK ((status IN ('SUCCEEDED','FAILED','CANCELED') AND completed_at IS NOT NULL) OR
           (status NOT IN ('SUCCEEDED','FAILED','CANCELED') AND completed_at IS NULL)),
    CHECK ((status IN ('SUCCEEDED','FAILED','CANCELED') AND settlement_state IN ('PENDING','SETTLED','MANUAL_REVIEW')) OR
           (status NOT IN ('SUCCEEDED','FAILED','CANCELED') AND settlement_state='NONE'))
);
CREATE UNIQUE INDEX async_jobs_idempotency_idx ON async_jobs(organization_id,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX async_jobs_owner_idx ON async_jobs(organization_id,project_id,api_key_id,created_at,id);
CREATE INDEX async_jobs_active_idx ON async_jobs(status,updated_at,id) WHERE status NOT IN ('SUCCEEDED','FAILED','CANCELED');

CREATE TABLE async_job_provider_attempts (
    job_id text NOT NULL REFERENCES async_jobs(id),
    attempt_no integer NOT NULL CHECK (attempt_no > 0),
    provider text NOT NULL CHECK (length(provider) BETWEEN 1 AND 40 AND provider=lower(provider)),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    provider_job_id text CHECK (length(provider_job_id) BETWEEN 1 AND 500),
    state text NOT NULL CHECK (state IN ('SUBMITTING','SUBMITTED','RECONCILING','TERMINAL')),
    lease_owner text,
    lease_token text,
    lease_until timestamptz,
    poll_count integer NOT NULL DEFAULT 0 CHECK (poll_count >= 0),
    next_poll_at timestamptz NOT NULL DEFAULT now(),
    last_error_category text CHECK (length(last_error_category) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(job_id,attempt_no),
    CHECK ((state IN ('SUBMITTED','TERMINAL') AND provider_job_id IS NOT NULL) OR state IN ('SUBMITTING','RECONCILING')),
    CHECK ((lease_owner IS NULL AND lease_token IS NULL AND lease_until IS NULL) OR
           (lease_owner IS NOT NULL AND lease_token IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE UNIQUE INDEX async_job_provider_identity_idx ON async_job_provider_attempts(provider,channel_id,provider_job_id) WHERE provider_job_id IS NOT NULL;
CREATE INDEX async_job_attempt_due_idx ON async_job_provider_attempts(next_poll_at,job_id) WHERE state IN ('SUBMITTED','RECONCILING');

CREATE TABLE async_job_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id text NOT NULL REFERENCES async_jobs(id),
    version bigint NOT NULL CHECK (version > 0),
    event_type text NOT NULL CHECK (event_type IN ('CREATED','SUBMIT_STARTED','SUBMIT_CONFIRMED','SUBMIT_UNKNOWN','OBSERVED','CANCEL_REQUESTED','SETTLEMENT_PENDING','SETTLED','RETRY_SCHEDULED','MANUAL_REVIEW')),
    from_status text,
    to_status text NOT NULL,
    category text CHECK (length(category) BETWEEN 1 AND 80),
    source text NOT NULL CHECK (source IN ('api','submit','poll','cancel','webhook','worker','reconciliation')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(job_id,version)
);
CREATE INDEX async_job_events_job_idx ON async_job_events(job_id,id);

CREATE FUNCTION enforce_async_job_update() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER async_jobs_update_guard BEFORE UPDATE ON async_jobs FOR EACH ROW EXECUTE FUNCTION enforce_async_job_update();

CREATE FUNCTION reject_async_job_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'async jobs are durable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER async_jobs_no_delete BEFORE DELETE ON async_jobs FOR EACH ROW EXECUTE FUNCTION reject_async_job_delete();
CREATE TRIGGER async_job_attempts_no_delete BEFORE DELETE ON async_job_provider_attempts FOR EACH ROW EXECUTE FUNCTION reject_async_job_delete();
CREATE TRIGGER async_job_events_no_update BEFORE UPDATE OR DELETE ON async_job_events FOR EACH ROW EXECUTE FUNCTION reject_async_job_delete();
