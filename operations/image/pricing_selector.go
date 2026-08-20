package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"strconv"
	"strings"
)

var (
	ErrInvalidPricingSelector = errors.New("invalid image pricing selector")
	ErrPricingUnavailable     = errors.New("image pricing selector unavailable")
)

type PricingSelector struct {
	Model    string
	Quantity int64
	Size     string
	Quality  string
}

func ParseJSONPricingSelector(protocol string, body []byte) (PricingSelector, error) {
	if protocol != "openai" {
		return PricingSelector{}, ErrPricingUnavailable
	}
	return ParseOpenAIJSONPricingSelector(body)
}

func ParseOpenAIJSONPricingSelector(body []byte) (PricingSelector, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	selector := PricingSelector{Quantity: 1, Size: "default", Quality: "default"}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
		key, ok := keyToken.(string)
		if !ok {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
		switch key {
		case "model":
			if seen[key] || json.Unmarshal(raw, &selector.Model) != nil {
				return PricingSelector{}, ErrInvalidPricingSelector
			}
			seen[key] = true
		case "n":
			if seen[key] || json.Unmarshal(raw, &selector.Quantity) != nil {
				return PricingSelector{}, ErrInvalidPricingSelector
			}
			seen[key] = true
		case "size":
			if seen[key] || json.Unmarshal(raw, &selector.Size) != nil {
				return PricingSelector{}, ErrInvalidPricingSelector
			}
			seen[key] = true
		case "quality":
			if seen[key] || json.Unmarshal(raw, &selector.Quality) != nil {
				return PricingSelector{}, ErrInvalidPricingSelector
			}
			seen[key] = true
		}
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	if !seen["model"] || !validSelector(selector) {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	return selector, nil
}

func ParseOpenAIMultipartPricingSelector(reader io.Reader, boundary string) (PricingSelector, error) {
	if reader == nil || boundary == "" || len(boundary) > 200 {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	selector := PricingSelector{Quantity: 1, Size: "default", Quality: "default"}
	seen := map[string]bool{}
	parts := multipart.NewReader(reader, boundary)
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
		name := part.FormName()
		if name != "model" && name != "n" && name != "size" && name != "quality" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}
		if seen[name] {
			_ = part.Close()
			return PricingSelector{}, ErrInvalidPricingSelector
		}
		seen[name] = true
		value, err := io.ReadAll(io.LimitReader(part, 202))
		_ = part.Close()
		if err != nil || len(value) > 200 {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
		switch name {
		case "model":
			selector.Model = string(value)
		case "n":
			selector.Quantity, err = strconv.ParseInt(string(value), 10, 64)
			if err != nil {
				return PricingSelector{}, ErrInvalidPricingSelector
			}
		case "size":
			selector.Size = string(value)
		case "quality":
			selector.Quality = string(value)
		}
	}
	if !seen["model"] || !validSelector(selector) {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	return selector, nil
}

func validSelector(selector PricingSelector) bool {
	return validModelID(selector.Model) && selector.Quantity >= 1 && selector.Quantity <= 10 && validDimension(selector.Size) && validDimension(selector.Quality)
}

func validDimension(value string) bool {
	return value != "" && len(value) <= 80 && strings.TrimSpace(value) == value
}
