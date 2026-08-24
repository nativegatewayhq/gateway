CREATE TABLE plugin_registry_index_snapshots (
    sequence bigint PRIMARY KEY CHECK (sequence > 0),
    index_digest bytea NOT NULL UNIQUE CHECK (octet_length(index_digest)=32),
    envelope_digest bytea NOT NULL UNIQUE CHECK (octet_length(envelope_digest)=32),
    previous_index_digest bytea CHECK (previous_index_digest IS NULL OR octet_length(previous_index_digest)=32),
    registry_created_at timestamptz NOT NULL,
    registry_expires_at timestamptz NOT NULL CHECK (registry_expires_at > registry_created_at),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((sequence=1 AND previous_index_digest IS NULL) OR (sequence>1 AND previous_index_digest IS NOT NULL))
);

CREATE FUNCTION reject_plugin_registry_index_snapshot_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'plugin registry index snapshots are immutable' USING ERRCODE='55000'; END $$;
CREATE TRIGGER plugin_registry_index_snapshots_no_update BEFORE UPDATE OR DELETE ON plugin_registry_index_snapshots FOR EACH ROW EXECUTE FUNCTION reject_plugin_registry_index_snapshot_mutation();

ALTER TABLE plugin_channel_snapshots
    ADD COLUMN registry_sequence bigint REFERENCES plugin_registry_index_snapshots(sequence),
    ADD COLUMN registry_index_digest bytea CHECK (registry_index_digest IS NULL OR octet_length(registry_index_digest)=32),
    ADD COLUMN registry_envelope_digest bytea CHECK (registry_envelope_digest IS NULL OR octet_length(registry_envelope_digest)=32),
    ADD COLUMN registry_admission_digest bytea CHECK (registry_admission_digest IS NULL OR octet_length(registry_admission_digest)=32),
    ADD CONSTRAINT plugin_channel_registry_evidence_complete CHECK (
        (registry_sequence IS NULL AND registry_index_digest IS NULL AND registry_envelope_digest IS NULL AND registry_admission_digest IS NULL)
        OR
        (registry_sequence IS NOT NULL AND registry_index_digest IS NOT NULL AND registry_envelope_digest IS NOT NULL AND registry_admission_digest IS NOT NULL)
    );
