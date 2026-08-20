# fal native Queue client examples

Set a Gateway HTTPS origin, service key, and an allowlisted fal model:

```bash
export GATEWAY_URL='https://gateway.example.com'
export GATEWAY_API_KEY='ngw_sk_...'
export FAL_MODEL='fal-ai/flux/dev'
```

The JavaScript example uses the official client's documented always-on `proxyUrl` middleware. The Gateway proxy endpoint validates the SDK target but never fetches an arbitrary target URL. The Python example sets the official client's `FAL_QUEUE_RUN_HOST` before importing `fal_client`; this client constructs Queue URLs from that host at import time.

The configured model must appear in `GATEWAY_FAL_MODELS` and have an active fal channel and exact price when billing is required. Upload transforms, webhooks, streaming, and `fal.run` are not supported by this Queue release.
