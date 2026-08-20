ALTER TABLE async_jobs
    DROP CONSTRAINT async_jobs_usage_estimate_check,
    ADD COLUMN usage_result_extractor_version text;

ALTER TABLE async_jobs
    ADD CONSTRAINT async_jobs_usage_estimate_check CHECK (
        (usage_dimension IS NULL AND usage_unit IS NULL AND estimated_quantity IS NULL AND usage_extractor_version IS NULL AND usage_result_extractor_version IS NULL)
        OR (usage_dimension='output' AND usage_unit='image' AND estimated_quantity BETWEEN 1 AND 10
            AND length(usage_extractor_version) BETWEEN 1 AND 80 AND length(usage_result_extractor_version) BETWEEN 1 AND 80)
    );

CREATE OR REPLACE FUNCTION enforce_async_job_usage_estimate_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.usage_dimension,OLD.usage_unit,OLD.estimated_quantity,OLD.usage_extractor_version,OLD.usage_result_extractor_version)
       IS DISTINCT FROM ROW(NEW.usage_dimension,NEW.usage_unit,NEW.estimated_quantity,NEW.usage_extractor_version,NEW.usage_result_extractor_version) THEN
        RAISE EXCEPTION 'async job usage estimate is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
