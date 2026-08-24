ALTER TABLE provider_channels DROP CONSTRAINT provider_channels_provider_check;
ALTER TABLE provider_channels ADD CONSTRAINT provider_channels_provider_check CHECK (
    provider IN ('google','openai','xai','replicate','fal','stability','anthropic','runway','plugin')
);

ALTER TABLE image_assets DROP CONSTRAINT image_assets_provider_check;
ALTER TABLE image_assets ADD CONSTRAINT image_assets_provider_check CHECK (
    provider IN ('openai','xai','google','plugin')
);

CREATE TABLE plugin_channel_snapshots (
    channel_id text PRIMARY KEY REFERENCES provider_channels(id),
    plugin_id text NOT NULL CHECK (length(plugin_id) BETWEEN 1 AND 128 AND plugin_id=btrim(plugin_id)),
    plugin_version text NOT NULL CHECK (plugin_version ~ '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'),
    manifest_digest bytea NOT NULL CHECK (octet_length(manifest_digest)=32),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model=btrim(model)),
    protocol text NOT NULL CHECK (protocol IN ('openai','gemini')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(plugin_id,plugin_version,manifest_digest,model,protocol)
);

CREATE FUNCTION reject_plugin_channel_snapshot_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'plugin channel snapshots are immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER plugin_channel_snapshots_no_update BEFORE UPDATE OR DELETE ON plugin_channel_snapshots FOR EACH ROW EXECUTE FUNCTION reject_plugin_channel_snapshot_mutation();
