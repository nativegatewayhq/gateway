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

func ParseGeminiJSONPricingSelector(model string, body []byte) (PricingSelector, error) {
	if !validModelID(model) {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	root, err := parseUniqueObject(body)
	if err != nil {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	selector := PricingSelector{Model: model, Quantity: 1, Size: "default", Quality: "default"}
	configuration, exists := root["generationConfig"]
	if !exists {
		return selector, nil
	}
	generation, err := parseUniqueObject(configuration)
	if err != nil {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	imageRaw, exists := generation["imageConfig"]
	if !exists {
		return selector, nil
	}
	imageConfig, err := parseUniqueObject(imageRaw)
	if err != nil {
		return PricingSelector{}, ErrInvalidPricingSelector
	}
	if raw, exists := imageConfig["aspectRatio"]; exists {
		if json.Unmarshal(raw, &selector.Size) != nil || !validDimension(selector.Size) {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
	}
	if raw, exists := imageConfig["imageSize"]; exists {
		if json.Unmarshal(raw, &selector.Quality) != nil || !validDimension(selector.Quality) {
			return PricingSelector{}, ErrInvalidPricingSelector
		}
	}
	return selector, nil
}

func parseUniqueObject(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInvalidPricingSelector
	}
	values := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalidPricingSelector
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrInvalidPricingSelector
		}
		if _, duplicate := values[key]; duplicate {
			return nil, ErrInvalidPricingSelector
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, ErrInvalidPricingSelector
		}
		values[key] = raw
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, ErrInvalidPricingSelector
	}
	return values, nil
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

func RewriteJSONModel(body []byte, providerModel string) ([]byte, error) {
	if !validModelID(providerModel) {
		return nil, ErrInvalidPricingSelector
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInvalidPricingSelector
	}
	start, end, count := 0, 0, 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalidPricingSelector
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrInvalidPricingSelector
		}
		before := int(decoder.InputOffset())
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, ErrInvalidPricingSelector
		}
		if key != "model" {
			continue
		}
		var current string
		if json.Unmarshal(raw, &current) != nil {
			return nil, ErrInvalidPricingSelector
		}
		count++
		start, end = before, int(decoder.InputOffset())
		for start < end && (body[start] == ' ' || body[start] == '\t' || body[start] == '\r' || body[start] == '\n' || body[start] == ':') {
			start++
		}
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF || count != 1 || start >= end {
		return nil, ErrInvalidPricingSelector
	}
	replacement, _ := json.Marshal(providerModel)
	result := make([]byte, 0, len(body)-end+start+len(replacement))
	result = append(result, body[:start]...)
	result = append(result, replacement...)
	result = append(result, body[end:]...)
	return result, nil
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

func RewriteMultipartModel(source io.Reader, boundary, providerModel string, destination io.Writer) (int64, error) {
	if source == nil || destination == nil || boundary == "" || len(boundary) > 200 || !validModelID(providerModel) {
		return 0, ErrInvalidPricingSelector
	}
	counter := &countingWriter{destination: destination}
	writer := multipart.NewWriter(counter)
	if err := writer.SetBoundary(boundary); err != nil {
		return 0, ErrInvalidPricingSelector
	}
	reader := multipart.NewReader(source, boundary)
	modelCount := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, ErrInvalidPricingSelector
		}
		output, err := writer.CreatePart(part.Header)
		if err != nil {
			_ = part.Close()
			return 0, ErrInvalidPricingSelector
		}
		if part.FormName() == "model" {
			modelCount++
			_, err = io.WriteString(output, providerModel)
			if _, copyErr := io.Copy(io.Discard, part); err == nil {
				err = copyErr
			}
		} else {
			_, err = io.Copy(output, part)
		}
		_ = part.Close()
		if err != nil {
			return 0, ErrInvalidPricingSelector
		}
	}
	if modelCount != 1 || writer.Close() != nil {
		return 0, ErrInvalidPricingSelector
	}
	return counter.written, nil
}

type countingWriter struct {
	destination io.Writer
	written     int64
}

func (writer *countingWriter) Write(value []byte) (int, error) {
	count, err := writer.destination.Write(value)
	writer.written += int64(count)
	return count, err
}

func validSelector(selector PricingSelector) bool {
	return validModelID(selector.Model) && selector.Quantity >= 1 && selector.Quantity <= 10 && validDimension(selector.Size) && validDimension(selector.Quality)
}

func validDimension(value string) bool {
	return value != "" && len(value) <= 80 && strings.TrimSpace(value) == value
}
