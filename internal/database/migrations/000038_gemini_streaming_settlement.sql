ALTER TABLE chat_usage_evidence DROP CONSTRAINT chat_usage_evidence_schema_version_check;
ALTER TABLE chat_usage_evidence ADD CONSTRAINT chat_usage_evidence_schema_version_check CHECK (
    schema_version IN (
        'openai-chat-usage-v1',
        'openai-responses-usage-v1',
        'openai-responses-stream-usage-v1',
        'gemini-usage-v1',
        'gemini-stream-usage-v1'
    )
);
