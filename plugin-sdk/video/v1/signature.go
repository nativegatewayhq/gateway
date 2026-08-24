package video

import asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"

const CallbackTimestampHeader = asyncv1.CallbackTimestampHeader
const CallbackDeliveryHeader = asyncv1.CallbackDeliveryHeader
const CallbackSignatureHeader = asyncv1.CallbackSignatureHeader

func CallbackSigningMessage(timestamp int64, delivery string, body []byte) ([]byte, error) {
	return asyncv1.CallbackSigningMessage(timestamp, delivery, body)
}
func SignCallback(secret []byte, timestamp int64, delivery string, body []byte) (string, error) {
	return asyncv1.SignCallback(secret, timestamp, delivery, body)
}
func VerifyCallbackSignature(secret []byte, timestamp int64, delivery string, body []byte, signature string) error {
	return asyncv1.VerifyCallbackSignature(secret, timestamp, delivery, body, signature)
}
