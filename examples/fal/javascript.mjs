import { fal } from "@fal-ai/client";

const gateway = process.env.GATEWAY_URL.replace(/\/$/, "");

fal.config({
  credentials: process.env.GATEWAY_API_KEY,
  proxyUrl: { url: `${gateway}/fal/proxy`, when: "always" },
});

const queued = await fal.queue.submit(process.env.FAL_MODEL, {
  input: { prompt: "a cat astronaut" },
});

console.log(await fal.queue.status(process.env.FAL_MODEL, {
  requestId: queued.request_id,
  logs: false,
}));

console.log(await fal.queue.result(process.env.FAL_MODEL, {
  requestId: queued.request_id,
}));
