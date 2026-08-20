ALTER TABLE chat_request_charges DROP CONSTRAINT chat_route_evidence_check;

ALTER TABLE chat_request_charges ADD CONSTRAINT chat_route_evidence_check CHECK (
    (candidate_id IS NULL AND provider IS NULL AND provider_model IS NULL AND routing_policy IS NULL AND route_rank IS NULL AND price_evaluated_at IS NULL AND route_evidence_version IS NULL)
    OR
    (length(candidate_id) BETWEEN 1 AND 200 AND candidate_id=btrim(candidate_id)
     AND provider IN ('openai','xai')
     AND length(provider_model) BETWEEN 1 AND 200 AND provider_model=btrim(provider_model)
     AND routing_policy IN ('fixed','priority','weighted','lowest_cost')
     AND route_rank >= 0
     AND price_evaluated_at IS NOT NULL
     AND ((protocol='openai' AND operation='chat.completions' AND route_evidence_version='openai-chat-route-v1')
       OR (protocol='openai' AND operation='responses.create' AND route_evidence_version='openai-responses-route-v1')))
);
