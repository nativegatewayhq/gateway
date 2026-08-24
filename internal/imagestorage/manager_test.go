package imagestorage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

type objectStoreFake struct {
	puts []Object
}

func (fake *objectStoreFake) Put(_ context.Context, object Object, body io.Reader) (StoredObject, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return StoredObject{}, err
	}
	fake.puts = append(fake.puts, object)
	return StoredObject{Key: object.Key, URL: "https://cdn.example.test/assets/" + object.Key, ContentType: object.ContentType, Size: object.Size, SHA256: object.SHA256}, nil
}
func (*objectStoreFake) Ready(context.Context) error { return nil }

type assetRepositoryFake struct {
	assets map[string]Asset
}

func (fake *assetRepositoryFake) Begin(_ context.Context, asset Asset) (Asset, error) {
	if existing, ok := fake.assets[asset.ID]; ok {
		return existing, nil
	}
	asset.State = Pending
	fake.assets[asset.ID] = asset
	return asset, nil
}
func (fake *assetRepositoryFake) Claim(_ context.Context, id, owner string, lease time.Duration) (Asset, bool, error) {
	asset := fake.assets[id]
	if asset.State != Pending || asset.LeaseOwner != "" {
		return asset, false, nil
	}
	until := time.Now().Add(lease)
	asset.LeaseOwner, asset.LeaseUntil = owner, &until
	fake.assets[id] = asset
	return asset, true, nil
}
func (fake *assetRepositoryFake) Get(_ context.Context, id string) (Asset, error) {
	return fake.assets[id], nil
}
func (fake *assetRepositoryFake) MarkAvailable(_ context.Context, id, owner string) (Asset, error) {
	asset := fake.assets[id]
	if asset.LeaseOwner != owner {
		return Asset{}, ErrInvalidObject
	}
	asset.State = Available
	asset.LeaseOwner, asset.LeaseUntil = "", nil
	fake.assets[id] = asset
	return asset, nil
}
func (fake *assetRepositoryFake) Release(_ context.Context, id, owner, category string) (Asset, error) {
	asset := fake.assets[id]
	if asset.LeaseOwner != owner {
		return Asset{}, ErrInvalidObject
	}
	asset.FailureCategory, asset.LeaseOwner, asset.LeaseUntil = category, "", nil
	fake.assets[id] = asset
	return asset, nil
}

func newManagerForTest(t *testing.T) (*Manager, *objectStoreFake) {
	t.Helper()
	config := collectorTestConfig(t)
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatal(err)
	}
	objects := &objectStoreFake{}
	manager, err := NewManager(config, collector, objects, &assetRepositoryFake{assets: map[string]Asset{}})
	if err != nil {
		t.Fatal(err)
	}
	return manager, objects
}

func testPNGBase64() string {
	return base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 520)...))
}

func TestManagerTransformsOpenAIBase64AndPreservesExtensions(t *testing.T) {
	manager, objects := newManagerForTest(t)
	body := `{"created":123,"extension":{"nested":true},"data":[{"b64_json":"` + testPNGBase64() + `","revised_prompt":"kept"}]}`
	result, err := manager.Transform(context.Background(), TransformInput{Protocol: "openai", Provider: "openai", ChannelID: "channel_00000000000000000000000000000001", RequestID: "request_abc", ChargeID: "charge_abc", Body: []byte(body)})
	if err != nil || len(objects.puts) != 1 {
		t.Fatalf("result=%s puts=%d err=%v", result, len(objects.puts), err)
	}
	var decoded map[string]any
	if json.Unmarshal(result, &decoded) != nil || decoded["extension"].(map[string]any)["nested"] != true {
		t.Fatalf("result=%s", result)
	}
	item := decoded["data"].([]any)[0].(map[string]any)
	if _, exists := item["b64_json"]; exists || !strings.HasPrefix(item["url"].(string), "https://cdn.example.test/assets/images/openai/charge_abc/") || item["revised_prompt"] != "kept" {
		t.Fatalf("item=%v", item)
	}
}

func TestManagerTransformsGeminiImageAndPreservesTextParts(t *testing.T) {
	manager, objects := newManagerForTest(t)
	body := `{"modelVersion":"kept","candidates":[{"content":{"role":"model","parts":[{"text":"hello"},{"inlineData":{"mimeType":"image/png","data":"` + testPNGBase64() + `"}}]}}]}`
	result, err := manager.Transform(context.Background(), TransformInput{Protocol: "gemini", Provider: "google", ChannelID: "channel_00000000000000000000000000000003", RequestID: "request_gemini", Body: []byte(body)})
	if err != nil || len(objects.puts) != 1 {
		t.Fatalf("result=%s puts=%d err=%v", result, len(objects.puts), err)
	}
	var decoded map[string]any
	if json.Unmarshal(result, &decoded) != nil || decoded["modelVersion"] != "kept" {
		t.Fatalf("result=%s", result)
	}
	parts := decoded["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "hello" || !strings.HasPrefix(parts[1].(map[string]any)["fileData"].(map[string]any)["fileUri"].(string), "https://cdn.example.test/") {
		t.Fatalf("parts=%v", parts)
	}
}

func TestManagerTransformsAsyncNativeBase64Results(t *testing.T) {
	for _, test := range []struct{ protocol, body string }{
		{"replicate", `{"id":"job","output":[{"mime_type":"image/png","base64":"` + testPNGBase64() + `"}]}`},
		{"fal", `{"request_id":"job","images":[{"content_type":"image/png","base64":"` + testPNGBase64() + `"}]}`},
	} {
		t.Run(test.protocol, func(t *testing.T) {
			manager, objects := newManagerForTest(t)
			result, err := manager.Transform(context.Background(), TransformInput{Protocol: test.protocol, Provider: "plugin", ChannelID: "channel_async", RequestID: "request_async", ChargeID: "charge_async", Body: []byte(test.body)})
			if err != nil || len(objects.puts) != 1 || !strings.Contains(string(result), "https://cdn.example.test/") || strings.Contains(string(result), "base64") {
				t.Fatalf("result=%s puts=%d err=%v", result, len(objects.puts), err)
			}
		})
	}
}

func TestManagerReplayUsesAvailableAssetWithoutSecondPut(t *testing.T) {
	manager, objects := newManagerForTest(t)
	input := TransformInput{Protocol: "openai", Provider: "openai", ChannelID: "channel_00000000000000000000000000000001", RequestID: "request_replay", Body: []byte(`{"data":[{"b64_json":"` + testPNGBase64() + `"}]}`)}
	first, err := manager.Transform(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Transform(context.Background(), input)
	if err != nil || string(first) != string(second) || len(objects.puts) != 1 {
		t.Fatalf("puts=%d first=%s second=%s err=%v", len(objects.puts), first, second, err)
	}
}
