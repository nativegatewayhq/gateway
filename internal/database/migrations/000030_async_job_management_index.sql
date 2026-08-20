CREATE INDEX async_jobs_management_owner_idx
    ON async_jobs(organization_id,project_id,api_key_id,created_at DESC,id DESC);

CREATE INDEX async_jobs_management_state_idx
    ON async_jobs(organization_id,project_id,api_key_id,status,settlement_state,created_at DESC,id DESC);
