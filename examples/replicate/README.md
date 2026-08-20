# Replicate native client examples

These examples keep the official Replicate request and response protocol while replacing only the credential and base URL. Before running them, set:

```bash
export GATEWAY_URL='https://gateway.example.com'
export GATEWAY_API_KEY='ngw_sk_...'
export REPLICATE_MODEL_VERSION='owner/model:version'
```

For JavaScript, install the current `replicate` package and run `node javascript.mjs`. For Python, install the current Replicate v2 package and run `python python_v2.py`. The configured model version must also appear in `GATEWAY_REPLICATE_MODELS` and have an active channel and price when billing is required.

Create requests should include a stable `Idempotency-Key` in production. The SDKs may expose this through request options; direct HTTP clients can set the header explicitly.

When the deployment enables required signed webhooks, clients do not change their request. The Gateway injects a private completed-event callback and never returns its capability URL. Client-provided `webhook` and `webhook_events_filter` fields are rejected; polling through the SDK remains available and is the recovery path for missed callbacks.
