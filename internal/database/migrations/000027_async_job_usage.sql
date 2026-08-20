ALTER TABLE async_jobs
    ADD COLUMN usage_dimension text,
    ADD COLUMN usage_unit text,
    ADD COLUMN estimated_quantity bigint,
    ADD COLUMN usage_extractor_version text,
    ADD CONSTRAINT async_jobs_usage_estimate_check CHECK (
        (usage_dimension IS NULL AND usage_unit IS NULL AND estimated_quantity IS NULL AND usage_extractor_version IS NULL)
        OR (usage_dimension='output' AND usage_unit='image' AND estimated_quantity BETWEEN 1 AND 10
            AND length(usage_extractor_version) BETWEEN 1 AND 80)
    );

CREATE TABLE async_job_usage_evidence (
    job_id text PRIMARY KEY REFERENCES async_jobs(id),
    charge_id text REFERENCES image_request_charges(id),
    provider text NOT NULL CHECK (provider IN ('replicate','fal')),
    source text NOT NULL CHECK (source IN ('submit','poll','webhook','cancel')),
    dimension text NOT NULL CHECK (dimension='output'),
    unit text NOT NULL CHECK (unit='image'),
    quantity bigint NOT NULL CHECK (quantity BETWEEN 0 AND 10),
    provenance text NOT NULL CHECK (provenance=source),
    extractor_version text NOT NULL CHECK (length(extractor_version) BETWEEN 1 AND 80),
    observation_sha256 bytea NOT NULL CHECK (octet_length(observation_sha256)=32),
    reconciliation_reason text CHECK (reconciliation_reason IN ('usage_unknown','usage_exceeds_estimate','partial_terminal_conflict','usage_identity_mismatch')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX async_job_usage_charge_idx ON async_job_usage_evidence(charge_id) WHERE charge_id IS NOT NULL;
CREATE TRIGGER async_job_usage_no_update BEFORE UPDATE OR DELETE ON async_job_usage_evidence FOR EACH ROW EXECUTE FUNCTION reject_async_job_delete();

CREATE FUNCTION enforce_async_job_usage_estimate_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.usage_dimension,OLD.usage_unit,OLD.estimated_quantity,OLD.usage_extractor_version)
       IS DISTINCT FROM ROW(NEW.usage_dimension,NEW.usage_unit,NEW.estimated_quantity,NEW.usage_extractor_version) THEN
        RAISE EXCEPTION 'async job usage estimate is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER async_job_usage_estimate_update_guard BEFORE UPDATE ON async_jobs FOR EACH ROW EXECUTE FUNCTION enforce_async_job_usage_estimate_update();
