CREATE TABLE audio_input_assets(
 id text PRIMARY KEY CHECK(id ~ '^audasset_[a-f0-9]{32}$'),
 organization_id text NOT NULL REFERENCES organizations(id),
 project_id text NOT NULL,
 api_key_id text NOT NULL,
 object_key text NOT NULL UNIQUE CHECK(length(object_key) BETWEEN 1 AND 500 AND object_key=btrim(object_key)),
 content_type text NOT NULL CHECK(length(content_type) BETWEEN 1 AND 100 AND content_type=btrim(content_type)),
 byte_length bigint NOT NULL CHECK(byte_length>0),
 sha256 bytea NOT NULL CHECK(octet_length(sha256)=32),
 state text NOT NULL CHECK(state IN('UPLOADING','AVAILABLE','DELETING','DELETED','FAILED')),
 expires_at timestamptz NOT NULL,
 available_at timestamptz,
 deleted_at timestamptz,
 failure_category text,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(project_id,organization_id) REFERENCES projects(id,organization_id),
 FOREIGN KEY(api_key_id,project_id) REFERENCES service_api_keys(id,project_id),
 CHECK(expires_at>created_at),
 CHECK((state='AVAILABLE' AND available_at IS NOT NULL AND deleted_at IS NULL) OR state<>'AVAILABLE'),
 CHECK((state='DELETED' AND deleted_at IS NOT NULL) OR state<>'DELETED')
);
CREATE INDEX audio_input_assets_owner_idx ON audio_input_assets(organization_id,project_id,api_key_id,created_at DESC);
CREATE INDEX audio_input_assets_cleanup_idx ON audio_input_assets(state,expires_at,updated_at) WHERE state IN('UPLOADING','AVAILABLE','DELETING','FAILED');

CREATE TABLE audio_input_asset_publications(
 organization_id text NOT NULL,
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 200),
 asset_id text NOT NULL UNIQUE REFERENCES audio_input_assets(id),
 request_fingerprint bytea NOT NULL CHECK(octet_length(request_fingerprint)=32),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,idempotency_key),
 FOREIGN KEY(organization_id) REFERENCES organizations(id)
);

CREATE TABLE audio_input_asset_leases(
 asset_id text NOT NULL REFERENCES audio_input_assets(id),
 lease_id text NOT NULL CHECK(lease_id ~ '^audlease_[a-f0-9]{32}$'),
 owner text NOT NULL CHECK(length(owner) BETWEEN 1 AND 128),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(asset_id,lease_id)
);
CREATE INDEX audio_input_asset_leases_expiry_idx ON audio_input_asset_leases(expires_at);

CREATE TABLE audio_input_asset_events(
 id bigserial PRIMARY KEY,
 asset_id text NOT NULL REFERENCES audio_input_assets(id),
 category text NOT NULL CHECK(category IN('CREATED','AVAILABLE','LEASED','RELEASED','DELETE_REQUESTED','DELETED','FAILED')),
 reason text,
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION enforce_audio_input_asset_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF ROW(OLD.id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.object_key,OLD.content_type,OLD.byte_length,OLD.sha256,OLD.expires_at,OLD.created_at) IS DISTINCT FROM ROW(NEW.id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.object_key,NEW.content_type,NEW.byte_length,NEW.sha256,NEW.expires_at,NEW.created_at) THEN RAISE EXCEPTION 'audio input asset identity is immutable' USING ERRCODE='55000'; END IF;
 IF NOT(NEW.state=OLD.state OR (OLD.state='UPLOADING' AND NEW.state IN('AVAILABLE','FAILED','DELETING')) OR (OLD.state='AVAILABLE' AND NEW.state='DELETING') OR (OLD.state='FAILED' AND NEW.state='DELETING') OR (OLD.state='DELETING' AND NEW.state='DELETED')) THEN RAISE EXCEPTION 'invalid audio input asset transition' USING ERRCODE='55000'; END IF;
 IF OLD.state='DELETED' AND ROW(OLD.state,OLD.available_at,OLD.deleted_at,OLD.failure_category) IS DISTINCT FROM ROW(NEW.state,NEW.available_at,NEW.deleted_at,NEW.failure_category) THEN RAISE EXCEPTION 'deleted audio input asset is immutable' USING ERRCODE='55000'; END IF;
 NEW.updated_at=now(); RETURN NEW; END $$;
CREATE TRIGGER audio_input_assets_update_guard BEFORE UPDATE ON audio_input_assets FOR EACH ROW EXECUTE FUNCTION enforce_audio_input_asset_update();
CREATE FUNCTION reject_audio_input_asset_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audio input assets are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER audio_input_assets_no_delete BEFORE DELETE ON audio_input_assets FOR EACH ROW EXECUTE FUNCTION reject_audio_input_asset_delete();
CREATE TRIGGER audio_input_asset_publications_no_mutation BEFORE UPDATE OR DELETE ON audio_input_asset_publications FOR EACH ROW EXECUTE FUNCTION reject_audio_event_mutation();
CREATE TRIGGER audio_input_asset_events_no_mutation BEFORE UPDATE OR DELETE ON audio_input_asset_events FOR EACH ROW EXECUTE FUNCTION reject_audio_event_mutation();
