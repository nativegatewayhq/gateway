ALTER TABLE provider_prices DROP CONSTRAINT provider_prices_protocol_check;
ALTER TABLE provider_prices ADD CONSTRAINT provider_prices_protocol_check CHECK (protocol IN ('openai','gemini','anthropic','replicate','fal','runway'));
ALTER TABLE provider_prices DROP CONSTRAINT provider_prices_operation_check;
ALTER TABLE provider_prices ADD CONSTRAINT provider_prices_operation_check CHECK (operation IN ('image.generate','image.edit','video.generate'));

CREATE TABLE video_credit_prices (
    price_id text PRIMARY KEY REFERENCES provider_prices(id),
    credits_per_second_micros bigint NOT NULL CHECK (credits_per_second_micros >= 0),
    fixed_credits_micros bigint NOT NULL CHECK (fixed_credits_micros >= 0),
    minimum_credits_micros bigint NOT NULL CHECK (minimum_credits_micros >= 0),
    CHECK (credits_per_second_micros > 0 OR fixed_credits_micros > 0),
    CHECK (credits_per_second_micros <= 1000000000000000 AND fixed_credits_micros <= 1000000000000000 AND minimum_credits_micros <= 1000000000000000)
);

CREATE TABLE video_credit_price_publications (
    id text PRIMARY KEY CHECK (id ~ '^publication_[a-f0-9]{32}$'),
    publication_key text NOT NULL UNIQUE CHECK (length(publication_key) BETWEEN 1 AND 200 AND publication_key=btrim(publication_key)),
    price_id text NOT NULL UNIQUE REFERENCES video_credit_prices(price_id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER video_credit_prices_no_update BEFORE UPDATE OR DELETE ON video_credit_prices FOR EACH ROW EXECUTE FUNCTION reject_provider_price_mutation();
CREATE TRIGGER video_credit_price_publications_no_update BEFORE UPDATE OR DELETE ON video_credit_price_publications FOR EACH ROW EXECUTE FUNCTION reject_provider_price_mutation();

ALTER TABLE image_request_charges DROP CONSTRAINT image_request_charges_protocol_check;
ALTER TABLE image_request_charges ADD CONSTRAINT image_request_charges_protocol_check CHECK (protocol IN ('openai','gemini','replicate','fal','runway'));
ALTER TABLE image_request_charges DROP CONSTRAINT image_request_charges_operation_check;
ALTER TABLE image_request_charges ADD CONSTRAINT image_request_charges_operation_check CHECK (operation IN ('image.generate','image.edit','video.generate'));
ALTER TABLE image_request_charges DROP CONSTRAINT image_request_charges_quantity_check;
ALTER TABLE image_request_charges ADD CONSTRAINT image_request_charges_quantity_check CHECK (quantity BETWEEN 1 AND 1000000000000000);
ALTER TABLE image_request_charges ADD COLUMN pricing_quantity bigint;
UPDATE image_request_charges SET pricing_quantity=quantity;
ALTER TABLE image_request_charges ALTER COLUMN pricing_quantity SET NOT NULL;
ALTER TABLE image_request_charges ADD CONSTRAINT image_request_charges_pricing_quantity_check CHECK (pricing_quantity BETWEEN 1 AND 60);

ALTER TABLE cost_quota_policies DROP CONSTRAINT cost_quota_policies_dimension_check;
ALTER TABLE cost_quota_policies ADD CONSTRAINT cost_quota_policies_dimension_check CHECK (
    (protocol IS NULL AND operation IS NULL AND model IS NULL)
    OR (((protocol='openai' AND operation IN ('image.generate','image.edit','chat.completions','responses.create'))
         OR (protocol='gemini' AND operation IN ('image.generate','chat.completions'))
         OR (protocol='anthropic' AND operation='messages.create')
         OR (protocol IN ('replicate','fal') AND operation='image.generate')
         OR (protocol='runway' AND operation='video.generate'))
        AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model))
);

CREATE OR REPLACE FUNCTION enforce_image_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.quantity,OLD.pricing_quantity,OLD.size,OLD.quality,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.created_at,OLD.idempotency_key,OLD.request_fingerprint,OLD.routing_policy,OLD.cost_rank,OLD.price_evaluated_at)
	   IS DISTINCT FROM
	   ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.quantity,NEW.pricing_quantity,NEW.size,NEW.quality,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.created_at,NEW.idempotency_key,NEW.request_fingerprint,NEW.routing_policy,NEW.cost_rank,NEW.price_evaluated_at) THEN
		RAISE EXCEPTION 'charge identity and estimate are immutable' USING ERRCODE='55000';
	END IF;
    IF NOT (NEW.state=OLD.state OR (OLD.state IN ('RESERVING','RESERVED','RECONCILING') AND NEW.state IN ('RESERVED','CAPTURED','RELEASED','RECONCILING'))) THEN
		RAISE EXCEPTION 'invalid charge transition' USING ERRCODE='55000';
	END IF;
	IF OLD.response_snapshot_version=1 AND
	   ROW(OLD.response_snapshot_version,OLD.response_status,OLD.response_headers,OLD.response_body,OLD.response_body_sha256,OLD.response_completed_at)
	   IS DISTINCT FROM
	   ROW(NEW.response_snapshot_version,NEW.response_status,NEW.response_headers,NEW.response_body,NEW.response_body_sha256,NEW.response_completed_at) THEN
		RAISE EXCEPTION 'terminal response snapshot is immutable' USING ERRCODE='55000';
	END IF;
	RETURN NEW;
END $$;

ALTER TABLE async_jobs DROP CONSTRAINT async_jobs_usage_estimate_check;
ALTER TABLE async_jobs ADD CONSTRAINT async_jobs_usage_estimate_check CHECK (
    (usage_dimension IS NULL AND usage_unit IS NULL AND estimated_quantity IS NULL AND usage_extractor_version IS NULL AND usage_result_extractor_version IS NULL)
    OR (usage_dimension='output' AND usage_unit='image' AND estimated_quantity BETWEEN 1 AND 10 AND length(usage_extractor_version) BETWEEN 1 AND 80 AND length(usage_result_extractor_version) BETWEEN 1 AND 80)
    OR (usage_dimension='provider_credit' AND usage_unit='microcredit' AND estimated_quantity BETWEEN 1 AND 1000000000000000 AND length(usage_extractor_version) BETWEEN 1 AND 80 AND length(usage_result_extractor_version) BETWEEN 1 AND 80)
);
ALTER TABLE async_job_usage_evidence DROP CONSTRAINT async_job_usage_evidence_provider_check;
ALTER TABLE async_job_usage_evidence ADD CONSTRAINT async_job_usage_evidence_provider_check CHECK (provider IN ('replicate','fal','runway'));
ALTER TABLE async_job_usage_evidence DROP CONSTRAINT async_job_usage_evidence_dimension_check;
ALTER TABLE async_job_usage_evidence ADD CONSTRAINT async_job_usage_evidence_dimension_check CHECK (dimension IN ('output','provider_credit'));
ALTER TABLE async_job_usage_evidence DROP CONSTRAINT async_job_usage_evidence_unit_check;
ALTER TABLE async_job_usage_evidence ADD CONSTRAINT async_job_usage_evidence_unit_check CHECK (unit IN ('image','microcredit'));
ALTER TABLE async_job_usage_evidence DROP CONSTRAINT async_job_usage_evidence_quantity_check;
ALTER TABLE async_job_usage_evidence ADD CONSTRAINT async_job_usage_evidence_quantity_check CHECK (quantity BETWEEN 0 AND 1000000000000000);
ALTER TABLE async_job_usage_evidence ADD CONSTRAINT async_job_usage_evidence_dimension_unit_check CHECK ((dimension='output' AND unit='image') OR (dimension='provider_credit' AND unit='microcredit'));
