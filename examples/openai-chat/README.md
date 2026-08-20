# OpenAI Chat SDK conformance handoff

These examples exercise the Gateway-owned non-streaming Chat Completions contract with the official OpenAI Python and JavaScript SDKs. Start Gateway with `GATEWAY_OPENAI_CHAT_MODELS=gpt-4.1`, then set `GATEWAY_BASE_URL` and `GATEWAY_API_KEY`.

```bash
python -m pip install openai
python examples/openai-chat/python.py

npm install openai
node examples/openai-chat/javascript.mjs
```

The Base URL includes `/v1`. `stream: true` is intentionally unsupported in Plan 036.
