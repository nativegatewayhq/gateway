CREATE TABLE provider_credentials (
    id text PRIMARY KEY CHECK (id ~ '^pcred_[0-9a-f]{32}$'),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    provider text NOT NULL CHECK (provider IN ('google','openai','xai')),
    version bigint NOT NULL CHECK (version > 0),
    state text NOT NULL CHECK (state IN ('staged','active','retired')),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 17 AND 4112),
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    wrapped_data_key bytea NOT NULL CHECK (octet_length(wrapped_data_key) = 48),
    wrap_nonce bytea NOT NULL CHECK (octet_length(wrap_nonce) = 12),
    master_key_id text NOT NULL CHECK (master_key_id ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    created_by text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 200),
    created_reason text NOT NULL CHECK (char_length(created_reason) BETWEEN 1 AND 500),
    created_at timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    retired_at timestamptz,
    UNIQUE(channel_id, version)
);

CREATE UNIQUE INDEX provider_credentials_one_active_per_channel
    ON provider_credentials(channel_id) WHERE state = 'active';

CREATE OR REPLACE FUNCTION enforce_provider_credential_scope() RETURNS trigger AS $$
DECLARE channel_provider text;
BEGIN
    SELECT provider INTO channel_provider FROM provider_channels WHERE id=NEW.channel_id;
    IF channel_provider IS NULL OR channel_provider <> NEW.provider THEN
        RAISE EXCEPTION 'provider credential channel scope mismatch';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credentials_scope
BEFORE INSERT OR UPDATE ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION enforce_provider_credential_scope();

CREATE OR REPLACE FUNCTION enforce_provider_credential_immutability() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'provider credentials cannot be deleted';
    END IF;
    IF NEW.id <> OLD.id OR NEW.channel_id <> OLD.channel_id OR NEW.provider <> OLD.provider OR
       NEW.version <> OLD.version OR NEW.ciphertext <> OLD.ciphertext OR NEW.nonce <> OLD.nonce OR
       NEW.wrapped_data_key <> OLD.wrapped_data_key OR NEW.wrap_nonce <> OLD.wrap_nonce OR
       NEW.master_key_id <> OLD.master_key_id OR NEW.created_by <> OLD.created_by OR
       NEW.created_reason <> OLD.created_reason OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'provider credential encrypted material is immutable';
    END IF;
    IF NOT ((OLD.state = 'staged' AND NEW.state IN ('active','retired')) OR
            (OLD.state = 'active' AND NEW.state = 'retired')) THEN
        RAISE EXCEPTION 'invalid provider credential state transition';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credentials_immutable
BEFORE UPDATE OR DELETE ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION enforce_provider_credential_immutability();

CREATE TABLE provider_credential_lifecycle_operations (
    operation_key text PRIMARY KEY CHECK (char_length(operation_key) BETWEEN 1 AND 200),
    action text NOT NULL CHECK (action IN ('stage','activate','retire')),
    request_tag bytea NOT NULL CHECK (octet_length(request_tag) = 32),
    credential_id text NOT NULL REFERENCES provider_credentials(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE provider_credential_events (
    id text PRIMARY KEY CHECK (id ~ '^pcevt_[0-9a-f]{32}$'),
    credential_id text NOT NULL REFERENCES provider_credentials(id),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    provider text NOT NULL CHECK (provider IN ('google','openai','xai')),
    credential_version bigint NOT NULL CHECK (credential_version > 0),
    action text NOT NULL CHECK (action IN ('stage','activate','retire')),
    actor text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 200),
    reason text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    operation_key text NOT NULL CHECK (char_length(operation_key) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION reject_provider_credential_event_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'provider credential events are append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credential_events_append_only
BEFORE UPDATE OR DELETE ON provider_credential_events
FOR EACH ROW EXECUTE FUNCTION reject_provider_credential_event_mutation();

CREATE TRIGGER provider_credential_lifecycle_operations_append_only
BEFORE UPDATE OR DELETE ON provider_credential_lifecycle_operations
FOR EACH ROW EXECUTE FUNCTION reject_provider_credential_event_mutation();
