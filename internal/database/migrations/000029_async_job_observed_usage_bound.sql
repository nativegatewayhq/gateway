ALTER TABLE async_job_usage_evidence
    DROP CONSTRAINT async_job_usage_evidence_quantity_check,
    ADD CONSTRAINT async_job_usage_evidence_quantity_check CHECK (quantity BETWEEN 0 AND 128);
