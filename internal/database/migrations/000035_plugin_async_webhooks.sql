ALTER TABLE async_job_webhook_bindings DROP CONSTRAINT async_job_webhook_bindings_provider_check;
ALTER TABLE async_job_webhook_bindings ADD CONSTRAINT async_job_webhook_bindings_provider_check CHECK (provider IN ('replicate','fal','plugin'));

ALTER TABLE async_job_webhook_deliveries DROP CONSTRAINT async_job_webhook_deliveries_provider_check;
ALTER TABLE async_job_webhook_deliveries ADD CONSTRAINT async_job_webhook_deliveries_provider_check CHECK (provider IN ('replicate','fal','plugin'));
