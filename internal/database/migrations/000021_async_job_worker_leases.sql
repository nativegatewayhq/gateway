ALTER TABLE async_job_provider_attempts DROP CONSTRAINT IF EXISTS async_job_provider_attempts_check;
ALTER TABLE async_job_provider_attempts ADD CONSTRAINT async_job_attempt_provider_identity_check CHECK (
    (state='SUBMITTED' AND provider_job_id IS NOT NULL) OR state IN ('SUBMITTING','RECONCILING','TERMINAL')
);
ALTER TABLE async_job_provider_attempts ADD COLUMN cancel_requested_at timestamptz;

ALTER TABLE async_jobs ADD COLUMN settlement_attempt_count integer NOT NULL DEFAULT 0 CHECK (settlement_attempt_count >= 0);
ALTER TABLE async_jobs ADD COLUMN settlement_next_attempt_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE async_jobs ADD COLUMN settlement_lease_owner text;
ALTER TABLE async_jobs ADD COLUMN settlement_lease_token text;
ALTER TABLE async_jobs ADD COLUMN settlement_lease_until timestamptz;
ALTER TABLE async_jobs ADD COLUMN settlement_last_error_category text CHECK (length(settlement_last_error_category) BETWEEN 1 AND 80);
ALTER TABLE async_jobs ADD CONSTRAINT async_job_settlement_lease_check CHECK (
    (settlement_lease_owner IS NULL AND settlement_lease_token IS NULL AND settlement_lease_until IS NULL) OR
    (settlement_lease_owner IS NOT NULL AND settlement_lease_token IS NOT NULL AND settlement_lease_until IS NOT NULL)
);
CREATE INDEX async_jobs_settlement_due_idx ON async_jobs(settlement_next_attempt_at,id) WHERE settlement_state='PENDING';
