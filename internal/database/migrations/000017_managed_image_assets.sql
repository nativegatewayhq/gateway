CREATE TABLE image_assets (
    id text PRIMARY KEY CHECK (id ~ '^asset_[a-f0-9]{32}$'),
    charge_id text REFERENCES image_request_charges(id),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    protocol text NOT NULL CHECK (protocol IN ('openai','gemini')),
    provider text NOT NULL CHECK (provider IN ('openai','xai','google')),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    result_index integer NOT NULL CHECK (result_index BETWEEN 0 AND 999),
    object_key text NOT NULL CHECK (length(object_key) BETWEEN 1 AND 1024 AND object_key = btrim(object_key)),
    content_type text NOT NULL CHECK (content_type LIKE 'image/%' AND length(content_type) <= 100),
    byte_length bigint NOT NULL CHECK (byte_length BETWEEN 0 AND 268435456),
    sha256 bytea NOT NULL CHECK (octet_length(sha256) = 32),
    state text NOT NULL CHECK (state IN ('PENDING','AVAILABLE','FAILED','ORPHANED')),
    failure_category text CHECK (failure_category IS NULL OR failure_category IN ('fetch_rejected','fetch_failed','invalid_content','upload_failed','persistence_failed')),
    lease_owner text CHECK (lease_owner IS NULL OR length(lease_owner) BETWEEN 1 AND 128),
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    available_at timestamptz,
    UNIQUE (protocol, request_id, result_index),
    UNIQUE (object_key),
    CHECK ((state = 'AVAILABLE' AND available_at IS NOT NULL AND failure_category IS NULL)
        OR (state = 'FAILED' AND available_at IS NULL AND failure_category IS NOT NULL)
        OR (state IN ('PENDING','ORPHANED') AND available_at IS NULL)),
    CHECK ((lease_owner IS NULL AND lease_until IS NULL) OR (state = 'PENDING' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX image_assets_charge_idx ON image_assets(charge_id, result_index) WHERE charge_id IS NOT NULL;
CREATE INDEX image_assets_pending_idx ON image_assets(updated_at, id) WHERE state = 'PENDING';

CREATE FUNCTION enforce_image_asset_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.id,OLD.charge_id,OLD.request_id,OLD.protocol,OLD.provider,OLD.channel_id,OLD.result_index,OLD.object_key,OLD.content_type,OLD.byte_length,OLD.sha256,OLD.created_at)
       IS DISTINCT FROM
       ROW(NEW.id,NEW.charge_id,NEW.request_id,NEW.protocol,NEW.provider,NEW.channel_id,NEW.result_index,NEW.object_key,NEW.content_type,NEW.byte_length,NEW.sha256,NEW.created_at) THEN
        RAISE EXCEPTION 'image asset identity and content are immutable' USING ERRCODE = '55000';
    END IF;
    IF NOT (
        NEW.state = OLD.state OR
        (OLD.state = 'PENDING' AND NEW.state IN ('AVAILABLE','FAILED','ORPHANED'))
    ) THEN
        RAISE EXCEPTION 'invalid image asset transition' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER image_assets_update_guard BEFORE UPDATE ON image_assets FOR EACH ROW EXECUTE FUNCTION enforce_image_asset_update();

CREATE FUNCTION reject_image_asset_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'image assets are append-only' USING ERRCODE = '55000'; END $$;
CREATE TRIGGER image_assets_no_delete BEFORE DELETE ON image_assets FOR EACH ROW EXECUTE FUNCTION reject_image_asset_delete();
