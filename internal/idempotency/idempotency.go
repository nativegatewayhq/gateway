// Package idempotency validates client keys and fingerprints exact requests.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"net/http"
)

const HeaderName = "Idempotency-Key"

var ErrInvalidKey = errors.New("invalid idempotency key")

func Extract(header http.Header) (string, error) {
	values := header.Values(HeaderName)
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || !Valid(values[0]) {
		return "", ErrInvalidKey
	}
	return values[0], nil
}

func Valid(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func Fingerprint(protocol, operation, model, channelID, mediaType string, body []byte) [32]byte {
	result, _ := FingerprintReader(protocol, operation, model, channelID, mediaType, bytes.NewReader(body), int64(len(body)))
	return result
}

func FingerprintReader(protocol, operation, model, channelID, mediaType string, body io.Reader, bodyLength int64) ([32]byte, error) {
	digest := sha256.New()
	writeField(digest, []byte("nativegateway-idempotency-v1"))
	for _, value := range []string{protocol, operation, model, channelID, mediaType} {
		writeField(digest, []byte(value))
	}
	if body == nil || bodyLength < 0 {
		return [32]byte{}, errors.New("invalid fingerprint body")
	}
	writeLength(digest, uint64(bodyLength))
	written, err := io.CopyN(digest, body, bodyLength)
	if err != nil || written != bodyLength {
		return [32]byte{}, errors.New("read fingerprint body")
	}
	var extra [1]byte
	if count, err := body.Read(extra[:]); err != io.EOF || count != 0 {
		return [32]byte{}, errors.New("fingerprint body length mismatch")
	}
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func writeField(destination hash.Hash, value []byte) {
	writeLength(destination, uint64(len(value)))
	_, _ = destination.Write(value)
}

func writeLength(destination hash.Hash, value uint64) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], value)
	_, _ = destination.Write(length[:])
}
