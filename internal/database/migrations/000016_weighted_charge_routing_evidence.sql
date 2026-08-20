ALTER TABLE image_request_charges
    DROP CONSTRAINT image_request_charges_routing_evidence_check,
    ADD CONSTRAINT image_request_charges_routing_evidence_check CHECK (
        (routing_policy IS NULL AND cost_rank IS NULL AND price_evaluated_at IS NULL) OR
        (routing_policy = 'lowest_cost' AND cost_rank >= 0 AND price_evaluated_at IS NOT NULL) OR
        (routing_policy = 'weighted' AND cost_rank >= 0 AND price_evaluated_at IS NULL)
    );
