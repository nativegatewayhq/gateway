ALTER TABLE image_request_charges
    DROP CONSTRAINT image_request_charges_protocol_check;

ALTER TABLE image_request_charges
    ADD CONSTRAINT image_request_charges_protocol_check
    CHECK (protocol IN ('openai', 'gemini'));
