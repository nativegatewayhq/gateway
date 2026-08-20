ALTER TABLE async_jobs DROP CONSTRAINT async_jobs_check1;
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_native_snapshot_check CHECK (
    (response_status IS NULL AND response_headers IS NULL AND response_body IS NULL AND response_body_sha256 IS NULL) OR
    (response_status BETWEEN 100 AND 599 AND jsonb_typeof(response_headers)='object' AND response_body IS NOT NULL AND octet_length(response_body_sha256)=32)
);
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_terminal_snapshot_check CHECK (
    status NOT IN ('SUCCEEDED','FAILED') OR response_status IS NOT NULL
);
