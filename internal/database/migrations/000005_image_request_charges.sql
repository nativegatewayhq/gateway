INSERT INTO provider_channels(id, provider, name, status) VALUES
    ('channel_00000000000000000000000000000001', 'openai', 'built-in-openai', 'active'),
    ('channel_00000000000000000000000000000002', 'xai', 'built-in-xai', 'active'),
    ('channel_00000000000000000000000000000003', 'google', 'built-in-google', 'active')
ON CONFLICT (id) DO NOTHING;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM (VALUES
            ('channel_00000000000000000000000000000001','openai'),
            ('channel_00000000000000000000000000000002','xai'),
            ('channel_00000000000000000000000000000003','google')
        ) expected(id, provider)
        JOIN provider_channels channel ON channel.id=expected.id
        WHERE channel.provider <> expected.provider
    ) THEN
        RAISE EXCEPTION 'built-in provider channel identity conflict' USING ERRCODE = '23505';
    END IF;
END $$;

CREATE TABLE image_request_charges (
    id text PRIMARY KEY CHECK (id ~ '^charge_[a-f0-9]{32}$'),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text NOT NULL,
    protocol text NOT NULL CHECK (protocol = 'openai'),
    operation text NOT NULL CHECK (operation IN ('image.generate','image.edit')),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model = btrim(model)),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    price_id text NOT NULL REFERENCES provider_prices(id),
    quantity bigint NOT NULL CHECK (quantity BETWEEN 1 AND 10),
    size text NOT NULL CHECK (length(size) BETWEEN 1 AND 80 AND size = btrim(size)),
    quality text NOT NULL CHECK (length(quality) BETWEEN 1 AND 80 AND quality = btrim(quality)),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    estimated_cost bigint NOT NULL CHECK (estimated_cost >= 0),
    reserved_sale bigint NOT NULL CHECK (reserved_sale > 0),
    actual_cost bigint CHECK (actual_cost >= 0),
    captured_sale bigint NOT NULL DEFAULT 0 CHECK (captured_sale >= 0 AND captured_sale <= reserved_sale),
    reservation_id text NOT NULL UNIQUE REFERENCES wallet_reservations(id),
    state text NOT NULL CHECK (state IN ('RESERVING','RESERVED','CAPTURED','RELEASED','RECONCILING')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, request_id),
    FOREIGN KEY (project_id, organization_id) REFERENCES projects(id, organization_id),
    CHECK ((state = 'CAPTURED' AND actual_cost IS NOT NULL AND captured_sale > 0)
        OR (state <> 'CAPTURED' AND actual_cost IS NULL AND captured_sale = 0))
);
CREATE INDEX image_request_charges_project_idx ON image_request_charges(project_id, created_at, id);
CREATE INDEX image_request_charges_state_idx ON image_request_charges(state, updated_at) WHERE state IN ('RESERVED','RECONCILING');

CREATE FUNCTION enforce_image_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.quantity,OLD.size,OLD.quality,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.created_at)
       IS DISTINCT FROM
       ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.quantity,NEW.size,NEW.quality,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.created_at) THEN
        RAISE EXCEPTION 'image charge identity and estimate are immutable' USING ERRCODE = '55000';
    END IF;
    IF NOT (
        NEW.state = OLD.state OR
        (OLD.state IN ('RESERVING','RESERVED','RECONCILING') AND NEW.state IN ('RESERVED','CAPTURED','RELEASED','RECONCILING'))
    ) THEN
        RAISE EXCEPTION 'invalid image charge transition' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER image_request_charges_update_guard BEFORE UPDATE ON image_request_charges FOR EACH ROW EXECUTE FUNCTION enforce_image_charge_update();

CREATE FUNCTION reject_image_charge_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'image charges are append-only' USING ERRCODE = '55000'; END $$;
CREATE TRIGGER image_request_charges_no_delete BEFORE DELETE ON image_request_charges FOR EACH ROW EXECUTE FUNCTION reject_image_charge_delete();
