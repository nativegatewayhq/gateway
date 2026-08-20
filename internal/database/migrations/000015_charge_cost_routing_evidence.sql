ALTER TABLE image_request_charges
    ADD COLUMN routing_policy text,
    ADD COLUMN cost_rank integer,
    ADD COLUMN price_evaluated_at timestamptz,
    ADD CONSTRAINT image_request_charges_routing_evidence_check CHECK (
        (routing_policy IS NULL AND cost_rank IS NULL AND price_evaluated_at IS NULL) OR
        (routing_policy = 'lowest_cost' AND cost_rank >= 0 AND price_evaluated_at IS NOT NULL)
    );

CREATE OR REPLACE FUNCTION enforce_image_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.quantity,OLD.size,OLD.quality,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.created_at,OLD.routing_policy,OLD.cost_rank,OLD.price_evaluated_at)
       IS DISTINCT FROM
       ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.quantity,NEW.size,NEW.quality,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.created_at,NEW.routing_policy,NEW.cost_rank,NEW.price_evaluated_at) THEN
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
