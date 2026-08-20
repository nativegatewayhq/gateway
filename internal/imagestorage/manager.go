package imagestorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type TransformInput struct {
	Protocol, Provider, ChannelID, RequestID, ChargeID string
	Body                                               []byte
}

type Manager struct {
	config    Config
	collector *Collector
	objects   ObjectStore
	assets    AssetRepository
}

func (manager *Manager) MaximumResponseBytes() int64 {
	// Base64 expands by 4/3. The bounded allowance covers native JSON fields
	// surrounding the encoded image results without making the parser unbounded.
	return manager.config.MaximumTotalBytes*4/3 + 1<<20
}

func NewManager(config Config, collector *Collector, objects ObjectStore, assets AssetRepository) (*Manager, error) {
	if err := config.Validate(); err != nil || config.Mode != Managed || collector == nil || objects == nil || assets == nil {
		return nil, ErrInvalidConfig
	}
	return &Manager{config: config, collector: collector, objects: objects, assets: assets}, nil
}

func (manager *Manager) Transform(ctx context.Context, input TransformInput) ([]byte, error) {
	if len(input.Body) == 0 || input.RequestID == "" || input.ChannelID == "" {
		return nil, ErrInvalidContent
	}
	switch input.Protocol {
	case "openai":
		return manager.transformOpenAI(ctx, input)
	case "gemini":
		return manager.transformGemini(ctx, input)
	default:
		return nil, ErrInvalidContent
	}
}

func (manager *Manager) transformOpenAI(ctx context.Context, input TransformInput) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(input.Body, &envelope); err != nil {
		return nil, ErrInvalidContent
	}
	var data []map[string]json.RawMessage
	if raw, exists := envelope["data"]; !exists || json.Unmarshal(raw, &data) != nil {
		return input.Body, nil
	}
	if len(data) > manager.config.MaximumImages {
		return nil, ErrTooLarge
	}
	var total int64
	for index, item := range data {
		var collected *Collected
		var err error
		if raw, exists := item["b64_json"]; exists {
			var encoded string
			if json.Unmarshal(raw, &encoded) != nil {
				return nil, ErrInvalidContent
			}
			collected, err = manager.collector.DecodeBase64(encoded, "")
		} else if raw, exists := item["url"]; exists {
			var assetURL string
			if json.Unmarshal(raw, &assetURL) != nil {
				return nil, ErrInvalidContent
			}
			collected, err = manager.collector.Fetch(ctx, input.Provider, assetURL)
		} else {
			continue
		}
		if err != nil {
			return nil, err
		}
		total += collected.Size
		if total > manager.config.MaximumTotalBytes {
			_ = collected.Close()
			return nil, ErrTooLarge
		}
		storedURL, err := manager.persist(ctx, input, index, collected)
		_ = collected.Close()
		if err != nil {
			return nil, err
		}
		encodedURL, _ := json.Marshal(storedURL)
		item["url"] = encodedURL
		delete(item, "b64_json")
	}
	encodedData, err := json.Marshal(data)
	if err != nil {
		return nil, ErrInvalidContent
	}
	envelope["data"] = encodedData
	result, err := json.Marshal(envelope)
	if err != nil {
		return nil, ErrInvalidContent
	}
	return result, nil
}

func (manager *Manager) transformGemini(ctx context.Context, input TransformInput) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(input.Body, &envelope); err != nil {
		return nil, ErrInvalidContent
	}
	var candidates []map[string]json.RawMessage
	if raw, exists := envelope["candidates"]; !exists || json.Unmarshal(raw, &candidates) != nil {
		return input.Body, nil
	}
	index := 0
	var total int64
	for _, candidate := range candidates {
		var content map[string]json.RawMessage
		if json.Unmarshal(candidate["content"], &content) != nil {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(content["parts"], &parts) != nil {
			continue
		}
		for _, part := range parts {
			inlineKey, fileKey := "inlineData", "fileData"
			raw, exists := part[inlineKey]
			if !exists {
				inlineKey, fileKey = "inline_data", "file_data"
				raw, exists = part[inlineKey]
			}
			if !exists {
				continue
			}
			if index >= manager.config.MaximumImages {
				return nil, ErrTooLarge
			}
			var inline map[string]json.RawMessage
			if json.Unmarshal(raw, &inline) != nil {
				return nil, ErrInvalidContent
			}
			mimeKey := "mimeType"
			if _, exists := inline[mimeKey]; !exists {
				mimeKey = "mime_type"
			}
			var contentType, encoded string
			if json.Unmarshal(inline[mimeKey], &contentType) != nil || json.Unmarshal(inline["data"], &encoded) != nil || !strings.HasPrefix(contentType, "image/") {
				continue
			}
			collected, err := manager.collector.DecodeBase64(encoded, contentType)
			if err != nil {
				return nil, err
			}
			total += collected.Size
			if total > manager.config.MaximumTotalBytes {
				_ = collected.Close()
				return nil, ErrTooLarge
			}
			storedURL, err := manager.persist(ctx, input, index, collected)
			_ = collected.Close()
			if err != nil {
				return nil, err
			}
			file := map[string]string{mimeKey: contentType}
			if fileKey == "fileData" {
				file["fileUri"] = storedURL
			} else {
				file["file_uri"] = storedURL
			}
			part[fileKey], _ = json.Marshal(file)
			delete(part, inlineKey)
			index++
		}
		content["parts"], _ = json.Marshal(parts)
		candidate["content"], _ = json.Marshal(content)
	}
	envelope["candidates"], _ = json.Marshal(candidates)
	result, err := json.Marshal(envelope)
	if err != nil {
		return nil, ErrInvalidContent
	}
	return result, nil
}

func (manager *Manager) persist(ctx context.Context, input TransformInput, index int, collected *Collected) (string, error) {
	identity := input.ChargeID
	if identity == "" {
		identity = input.RequestID
	}
	key, err := ObjectKey(input.Protocol, identity, index, collected.SHA256, collected.Extension)
	if err != nil {
		return "", err
	}
	id, err := AssetID(input.Protocol, input.RequestID, index)
	if err != nil {
		return "", err
	}
	asset, err := manager.assets.Begin(ctx, Asset{ID: id, ChargeID: input.ChargeID, RequestID: input.RequestID, Protocol: input.Protocol, Provider: input.Provider, ChannelID: input.ChannelID, ResultIndex: index, ObjectKey: key, ContentType: collected.ContentType, ByteLength: collected.Size, SHA256: collected.SHA256})
	if err != nil {
		return "", err
	}
	if asset.State == Available {
		return publicURL(manager.config.CDNBaseURL, asset.ObjectKey)
	}
	if asset.State != Pending {
		return "", ErrUnavailable
	}
	ownerBytes := make([]byte, 16)
	if _, err := rand.Read(ownerBytes); err != nil {
		return "", ErrUnavailable
	}
	owner := hex.EncodeToString(ownerBytes)
	lease := manager.config.UploadTimeout + 5*time.Second
	if lease > 10*time.Minute {
		lease = 10 * time.Minute
	}
	for {
		claimedAsset, claimed, claimErr := manager.assets.Claim(ctx, id, owner, lease)
		if claimErr != nil {
			return "", claimErr
		}
		if claimed {
			break
		}
		if claimedAsset.State == Available {
			return publicURL(manager.config.CDNBaseURL, claimedAsset.ObjectKey)
		}
		select {
		case <-ctx.Done():
			return "", ErrUnavailable
		case <-time.After(25 * time.Millisecond):
		}
	}
	if _, err := collected.File.Seek(0, io.SeekStart); err != nil {
		manager.release(id, owner, "persistence_failed")
		return "", ErrUnavailable
	}
	stored, err := manager.objects.Put(ctx, Object{Key: key, ContentType: collected.ContentType, Size: collected.Size, SHA256: collected.SHA256}, collected.File)
	if err != nil {
		manager.release(id, owner, "upload_failed")
		return "", ErrUnavailable
	}
	finishContext, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	_, finishErr := manager.assets.MarkAvailable(finishContext, id, owner)
	finishCancel()
	if finishErr != nil {
		manager.release(id, owner, "persistence_failed")
		return "", fmt.Errorf("asset persistence: %w", ErrUnavailable)
	}
	return stored.URL, nil
}

func (manager *Manager) release(id, owner, category string) {
	releaseContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = manager.assets.Release(releaseContext, id, owner, category)
}

func FailureCategory(err error) string {
	switch {
	case errors.Is(err, ErrFetchRejected):
		return "fetch_rejected"
	case errors.Is(err, ErrFetchFailed):
		return "fetch_failed"
	case errors.Is(err, ErrInvalidContent), errors.Is(err, ErrTooLarge):
		return "invalid_content"
	default:
		return "storage_unavailable"
	}
}
