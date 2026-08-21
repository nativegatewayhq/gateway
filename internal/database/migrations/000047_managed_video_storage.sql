CREATE TABLE video_assets (
    id text PRIMARY KEY CHECK (id ~ '^vasset_[a-f0-9]{32}$'),
    job_id text NOT NULL REFERENCES async_jobs(id),
    charge_id text REFERENCES image_request_charges(id),
    provider text NOT NULL CHECK (provider='runway'),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    result_index integer NOT NULL CHECK (result_index BETWEEN 0 AND 9),
    object_key text NOT NULL CHECK (length(object_key) BETWEEN 1 AND 1000),
    content_type text NOT NULL CHECK (content_type IN ('video/mp4','video/webm','video/quicktime')),
    byte_length bigint NOT NULL CHECK (byte_length BETWEEN 1 AND 2147483648),
    sha256 bytea NOT NULL CHECK (octet_length(sha256)=32),
    state text NOT NULL CHECK (state IN ('PENDING','AVAILABLE')),
    lease_owner text,
    lease_until timestamptz,
    available_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(job_id,result_index),
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
    CHECK ((state='AVAILABLE')=(available_at IS NOT NULL))
);

CREATE TABLE video_asset_events (
    id bigserial PRIMARY KEY,
    asset_id text NOT NULL REFERENCES video_assets(id),
    category text NOT NULL CHECK (category IN ('created','claimed','available','released')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER video_asset_events_no_update BEFORE UPDATE OR DELETE ON video_asset_events FOR EACH ROW EXECUTE FUNCTION reject_runway_upload_asset_mutation();

CREATE FUNCTION enforce_video_asset_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.job_id IS DISTINCT FROM OLD.job_id
       OR NEW.charge_id IS DISTINCT FROM OLD.charge_id
       OR NEW.provider IS DISTINCT FROM OLD.provider
       OR NEW.channel_id IS DISTINCT FROM OLD.channel_id
       OR NEW.result_index IS DISTINCT FROM OLD.result_index
       OR NEW.object_key IS DISTINCT FROM OLD.object_key
       OR NEW.content_type IS DISTINCT FROM OLD.content_type
       OR NEW.byte_length IS DISTINCT FROM OLD.byte_length
       OR NEW.sha256 IS DISTINCT FROM OLD.sha256
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'video asset identity is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER video_assets_identity_immutable BEFORE UPDATE ON video_assets FOR EACH ROW EXECUTE FUNCTION enforce_video_asset_identity();

ALTER TABLE async_jobs ADD COLUMN managed_result_required boolean NOT NULL DEFAULT false;
ALTER TABLE async_jobs ADD COLUMN managed_response_status integer CHECK (managed_response_status BETWEEN 100 AND 599);
ALTER TABLE async_jobs ADD COLUMN managed_response_headers jsonb;
ALTER TABLE async_jobs ADD COLUMN managed_response_body bytea;
ALTER TABLE async_jobs ADD COLUMN managed_response_sha256 bytea CHECK (managed_response_sha256 IS NULL OR octet_length(managed_response_sha256)=32);
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_managed_response_check CHECK (
    (managed_response_status IS NULL AND managed_response_headers IS NULL AND managed_response_body IS NULL AND managed_response_sha256 IS NULL)
    OR (managed_response_status IS NOT NULL AND managed_response_headers IS NOT NULL AND managed_response_body IS NOT NULL AND managed_response_sha256 IS NOT NULL)
);

CREATE OR REPLACE FUNCTION enforce_async_job_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.id,OLD.request_id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.protocol,OLD.operation,OLD.model,OLD.provider,OLD.channel_id,OLD.charge_id,OLD.idempotency_key,OLD.request_fingerprint,OLD.managed_result_required,OLD.created_at)
       IS DISTINCT FROM ROW(NEW.id,NEW.request_id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.protocol,NEW.operation,NEW.model,NEW.provider,NEW.channel_id,NEW.charge_id,NEW.idempotency_key,NEW.request_fingerprint,NEW.managed_result_required,NEW.created_at) THEN
        RAISE EXCEPTION 'async job identity is immutable' USING ERRCODE='55000';
    END IF;
    IF OLD.status IN ('SUCCEEDED','FAILED','CANCELED') AND ROW(OLD.status,OLD.failure_category,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.completed_at)
       IS DISTINCT FROM ROW(NEW.status,NEW.failure_category,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.completed_at) THEN
        RAISE EXCEPTION 'terminal async job is immutable' USING ERRCODE='55000';
    END IF;
    IF OLD.managed_response_status IS NOT NULL AND ROW(OLD.managed_response_status,OLD.managed_response_headers,OLD.managed_response_body,OLD.managed_response_sha256)
       IS DISTINCT FROM ROW(NEW.managed_response_status,NEW.managed_response_headers,NEW.managed_response_body,NEW.managed_response_sha256) THEN
        RAISE EXCEPTION 'managed async job snapshot is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
