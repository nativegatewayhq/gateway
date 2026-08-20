ALTER TABLE service_api_keys
    ADD COLUMN requests_per_minute bigint,
    ADD COLUMN burst bigint,
    ADD CONSTRAINT service_api_keys_rate_limit_policy_check CHECK (
        (requests_per_minute IS NULL AND burst IS NULL) OR
        (requests_per_minute BETWEEN 1 AND 1000000 AND burst BETWEEN 1 AND requests_per_minute)
    );
