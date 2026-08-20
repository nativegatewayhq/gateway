import OpenAI from "openai";

const client = new OpenAI({
  apiKey: process.env.GATEWAY_API_KEY,
  baseURL: process.env.GATEWAY_BASE_URL ?? "http://127.0.0.1:8080/v1",
});
const response = await client.chat.completions.create({
  model: process.env.GATEWAY_CHAT_MODEL ?? "gpt-4.1",
  messages: [{ role: "user", content: "Reply with: gateway ok" }],
});
console.log(response.choices[0].message.content);
