CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

CREATE TABLE provider_channels (
    id text PRIMARY KEY CHECK (id ~ '^channel_[a-f0-9]{32}$'),
    provider text NOT NULL CHECK (provider IN ('google','openai','xai','replicate','fal','stability')),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120 AND name = btrim(name)),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE provider_prices (
    id text PRIMARY KEY CHECK (id ~ '^price_[a-f0-9]{32}$'),
    channel_id text NOT NULL REFERENCES provider_channels(id),
    protocol text NOT NULL CHECK (protocol IN ('openai','gemini','anthropic','replicate','fal')),
    operation text NOT NULL CHECK (operation IN ('image.generate','image.edit')),
    model text NOT NULL CHECK (length(model) BETWEEN 1 AND 200 AND model = btrim(model)),
    size text NOT NULL CHECK (length(size) BETWEEN 1 AND 80 AND size = btrim(size)),
    quality text NOT NULL CHECK (length(quality) BETWEEN 1 AND 80 AND quality = btrim(quality)),
    currency text NOT NULL CHECK (currency = 'USD_TICKS'),
    unit_cost bigint NOT NULL CHECK (unit_cost >= 0),
    unit_sale bigint NOT NULL CHECK (unit_sale > 0 AND unit_sale >= unit_cost),
    effective_from timestamptz NOT NULL,
    effective_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (effective_until IS NULL OR effective_until > effective_from),
    EXCLUDE USING gist (
        channel_id WITH =,
        protocol WITH =,
        operation WITH =,
        model WITH =,
        size WITH =,
        quality WITH =,
        tstzrange(effective_from, effective_until, '[)') WITH &&
    )
);
CREATE INDEX provider_prices_lookup_idx ON provider_prices(channel_id, protocol, operation, model, size, quality, effective_from DESC);

CREATE TABLE price_publications (
    id text PRIMARY KEY CHECK (id ~ '^publication_[a-f0-9]{32}$'),
    publication_key text NOT NULL UNIQUE CHECK (length(publication_key) BETWEEN 1 AND 200 AND publication_key = btrim(publication_key)),
    price_id text NOT NULL UNIQUE REFERENCES provider_prices(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION reject_provider_price_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'provider prices are append-only' USING ERRCODE = '55000'; END $$;
CREATE TRIGGER provider_prices_no_update BEFORE UPDATE ON provider_prices FOR EACH ROW EXECUTE FUNCTION reject_provider_price_mutation();
CREATE TRIGGER provider_prices_no_delete BEFORE DELETE ON provider_prices FOR EACH ROW EXECUTE FUNCTION reject_provider_price_mutation();
