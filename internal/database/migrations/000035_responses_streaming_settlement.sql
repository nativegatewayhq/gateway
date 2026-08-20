ALTER TABLE chat_usage_evidence DROP CONSTRAINT chat_usage_evidence_schema_version_check;
ALTER TABLE chat_usage_evidence ADD CONSTRAINT chat_usage_evidence_schema_version_check CHECK (schema_version IN ('openai-chat-usage-v1','openai-responses-usage-v1','openai-responses-stream-usage-v1'));

ALTER TABLE chat_charge_reconciliations DROP CONSTRAINT chat_charge_reconciliations_terminal_category_check;
ALTER TABLE chat_charge_reconciliations ADD CONSTRAINT chat_charge_reconciliations_terminal_category_check CHECK (terminal_category IS NULL OR terminal_category IN ('complete','missing_usage','invalid_usage','missing_done','missing_terminal','write_failed','provider_error','client_disconnect','response_failed','response_incomplete','error_event'));
