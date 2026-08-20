package image

import (
	"bytes"
	"errors"
	"mime/multipart"
	"testing"
)

func TestParseOpenAIJSONPricingSelector(t *testing.T) {
	selector, err := ParseOpenAIJSONPricingSelector([]byte(`{"prompt":"secret","model":"gpt-image-1","n":2,"size":"1024x1024","quality":"high"}`))
	if err != nil {
		t.Fatal(err)
	}
	if selector != (PricingSelector{Model: "gpt-image-1", Quantity: 2, Size: "1024x1024", Quality: "high"}) {
		t.Fatalf("selector=%+v", selector)
	}
	defaults, err := ParseOpenAIJSONPricingSelector([]byte(`{"model":"grok-imagine-image-quality"}`))
	if err != nil || defaults.Quantity != 1 || defaults.Size != "default" || defaults.Quality != "default" {
		t.Fatalf("defaults=%+v error=%v", defaults, err)
	}
}

func TestParseOpenAIJSONPricingSelectorRejectsAmbiguity(t *testing.T) {
	for _, body := range []string{
		`{"model":"one","model":"two"}`,
		`{"model":"one","n":0}`,
		`{"model":"one","n":11}`,
		`{"model":"one","n":1.5}`,
		`{"model":"../one"}`,
		`{"model":"one"} trailing`,
	} {
		if _, err := ParseOpenAIJSONPricingSelector([]byte(body)); !errors.Is(err, ErrInvalidPricingSelector) {
			t.Fatalf("body=%q error=%v", body, err)
		}
	}
}

func TestGeminiPricingSelectorIsExplicitlyUnavailable(t *testing.T) {
	if _, err := ParseJSONPricingSelector("gemini", []byte(`{"model":"gemini-image"}`)); !errors.Is(err, ErrPricingUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

func TestParseOpenAIMultipartPricingSelectorDiscardsFiles(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "gpt-image-1")
	_ = writer.WriteField("size", "1024x1024")
	_ = writer.WriteField("quality", "standard")
	file, _ := writer.CreateFormFile("image", "input.png")
	_, _ = file.Write(bytes.Repeat([]byte{7}, 4096))
	_ = writer.Close()
	selector, err := ParseOpenAIMultipartPricingSelector(&body, writer.Boundary())
	if err != nil {
		t.Fatal(err)
	}
	if selector.Model != "gpt-image-1" || selector.Quantity != 1 || selector.Size != "1024x1024" || selector.Quality != "standard" {
		t.Fatalf("selector=%+v", selector)
	}
}
