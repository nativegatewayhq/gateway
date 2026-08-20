CREATE TABLE runway_upload_assets (
    id text PRIMARY KEY CHECK (id ~ '^asset_[a-f0-9]{32}$'),
    uri_digest bytea NOT NULL CHECK (octet_length(uri_digest)=32),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text NOT NULL REFERENCES projects(id),
    api_key_id text NOT NULL REFERENCES service_api_keys(id),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > issued_at),
    UNIQUE(channel_id,uri_digest)
);

CREATE INDEX runway_upload_assets_owner_active_idx ON runway_upload_assets(organization_id,project_id,api_key_id,channel_id,expires_at);

CREATE TABLE runway_upload_asset_events (
    id text PRIMARY KEY CHECK (id ~ '^event_[a-f0-9]{32}$'),
    asset_id text NOT NULL REFERENCES runway_upload_assets(id),
    category text NOT NULL CHECK (category IN ('issued','authorized')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION reject_runway_upload_asset_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'runway upload asset history is append-only' USING ERRCODE='55000';
END $$;

CREATE TRIGGER runway_upload_assets_no_update BEFORE UPDATE OR DELETE ON runway_upload_assets FOR EACH ROW EXECUTE FUNCTION reject_runway_upload_asset_mutation();
CREATE TRIGGER runway_upload_asset_events_no_update BEFORE UPDATE OR DELETE ON runway_upload_asset_events FOR EACH ROW EXECUTE FUNCTION reject_runway_upload_asset_mutation();
