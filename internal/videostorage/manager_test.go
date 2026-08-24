package videostorage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type collectorFake struct{ calls int }

func (fake *collectorFake) Fetch(_ context.Context, raw string) (*Collected, error) {
	fake.calls++
	if strings.Contains(raw, "private") {
		return nil, ErrFetchRejected
	}
	file, _ := os.CreateTemp("", "video-test-*")
	content := []byte("bounded-video")
	_, _ = file.Write(content)
	_, _ = file.Seek(0, io.SeekStart)
	return &Collected{File: file, Size: int64(len(content)), SHA256: sha256.Sum256(content), ContentType: "video/mp4", Extension: "mp4"}, nil
}

type objectStoreFake struct{ puts int }

func (fake *objectStoreFake) Put(_ context.Context, object imagestorage.Object, reader io.Reader) (imagestorage.StoredObject, error) {
	fake.puts++
	read, _ := io.ReadAll(reader)
	if int64(len(read)) != object.Size {
		return imagestorage.StoredObject{}, ErrInvalid
	}
	return imagestorage.StoredObject{Key: object.Key, URL: "https://cdn.example/" + object.Key, ContentType: object.ContentType, Size: object.Size, SHA256: object.SHA256}, nil
}
func (*objectStoreFake) Ready(context.Context) error { return nil }

type assetRepositoryFake struct{ assets map[int]Asset }

func (fake *assetRepositoryFake) Get(_ context.Context, _ string, index int) (Asset, bool, error) {
	asset, ok := fake.assets[index]
	return asset, ok, nil
}
func (fake *assetRepositoryFake) Begin(_ context.Context, asset Asset) (Asset, error) {
	fake.assets[asset.ResultIndex] = asset
	return asset, nil
}
func (fake *assetRepositoryFake) Claim(_ context.Context, id, owner string, _ time.Duration) (Asset, bool, error) {
	for index, asset := range fake.assets {
		if asset.ID == id {
			asset.LeaseOwner = owner
			fake.assets[index] = asset
			return asset, true, nil
		}
	}
	return Asset{}, false, ErrInvalid
}
func (fake *assetRepositoryFake) MarkAvailable(_ context.Context, id, _ string) (Asset, error) {
	for index, asset := range fake.assets {
		if asset.ID == id {
			asset.State = "AVAILABLE"
			fake.assets[index] = asset
			return asset, nil
		}
	}
	return Asset{}, ErrInvalid
}
func (*assetRepositoryFake) Release(context.Context, string, string) error { return nil }
func (*assetRepositoryFake) Ready(context.Context) error                   { return nil }

func TestManagerPersistsAndReplaysManagedNativeOutput(t *testing.T) {
	config := testConfig()
	collector := &collectorFake{}
	objects := &objectStoreFake{}
	assets := &assetRepositoryFake{assets: map[int]Asset{}}
	manager, err := NewManager(config, collector, objects, assets)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"job_0123456789abcdef0123456789abcdef","status":"SUCCEEDED","output":["https://runway.example/output.mp4"],"cost":{"credits":5}}`)
	job := joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", ChargeID: "charge_0123456789abcdef0123456789abcdef", Protocol: "runway", Provider: "runway", ChannelID: "channel_00000000000000000000000000000007", Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Body: body}}
	result, err := manager.Transform(context.Background(), job)
	if err != nil || strings.Contains(string(result.Body), "runway.example") || !strings.Contains(string(result.Body), "https://cdn.example/videos/runway/") {
		t.Fatalf("result=%s err=%v", result.Body, err)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(result.Body, &envelope) != nil || string(envelope["cost"]) != `{"credits":5}` {
		t.Fatalf("native fields=%s", result.Body)
	}
	if _, err = manager.Transform(context.Background(), job); err != nil || collector.calls != 1 || objects.puts != 1 {
		t.Fatalf("replay calls=%d/%d err=%v", collector.calls, objects.puts, err)
	}
}

func TestManagerAcceptsPluginBackedRunwayOutput(t *testing.T) {
	collector := &collectorFake{}
	objects := &objectStoreFake{}
	assets := &assetRepositoryFake{assets: map[int]Asset{}}
	manager, err := NewManager(testConfig(), collector, objects, assets)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"SUCCEEDED","output":["https://runway.example/output.mp4"]}`)
	job := joboperation.Job{ID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Protocol: "runway", Provider: "plugin", ChannelID: "channel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Body: body}}
	result, err := manager.Transform(context.Background(), job)
	if err != nil || !strings.Contains(string(result.Body), "https://cdn.example/videos/plugin/") || assets.assets[0].Provider != "plugin" {
		t.Fatalf("result=%s asset=%#v err=%v", result.Body, assets.assets[0], err)
	}
}

func testConfig() Config {
	return Config{Mode: Managed, Endpoint: "http://127.0.0.1:9000", Region: "auto", Bucket: "videos", AccessKeyID: "access", SecretAccessKey: "secret", CDNBaseURL: "https://cdn.example", MaximumVideos: 4, MaximumConcurrentDownloads: 2, MaximumVideoBytes: 1024, MaximumTotalBytes: 4096, FetchTimeout: time.Second, UploadTimeout: time.Second, FetchOrigins: map[string][]string{"runway": {"https://runway.example"}}}
}
