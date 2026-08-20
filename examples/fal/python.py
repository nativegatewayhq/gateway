import os
from urllib.parse import urlparse


gateway = urlparse(os.environ["GATEWAY_URL"])
if gateway.scheme != "https" or not gateway.netloc or gateway.path not in ("", "/"):
    raise ValueError("GATEWAY_URL must be an HTTPS origin")

# fal_client resolves this value while importing its auth/client modules.
os.environ["FAL_QUEUE_RUN_HOST"] = gateway.netloc
os.environ["FAL_KEY"] = os.environ["GATEWAY_API_KEY"]

import fal_client  # noqa: E402


handle = fal_client.submit(
    os.environ["FAL_MODEL"],
    arguments={"prompt": "a cat astronaut"},
)

print(handle.status(with_logs=False))
print(handle.get())
