ALTER TABLE image_charge_reconciliations DROP CONSTRAINT image_charge_reconciliations_reason_check;
ALTER TABLE image_charge_reconciliations ADD CONSTRAINT image_charge_reconciliations_reason_check CHECK (reason IN ('response_unavailable','settlement_failed','storage_failed','executor_timeout','executor_connection_lost','provider_panic','legacy_unknown'));
