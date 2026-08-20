CREATE TABLE provider_channel_spend_policies (
    id text PRIMARY KEY CHECK (id ~ '^spcap_[a-f0-9]{32}$'),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    period text NOT NULL CHECK (period IN ('day','month')),
    currency text NOT NULL CHECK (currency='USD_TICKS'),
    limit_amount bigint NOT NULL CHECK (limit_amount > 0),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX provider_channel_spend_policies_active_unique ON provider_channel_spend_policies(channel_id,period) WHERE status='active';

CREATE TABLE provider_channel_spend_policy_events (
    id text PRIMARY KEY CHECK (id ~ '^spevt_[a-f0-9]{32}$'),
    policy_id text NOT NULL REFERENCES provider_channel_spend_policies(id),
    version bigint NOT NULL CHECK (version > 0),
    action text NOT NULL CHECK (action IN ('create','update','disable')),
    actor text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200 AND actor=btrim(actor)),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500 AND reason=btrim(reason)),
    limit_amount bigint NOT NULL CHECK (limit_amount > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(policy_id,version)
);

CREATE TABLE provider_channel_spend_buckets (
    policy_id text NOT NULL REFERENCES provider_channel_spend_policies(id),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    currency text NOT NULL CHECK (currency='USD_TICKS'),
    reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    captured bigint NOT NULL DEFAULT 0 CHECK (captured >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(policy_id,period_start),
    CHECK(period_end > period_start)
);

CREATE TABLE provider_channel_spend_allocations (
    charge_id text NOT NULL REFERENCES image_request_charges(id),
    policy_id text NOT NULL REFERENCES provider_channel_spend_policies(id),
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    limit_snapshot bigint NOT NULL CHECK (limit_snapshot > 0),
    reserved_cost bigint NOT NULL CHECK (reserved_cost > 0),
    captured_cost bigint NOT NULL DEFAULT 0 CHECK (captured_cost >= 0 AND captured_cost <= reserved_cost),
    state text NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved','captured','released')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(charge_id,policy_id),
    FOREIGN KEY(policy_id,period_start) REFERENCES provider_channel_spend_buckets(policy_id,period_start),
    CHECK ((state='captured' AND captured_cost > 0) OR (state<>'captured' AND captured_cost=0))
);
CREATE INDEX provider_channel_spend_allocations_policy_idx ON provider_channel_spend_allocations(policy_id,period_start,state);

CREATE FUNCTION reject_provider_spend_event_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'provider spend events are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER provider_spend_events_no_update BEFORE UPDATE OR DELETE ON provider_channel_spend_policy_events FOR EACH ROW EXECUTE FUNCTION reject_provider_spend_event_change();
