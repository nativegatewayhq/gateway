import os

from replicate.client import Client


client = Client(
    bearer_token=os.environ["GATEWAY_API_KEY"],
    base_url=os.environ["GATEWAY_URL"],
)

prediction = client.predictions.create(
    version=os.environ["REPLICATE_MODEL_VERSION"],
    input={"prompt": "a cat astronaut"},
)

print(client.predictions.get(prediction.id))
