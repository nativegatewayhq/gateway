import os

from openai import OpenAI

client = OpenAI(
    api_key=os.environ["GATEWAY_API_KEY"],
    base_url=os.environ.get("GATEWAY_BASE_URL", "http://127.0.0.1:8080/v1"),
)
response = client.chat.completions.create(
    model=os.environ.get("GATEWAY_CHAT_MODEL", "gpt-4.1"),
    messages=[{"role": "user", "content": "Reply with: gateway ok"}],
)
print(response.choices[0].message.content)
