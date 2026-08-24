package video

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
	"io"
)

var ErrInvalid = errors.New("invalid plugin video envelope")

func DecodeSubmitRequest(reader io.Reader, maximum int64, expected Expectation) (SubmitRequest, error) {
	var value SubmitRequest
	if decode(reader, maximum, &value) != nil || ValidateSubmitRequest(value, expected) != nil {
		return SubmitRequest{}, ErrInvalid
	}
	return value, nil
}
func DecodeControlRequest(reader io.Reader, maximum int64) (ControlRequest, error) {
	var value ControlRequest
	if decode(reader, maximum, &value) != nil || ValidateControlRequest(value) != nil {
		return ControlRequest{}, ErrInvalid
	}
	return value, nil
}
func DecodeSubmitResponse(reader io.Reader, maximum int64, expected Expectation) (SubmitResponse, error) {
	var value SubmitResponse
	if decode(reader, maximum, &value) != nil || ValidateSubmitResponse(value, expected) != nil {
		return SubmitResponse{}, ErrInvalid
	}
	return value, nil
}
func DecodeObservationResponse(reader io.Reader, maximum int64, expected Expectation) (ObservationResponse, error) {
	var value ObservationResponse
	if decode(reader, maximum, &value) != nil || ValidateObservationResponse(value, expected) != nil {
		return ObservationResponse{}, ErrInvalid
	}
	return value, nil
}
func DecodeCallback(reader io.Reader, maximum int64, expected Expectation) (Callback, error) {
	var value Callback
	if decode(reader, maximum, &value) != nil || ValidateCallback(value, expected) != nil {
		return Callback{}, ErrInvalid
	}
	return value, nil
}
func CanonicalSubmitRequest(value SubmitRequest, expected Expectation) ([]byte, error) {
	if ValidateSubmitRequest(value, expected) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}
func CanonicalControlRequest(value ControlRequest) ([]byte, error) {
	if ValidateControlRequest(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}
func CanonicalSubmitResponse(value SubmitResponse, expected Expectation) ([]byte, error) {
	if ValidateSubmitResponse(value, expected) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}
func CanonicalObservationResponse(value ObservationResponse, expected Expectation) ([]byte, error) {
	if ValidateObservationResponse(value, expected) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}
func CanonicalCallback(value Callback, expected Expectation) ([]byte, error) {
	if ValidateCallback(value, expected) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}
func decode(reader io.Reader, maximum int64, target any) error {
	if reader == nil || maximum < 1 || maximum > 128<<20 {
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
