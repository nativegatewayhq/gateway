CREATE TABLE audio_transcription_prices (
 id text PRIMARY KEY CHECK(id ~ '^atp_[a-f0-9]{32}$'), channel_id text NOT NULL REFERENCES provider_channels(id),
 protocol text NOT NULL CHECK(protocol='openai'), operation text NOT NULL CHECK(operation='audio.transcription'),
 model text NOT NULL CHECK(length(model) BETWEEN 1 AND 200 AND model=btrim(model)),
 strategy text NOT NULL CHECK(strategy IN('openai-transcription-token-v1','openai-transcription-duration-v1')), currency text NOT NULL CHECK(currency='USD_TICKS'),
 cost_input_per_million_tokens bigint NOT NULL DEFAULT 0 CHECK(cost_input_per_million_tokens>=0), cost_output_per_million_tokens bigint NOT NULL DEFAULT 0 CHECK(cost_output_per_million_tokens>=0),
 sale_input_per_million_tokens bigint NOT NULL DEFAULT 0 CHECK(sale_input_per_million_tokens>=0), sale_output_per_million_tokens bigint NOT NULL DEFAULT 0 CHECK(sale_output_per_million_tokens>=0),
 cost_per_minute bigint NOT NULL DEFAULT 0 CHECK(cost_per_minute>=0), sale_per_minute bigint NOT NULL DEFAULT 0 CHECK(sale_per_minute>=0),
 maximum_input_tokens bigint NOT NULL DEFAULT 0 CHECK(maximum_input_tokens>=0), maximum_output_tokens bigint NOT NULL DEFAULT 0 CHECK(maximum_output_tokens>=0),
 maximum_duration_milliseconds bigint NOT NULL DEFAULT 0 CHECK(maximum_duration_milliseconds>=0),
 effective_from timestamptz NOT NULL, effective_until timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(effective_until IS NULL OR effective_until>effective_from),
 CHECK((strategy='openai-transcription-token-v1' AND sale_input_per_million_tokens>0 AND sale_output_per_million_tokens>0 AND maximum_input_tokens BETWEEN 1 AND 10000000 AND maximum_output_tokens BETWEEN 1 AND 10000000 AND cost_per_minute=0 AND sale_per_minute=0 AND maximum_duration_milliseconds=0) OR (strategy='openai-transcription-duration-v1' AND sale_per_minute>0 AND maximum_duration_milliseconds BETWEEN 1 AND 86400000 AND cost_input_per_million_tokens=0 AND cost_output_per_million_tokens=0 AND sale_input_per_million_tokens=0 AND sale_output_per_million_tokens=0 AND maximum_input_tokens=0 AND maximum_output_tokens=0)),
 EXCLUDE USING gist(channel_id WITH =,protocol WITH =,operation WITH =,model WITH =,tstzrange(effective_from,effective_until,'[)') WITH &&)
);
CREATE INDEX audio_transcription_prices_lookup_idx ON audio_transcription_prices(channel_id,model,effective_from DESC);
CREATE TABLE audio_transcription_price_publications(publication_key text PRIMARY KEY CHECK(length(publication_key) BETWEEN 1 AND 200 AND publication_key=btrim(publication_key)),price_id text NOT NULL UNIQUE REFERENCES audio_transcription_prices(id),created_at timestamptz NOT NULL DEFAULT now());
CREATE TRIGGER audio_transcription_prices_no_update BEFORE UPDATE OR DELETE ON audio_transcription_prices FOR EACH ROW EXECUTE FUNCTION reject_provider_price_mutation();
CREATE TRIGGER audio_transcription_publications_no_update BEFORE UPDATE OR DELETE ON audio_transcription_price_publications FOR EACH ROW EXECUTE FUNCTION reject_provider_price_mutation();

CREATE TABLE audio_transcription_charges(
 id text PRIMARY KEY CHECK(id ~ '^atc_[a-f0-9]{32}$'), request_id text NOT NULL CHECK(length(request_id) BETWEEN 1 AND 128),
 organization_id text NOT NULL REFERENCES organizations(id), project_id text NOT NULL, api_key_id text NOT NULL,
 protocol text NOT NULL CHECK(protocol='openai'),operation text NOT NULL CHECK(operation='audio.transcription'),model text NOT NULL CHECK(length(model) BETWEEN 1 AND 200),
 channel_id text NOT NULL REFERENCES provider_channels(id),price_id text NOT NULL REFERENCES audio_transcription_prices(id),strategy text NOT NULL CHECK(strategy IN('openai-transcription-token-v1','openai-transcription-duration-v1')),
 maximum_input_tokens bigint NOT NULL CHECK(maximum_input_tokens>=0),maximum_output_tokens bigint NOT NULL CHECK(maximum_output_tokens>=0),maximum_duration_milliseconds bigint NOT NULL CHECK(maximum_duration_milliseconds>=0),
 currency text NOT NULL CHECK(currency='USD_TICKS'),estimated_cost bigint NOT NULL CHECK(estimated_cost>=0),reserved_sale bigint NOT NULL CHECK(reserved_sale>0),actual_cost bigint,captured_sale bigint NOT NULL DEFAULT 0,
 reservation_id text NOT NULL UNIQUE REFERENCES wallet_reservations(id),state text NOT NULL CHECK(state IN('RESERVED','CAPTURED','RELEASED','RECONCILING')),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 200),request_fingerprint bytea NOT NULL CHECK(octet_length(request_fingerprint)=32),completed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(organization_id,request_id),UNIQUE(organization_id,idempotency_key),FOREIGN KEY(project_id,organization_id) REFERENCES projects(id,organization_id),FOREIGN KEY(api_key_id,project_id) REFERENCES service_api_keys(id,project_id),
 CHECK((strategy='openai-transcription-token-v1' AND maximum_input_tokens>0 AND maximum_output_tokens>0 AND maximum_duration_milliseconds=0) OR (strategy='openai-transcription-duration-v1' AND maximum_input_tokens=0 AND maximum_output_tokens=0 AND maximum_duration_milliseconds>0)),
 CHECK((state='CAPTURED' AND actual_cost IS NOT NULL AND captured_sale>0 AND completed_at IS NOT NULL) OR (state<>'CAPTURED' AND actual_cost IS NULL AND captured_sale=0)),
 CHECK(captured_sale<=reserved_sale)
);
CREATE INDEX audio_transcription_charges_state_idx ON audio_transcription_charges(state,updated_at) WHERE state IN('RESERVED','RECONCILING');
CREATE FUNCTION enforce_audio_transcription_charge_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF ROW(OLD.request_id,OLD.organization_id,OLD.project_id,OLD.api_key_id,OLD.protocol,OLD.operation,OLD.model,OLD.channel_id,OLD.price_id,OLD.strategy,OLD.maximum_input_tokens,OLD.maximum_output_tokens,OLD.maximum_duration_milliseconds,OLD.currency,OLD.estimated_cost,OLD.reserved_sale,OLD.reservation_id,OLD.idempotency_key,OLD.request_fingerprint,OLD.created_at) IS DISTINCT FROM ROW(NEW.request_id,NEW.organization_id,NEW.project_id,NEW.api_key_id,NEW.protocol,NEW.operation,NEW.model,NEW.channel_id,NEW.price_id,NEW.strategy,NEW.maximum_input_tokens,NEW.maximum_output_tokens,NEW.maximum_duration_milliseconds,NEW.currency,NEW.estimated_cost,NEW.reserved_sale,NEW.reservation_id,NEW.idempotency_key,NEW.request_fingerprint,NEW.created_at) THEN RAISE EXCEPTION 'audio transcription charge identity is immutable' USING ERRCODE='55000'; END IF;
 IF NOT(NEW.state=OLD.state OR (OLD.state IN('RESERVED','RECONCILING') AND NEW.state IN('CAPTURED','RELEASED','RECONCILING'))) THEN RAISE EXCEPTION 'invalid audio transcription charge transition' USING ERRCODE='55000'; END IF;
 IF OLD.state IN('CAPTURED','RELEASED') AND ROW(OLD.actual_cost,OLD.captured_sale,OLD.completed_at) IS DISTINCT FROM ROW(NEW.actual_cost,NEW.captured_sale,NEW.completed_at) THEN RAISE EXCEPTION 'terminal audio transcription charge is immutable' USING ERRCODE='55000'; END IF; RETURN NEW; END $$;
CREATE TRIGGER audio_transcription_charges_update_guard BEFORE UPDATE ON audio_transcription_charges FOR EACH ROW EXECUTE FUNCTION enforce_audio_transcription_charge_update();
CREATE FUNCTION reject_audio_transcription_charge_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audio transcription charges are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER audio_transcription_charges_no_delete BEFORE DELETE ON audio_transcription_charges FOR EACH ROW EXECUTE FUNCTION reject_audio_transcription_charge_delete();

CREATE TABLE audio_transcription_usage_evidence(
 charge_id text PRIMARY KEY REFERENCES audio_transcription_charges(id),schema_version text NOT NULL CHECK(schema_version IN('openai-transcription-token-json-v1','openai-transcription-duration-json-v1','openai-transcription-token-sse-v1','openai-transcription-duration-sse-v1')),
 usage_type text NOT NULL CHECK(usage_type IN('tokens','duration')),input_tokens bigint NOT NULL DEFAULT 0 CHECK(input_tokens>=0),audio_input_tokens bigint NOT NULL DEFAULT 0 CHECK(audio_input_tokens>=0),text_input_tokens bigint NOT NULL DEFAULT 0 CHECK(text_input_tokens>=0),output_tokens bigint NOT NULL DEFAULT 0 CHECK(output_tokens>=0),total_tokens bigint NOT NULL DEFAULT 0 CHECK(total_tokens>=0),duration_milliseconds bigint NOT NULL DEFAULT 0 CHECK(duration_milliseconds>=0),
 response_status integer NOT NULL CHECK(response_status BETWEEN 200 AND 299),response_headers jsonb NOT NULL DEFAULT '{}',response_sha256 bytea NOT NULL CHECK(octet_length(response_sha256)=32),created_at timestamptz NOT NULL DEFAULT now(),
 CHECK((usage_type='tokens' AND input_tokens=audio_input_tokens+text_input_tokens AND total_tokens=input_tokens+output_tokens AND duration_milliseconds=0) OR (usage_type='duration' AND input_tokens=0 AND audio_input_tokens=0 AND text_input_tokens=0 AND output_tokens=0 AND total_tokens=0 AND duration_milliseconds>0))
);
CREATE TRIGGER audio_transcription_usage_no_mutation BEFORE UPDATE OR DELETE ON audio_transcription_usage_evidence FOR EACH ROW EXECUTE FUNCTION reject_audio_event_mutation();
CREATE TABLE audio_transcription_charge_events(id bigserial PRIMARY KEY,charge_id text NOT NULL REFERENCES audio_transcription_charges(id),event_type text NOT NULL CHECK(event_type IN('RESERVED','CAPTURED','RELEASED','RECONCILING')),reason text,created_at timestamptz NOT NULL DEFAULT now());
CREATE TRIGGER audio_transcription_events_no_mutation BEFORE UPDATE OR DELETE ON audio_transcription_charge_events FOR EACH ROW EXECUTE FUNCTION reject_audio_event_mutation();
CREATE TABLE audio_transcription_reconciliations(charge_id text PRIMARY KEY REFERENCES audio_transcription_charges(id),reason text NOT NULL,state text NOT NULL DEFAULT 'PENDING' CHECK(state IN('PENDING','LEASED','MANUAL_REVIEW','RESOLVED')),attempt_count integer NOT NULL DEFAULT 0,next_attempt_at timestamptz NOT NULL DEFAULT now(),lease_owner text,lease_until timestamptz,last_error_category text,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),resolved_at timestamptz);

CREATE OR REPLACE FUNCTION require_known_charge() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM image_request_charges WHERE id=NEW.charge_id) AND NOT EXISTS(SELECT 1 FROM chat_request_charges WHERE id=NEW.charge_id) AND NOT EXISTS(SELECT 1 FROM audio_speech_charges WHERE id=NEW.charge_id) AND NOT EXISTS(SELECT 1 FROM audio_transcription_charges WHERE id=NEW.charge_id) THEN RAISE EXCEPTION 'allocation requires a known charge' USING ERRCODE='23503'; END IF; RETURN NEW; END $$;
ALTER TABLE cost_quota_policies DROP CONSTRAINT cost_quota_policies_dimension_check;
ALTER TABLE cost_quota_policies ADD CONSTRAINT cost_quota_policies_dimension_check CHECK((protocol IS NULL AND operation IS NULL AND model IS NULL) OR (((protocol='openai' AND operation IN('image.generate','image.edit','chat.completions','responses.create','audio.speech','audio.transcription')) OR (protocol='gemini' AND operation IN('image.generate','chat.completions')) OR (protocol='anthropic' AND operation='messages.create') OR (protocol IN('replicate','fal') AND operation='image.generate') OR (protocol='runway' AND operation='video.generate')) AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model)));
