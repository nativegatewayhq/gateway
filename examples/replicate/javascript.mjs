import Replicate from "replicate";

const replicate = new Replicate({
  auth: process.env.GATEWAY_API_KEY,
  baseUrl: process.env.GATEWAY_URL,
});

const prediction = await replicate.predictions.create({
  version: process.env.REPLICATE_MODEL_VERSION,
  input: { prompt: "a cat astronaut" },
});

console.log(await replicate.predictions.get(prediction.id));
