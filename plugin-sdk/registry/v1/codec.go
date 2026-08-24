package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
)

var ErrInvalid = errors.New("invalid adapter registry document")

func DecodeEnvelope(reader io.Reader, maximum int64) (Envelope, error) {
	var value Envelope
	if decode(reader, maximum, &value) != nil || validateEnvelope(value) != nil {
		return Envelope{}, ErrInvalid
	}
	return value, nil
}

func DecodeIndex(reader io.Reader, maximum int64) (Index, error) {
	var value Index
	if decode(reader, maximum, &value) != nil || validateIndex(value) != nil {
		return Index{}, ErrInvalid
	}
	return value, nil
}

func DecodeStatement(reader io.Reader, maximum int64) (Statement, error) {
	var value Statement
	if decode(reader, maximum, &value) != nil || validateStatement(value) != nil {
		return Statement{}, ErrInvalid
	}
	return value, nil
}

func DecodeTrustPolicy(reader io.Reader, maximum int64) (TrustPolicy, error) {
	var value TrustPolicy
	if decode(reader, maximum, &value) != nil || validateTrustPolicy(value) != nil {
		return TrustPolicy{}, ErrInvalid
	}
	return value, nil
}

func CanonicalIndex(value Index) ([]byte, error) {
	if validateIndex(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func CanonicalStatement(value Statement) ([]byte, error) {
	if validateStatement(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func CanonicalTrustPolicy(value TrustPolicy) ([]byte, error) {
	if validateTrustPolicy(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func CanonicalEnvelope(value Envelope) ([]byte, error) {
	if validateEnvelope(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func Digest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func decode(reader io.Reader, maximum int64, target any) error {
	if reader == nil || maximum < 1 || maximum > MaximumEnvelopeBytes {
		return ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maximum || jsonstrict.Validate(body) != nil {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ErrInvalid
	}
	return nil
}
