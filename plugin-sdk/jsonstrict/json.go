// Package jsonstrict detects ambiguous JSON before typed decoding.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var ErrInvalid = errors.New("invalid or duplicate JSON")

// Validate rejects malformed JSON, duplicate object member names at any
// nesting depth, and trailing values. It does not impose a schema.
func Validate(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := scanValue(decoder); err != nil {
		return ErrInvalid
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ErrInvalid
	}
	return nil
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err = decoder.Token()
			if err != nil {
				return err
			}
			key, valid := token.(string)
			if !valid || seen[key] {
				return ErrInvalid
			}
			seen[key] = true
			if err = scanValue(decoder); err != nil {
				return err
			}
		}
		token, err = decoder.Token()
		if err != nil || token != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for decoder.More() {
			if err = scanValue(decoder); err != nil {
				return err
			}
		}
		token, err = decoder.Token()
		if err != nil || token != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
