CREATE TABLE organization_wallets (
    organization_id text NOT NULL REFERENCES organizations(id),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    available bigint NOT NULL DEFAULT 0 CHECK (available >= 0),
    reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, currency)
);

CREATE TABLE wallet_reservations (
    id text PRIMARY KEY CHECK (id ~ '^res_[a-f0-9]{32}$'),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text NOT NULL,
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 200),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    maximum bigint NOT NULL CHECK (maximum > 0),
    captured bigint NOT NULL DEFAULT 0 CHECK (captured >= 0 AND captured <= maximum),
    refunded bigint NOT NULL DEFAULT 0 CHECK (refunded >= 0 AND refunded <= captured),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','captured','released')),
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, request_id),
    FOREIGN KEY (project_id, organization_id) REFERENCES projects(id, organization_id)
);

CREATE TABLE wallet_operations (
    id text PRIMARY KEY CHECK (id ~ '^wop_[a-f0-9]{32}$'),
    organization_id text NOT NULL REFERENCES organizations(id),
    operation_key text NOT NULL CHECK (length(operation_key) BETWEEN 1 AND 200),
    kind text NOT NULL CHECK (kind IN ('deposit','reserve','capture','release','refund')),
    amount bigint NOT NULL CHECK (amount >= 0),
    reservation_id text REFERENCES wallet_reservations(id),
    result_available bigint NOT NULL CHECK (result_available >= 0),
    result_reserved bigint NOT NULL CHECK (result_reserved >= 0),
    result_state text,
    result_project_id text,
    result_request_id text,
    result_maximum bigint NOT NULL CHECK (result_maximum >= 0),
    result_captured bigint NOT NULL CHECK (result_captured >= 0 AND result_captured <= result_maximum),
    result_refunded bigint NOT NULL CHECK (result_refunded >= 0 AND result_refunded <= result_captured),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, operation_key)
);

CREATE TABLE ledger_entries (
    id text PRIMARY KEY CHECK (id ~ '^led_[a-f0-9]{32}$'),
    operation_id text NOT NULL REFERENCES wallet_operations(id),
    organization_id text NOT NULL REFERENCES organizations(id),
    project_id text REFERENCES projects(id),
    reservation_id text REFERENCES wallet_reservations(id),
    entry_type text NOT NULL CHECK (entry_type IN ('deposit','reserve','capture','release','refund')),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    delta_available bigint NOT NULL,
    delta_reserved bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (delta_available <> 0 OR delta_reserved <> 0)
);
CREATE INDEX ledger_entries_organization_idx ON ledger_entries(organization_id, created_at, id);
CREATE INDEX ledger_entries_reservation_idx ON ledger_entries(reservation_id) WHERE reservation_id IS NOT NULL;

CREATE FUNCTION reject_ledger_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'ledger entries are append-only' USING ERRCODE = '55000'; END $$;
CREATE TRIGGER ledger_entries_no_update BEFORE UPDATE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();
CREATE TRIGGER ledger_entries_no_delete BEFORE DELETE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_ledger_mutation();
