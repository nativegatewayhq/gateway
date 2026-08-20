CREATE TABLE async_job_webhook_bindings (
    job_id text PRIMARY KEY REFERENCES async_jobs(id),
    provider text NOT NULL CHECK (provider='replicate'),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    token_digest bytea NOT NULL CHECK (octet_length(token_digest)=32),
    expires_at timestamptz NOT NULL,
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);
CREATE INDEX async_job_webhook_bindings_expiry_idx ON async_job_webhook_bindings(expires_at,job_id) WHERE disabled_at IS NULL;

CREATE TABLE async_job_webhook_deliveries (
    provider text NOT NULL CHECK (provider='replicate'),
    delivery_id text NOT NULL CHECK (length(delivery_id) BETWEEN 1 AND 200),
    job_id text NOT NULL REFERENCES async_jobs(id),
    terminal_status text NOT NULL CHECK (terminal_status IN ('SUCCEEDED','FAILED','CANCELED')),
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(provider,delivery_id)
);
CREATE INDEX async_job_webhook_deliveries_job_idx ON async_job_webhook_deliveries(job_id,received_at);

CREATE FUNCTION enforce_async_job_webhook_binding_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.job_id,OLD.provider,OLD.channel_id,OLD.token_digest,OLD.expires_at,OLD.created_at)
       IS DISTINCT FROM ROW(NEW.job_id,NEW.provider,NEW.channel_id,NEW.token_digest,NEW.expires_at,NEW.created_at) OR
       (OLD.disabled_at IS NOT NULL AND NEW.disabled_at IS DISTINCT FROM OLD.disabled_at) THEN
        RAISE EXCEPTION 'async job webhook binding identity is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER async_job_webhook_bindings_update_guard BEFORE UPDATE ON async_job_webhook_bindings FOR EACH ROW EXECUTE FUNCTION enforce_async_job_webhook_binding_update();
CREATE TRIGGER async_job_webhook_bindings_no_delete BEFORE DELETE ON async_job_webhook_bindings FOR EACH ROW EXECUTE FUNCTION reject_async_job_delete();
CREATE TRIGGER async_job_webhook_deliveries_no_update BEFORE UPDATE OR DELETE ON async_job_webhook_deliveries FOR EACH ROW EXECUTE FUNCTION reject_async_job_delete();
