package async

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

const CallbackTimestampHeader = "X-Native-Gateway-Plugin-Timestamp"
const CallbackDeliveryHeader = "X-Native-Gateway-Plugin-Delivery"
const CallbackSignatureHeader = "X-Native-Gateway-Plugin-Signature"

// CallbackSigningMessage returns the versioned unambiguous message used by
// sidecars and Gateway to authenticate an exact callback body.
func CallbackSigningMessage(timestamp int64, deliveryID string, body []byte) ([]byte, error) {
	if timestamp < 1 || !deliveryPattern.MatchString(deliveryID) || len(body) < 2 || len(body) > 128<<20 {
		return nil, ErrInvalid
	}
	timestampText := strconv.FormatInt(timestamp, 10)
	prefix := "NGPLUGIN-CALLBACK-V1 " + strconv.Itoa(len(timestampText)) + " " + timestampText + " " + strconv.Itoa(len(deliveryID)) + " " + deliveryID + " " + strconv.Itoa(len(body)) + " "
	return append([]byte(prefix), body...), nil
}

// SignCallback returns a lowercase hex HMAC-SHA256 for the callback message.
// The callback key is purpose-specific and must not be the sidecar bearer key.
func SignCallback(secret []byte, timestamp int64, deliveryID string, body []byte) (string, error) {
	if len(secret) != 32 {
		return "", ErrInvalid
	}
	message, err := CallbackSigningMessage(timestamp, deliveryID, body)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(message)
	return "v1=" + hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifyCallbackSignature compares a v1 signature in constant time.
func VerifyCallbackSignature(secret []byte, timestamp int64, deliveryID string, body []byte, signature string) error {
	if len(signature) != len("v1=")+sha256.Size*2 || signature[:3] != "v1=" {
		return ErrInvalid
	}
	provided, err := hex.DecodeString(signature[3:])
	if err != nil {
		return ErrInvalid
	}
	expected, err := SignCallback(secret, timestamp, deliveryID, body)
	if err != nil {
		return ErrInvalid
	}
	expectedBytes, _ := hex.DecodeString(expected[3:])
	if !hmac.Equal(provided, expectedBytes) {
		return ErrInvalid
	}
	return nil
}
