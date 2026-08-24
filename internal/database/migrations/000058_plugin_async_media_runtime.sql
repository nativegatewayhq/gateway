ALTER TABLE plugin_channel_snapshots DROP CONSTRAINT plugin_channel_snapshots_protocol_check;
ALTER TABLE plugin_channel_snapshots ADD CONSTRAINT plugin_channel_snapshots_protocol_check CHECK (
    protocol IN ('openai','gemini','replicate','fal','runway')
);

ALTER TABLE async_job_usage_evidence DROP CONSTRAINT async_job_usage_evidence_provider_check;
ALTER TABLE async_job_usage_evidence ADD CONSTRAINT async_job_usage_evidence_provider_check CHECK (
    provider IN ('replicate','fal','runway','plugin')
);

ALTER TABLE video_assets DROP CONSTRAINT video_assets_provider_check;
ALTER TABLE video_assets ADD CONSTRAINT video_assets_provider_check CHECK (
    provider IN ('runway','plugin')
);
