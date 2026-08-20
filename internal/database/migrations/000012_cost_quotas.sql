ALTER TABLE service_api_keys ADD CONSTRAINT service_api_keys_id_project_unique UNIQUE(id,project_id);

CREATE TABLE cost_quota_policies (
    id text PRIMARY KEY CHECK (id ~ '^quota_[a-f0-9]{32}$'),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    scope_type text NOT NULL CHECK (scope_type IN ('organization','project','api_key')),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text REFERENCES projects(id),
    api_key_id text REFERENCES service_api_keys(id),
    protocol text,
    operation text,
    model text,
    period text NOT NULL CHECK (period IN ('day','month')),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    limit_amount bigint NOT NULL CHECK (limit_amount > 0),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope_type='organization' AND project_id IS NULL AND api_key_id IS NULL)
        OR (scope_type='project' AND project_id IS NOT NULL AND api_key_id IS NULL)
        OR (scope_type='api_key' AND project_id IS NOT NULL AND api_key_id IS NOT NULL)),
    CHECK ((protocol IS NULL AND operation IS NULL AND model IS NULL)
        OR (((protocol='openai' AND operation IN ('image.generate','image.edit')) OR (protocol='gemini' AND operation='image.generate'))
            AND model IS NOT NULL AND length(model) BETWEEN 1 AND 200 AND model=btrim(model))),
    FOREIGN KEY (project_id, organization_id) REFERENCES projects(id, organization_id),
    FOREIGN KEY (api_key_id, project_id) REFERENCES service_api_keys(id, project_id)
);
CREATE UNIQUE INDEX cost_quota_policies_active_dimension_unique ON cost_quota_policies(
    scope_type, organization_id, COALESCE(project_id,''), COALESCE(api_key_id,''),
    COALESCE(protocol,''), COALESCE(operation,''), COALESCE(model,''), period
) WHERE status='active';

CREATE TABLE cost_quota_policy_events (
    id text PRIMARY KEY CHECK (id ~ '^qevt_[a-f0-9]{32}$'),
    policy_id text NOT NULL REFERENCES cost_quota_policies(id),
    version bigint NOT NULL CHECK (version > 0),
    action text NOT NULL CHECK (action IN ('create','update','disable')),
    actor text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200 AND actor=btrim(actor)),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500 AND reason=btrim(reason)),
    limit_amount bigint NOT NULL CHECK (limit_amount > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(policy_id, version)
);

CREATE TABLE cost_quota_buckets (
    policy_id text NOT NULL REFERENCES cost_quota_policies(id),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    currency text NOT NULL CHECK (currency='USD_TICKS'),
    reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    captured bigint NOT NULL DEFAULT 0 CHECK (captured >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(policy_id, period_start),
    CHECK (period_end > period_start)
);

CREATE TABLE cost_quota_allocations (
    charge_id text NOT NULL REFERENCES image_request_charges(id),
    policy_id text NOT NULL REFERENCES cost_quota_policies(id),
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    limit_snapshot bigint NOT NULL CHECK (limit_snapshot > 0),
    reserved_amount bigint NOT NULL CHECK (reserved_amount > 0),
    captured_amount bigint NOT NULL DEFAULT 0 CHECK (captured_amount >= 0 AND captured_amount <= reserved_amount),
    state text NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved','captured','released')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(charge_id, policy_id),
    FOREIGN KEY (policy_id, period_start) REFERENCES cost_quota_buckets(policy_id, period_start),
    CHECK ((state='captured' AND captured_amount > 0) OR (state<>'captured' AND captured_amount=0))
);
CREATE INDEX cost_quota_allocations_policy_period_idx ON cost_quota_allocations(policy_id,period_start,state);

CREATE FUNCTION reject_cost_quota_event_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'cost quota events are append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER cost_quota_events_no_update BEFORE UPDATE OR DELETE ON cost_quota_policy_events FOR EACH ROW EXECUTE FUNCTION reject_cost_quota_event_change();
