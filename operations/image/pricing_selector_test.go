package image

import (
	"bytes"
	"errors"
	"io"
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

func TestRewriteJSONModelPreservesOtherWireValues(t *testing.T) {
	body := []byte(" {\n \"prompt\" : \"secret\", \"number\":1.00, \"model\" : \"logical-model\", \"unknown\" : {\"x\":true} } ")
	rewritten, err := RewriteJSONModel(body, "provider-model")
	if err != nil {
		t.Fatal(err)
	}
	want := " {\n \"prompt\" : \"secret\", \"number\":1.00, \"model\" : \"provider-model\", \"unknown\" : {\"x\":true} } "
	if string(rewritten) != want {
		t.Fatalf("rewrite=%q", rewritten)
	}
}

func TestGeminiPricingSelectorIsExplicitlyUnavailable(t *testing.T) {
	if _, err := ParseJSONPricingSelector("gemini", []byte(`{"model":"gemini-image"}`)); !errors.Is(err, ErrPricingUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

func TestParseGeminiJSONPricingSelector(t *testing.T) {
	selector, err := ParseGeminiJSONPricingSelector("gemini-image", []byte(`{"contents":[],"generationConfig":{"candidateCount":1,"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}}}`))
	if err != nil || selector.Model != "gemini-image" || selector.Quantity != 1 || selector.Size != "16:9" || selector.Quality != "2K" {
		t.Fatalf("selector=%+v error=%v", selector, err)
	}
	defaults, err := ParseGeminiJSONPricingSelector("gemini-image", []byte(`{"contents":[]}`))
	if err != nil || defaults.Size != "default" || defaults.Quality != "default" {
		t.Fatalf("defaults=%+v error=%v", defaults, err)
	}
	for _, body := range []string{`[]`, `{"generationConfig":null}`, `{"generationConfig":{"imageConfig":[]}}`, `{"generationConfig":{"imageConfig":{"aspectRatio":1}}}`, `{"generationConfig":{"imageConfig":{"imageSize":""}}}`, `{"contents":[],"contents":[]}`, `{"generationConfig":{"imageConfig":{},"imageConfig":{}}}`} {
		if _, err := ParseGeminiJSONPricingSelector("gemini-image", []byte(body)); !errors.Is(err, ErrInvalidPricingSelector) {
			t.Errorf("body %s error=%v", body, err)
		}
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

func TestRewriteMultipartModelPreservesFileAndMetadata(t *testing.T) {
	var source bytes.Buffer
	writer := multipart.NewWriter(&source)
	_ = writer.WriteField("model", "logical-model")
	file, _ := writer.CreateFormFile("image", "input.png")
	payload := bytes.Repeat([]byte{7}, 4096)
	_, _ = file.Write(payload)
	_ = writer.Close()
	var destination bytes.Buffer
	written, err := RewriteMultipartModel(&source, writer.Boundary(), "provider-model", &destination)
	if err != nil || written != int64(destination.Len()) {
		t.Fatalf("written=%d len=%d error=%v", written, destination.Len(), err)
	}
	reader := multipart.NewReader(&destination, writer.Boundary())
	modelPart, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	model, _ := io.ReadAll(modelPart)
	imagePart, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	image, _ := io.ReadAll(imagePart)
	if string(model) != "provider-model" || imagePart.FormName() != "image" || imagePart.FileName() != "input.png" || !bytes.Equal(image, payload) {
		t.Fatalf("model=%q form=%q file=%q image=%d", model, imagePart.FormName(), imagePart.FileName(), len(image))
	}
}
