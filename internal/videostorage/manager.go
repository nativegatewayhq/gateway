package videostorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type Manager struct {
	config     Config
	collector  VideoCollector
	objects    imagestorage.ObjectStore
	assets     AssetRepository
	concurrent chan struct{}
}

type VideoCollector interface {
	Fetch(context.Context, string) (*Collected, error)
}
type AssetRepository interface {
	Get(context.Context, string, int) (Asset, bool, error)
	Begin(context.Context, Asset) (Asset, error)
	Claim(context.Context, string, string, time.Duration) (Asset, bool, error)
	MarkAvailable(context.Context, string, string) (Asset, error)
	Release(context.Context, string, string) error
	Ready(context.Context) error
}

func NewManager(config Config, collector VideoCollector, objects imagestorage.ObjectStore, assets AssetRepository) (*Manager, error) {
	if config.Validate() != nil || config.Mode != Managed || collector == nil || objects == nil || assets == nil {
		return nil, ErrInvalidConfig
	}
	return &Manager{config: config, collector: collector, objects: objects, assets: assets, concurrent: make(chan struct{}, config.MaximumConcurrentDownloads)}, nil
}

func (manager *Manager) Ready(ctx context.Context) error {
	if err := manager.assets.Ready(ctx); err != nil {
		return err
	}
	return manager.objects.Ready(ctx)
}

func (manager *Manager) Transform(ctx context.Context, job joboperation.Job) (joboperation.Snapshot, error) {
	if job.Protocol != "runway" || job.Provider != "runway" && job.Provider != "plugin" || job.Status != joboperation.Succeeded || len(job.Snapshot.Body) == 0 {
		return joboperation.Snapshot{}, ErrInvalid
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(job.Snapshot.Body, &envelope) != nil {
		return joboperation.Snapshot{}, ErrInvalid
	}
	var output []string
	if json.Unmarshal(envelope["output"], &output) != nil || len(output) < 1 || len(output) > manager.config.MaximumVideos {
		return joboperation.Snapshot{}, ErrInvalid
	}
	managed := make([]string, len(output))
	var total int64
	for index, rawURL := range output {
		stored, found, err := manager.assets.Get(ctx, job.ID, index)
		if err != nil {
			return joboperation.Snapshot{}, err
		}
		if found && stored.State == "AVAILABLE" {
			managed[index], err = cdnURL(manager.config.CDNBaseURL, stored.ObjectKey)
			if err != nil {
				return joboperation.Snapshot{}, err
			}
			total += stored.ByteLength
			continue
		}
		select {
		case manager.concurrent <- struct{}{}:
		case <-ctx.Done():
			return joboperation.Snapshot{}, ErrUnavailable
		}
		collected, fetchErr := manager.collector.Fetch(ctx, rawURL)
		<-manager.concurrent
		if fetchErr != nil {
			return joboperation.Snapshot{}, fetchErr
		}
		total += collected.Size
		if total > manager.config.MaximumTotalBytes {
			_ = collected.Close()
			return joboperation.Snapshot{}, ErrTooLarge
		}
		managed[index], err = manager.persist(ctx, job, index, collected)
		_ = collected.Close()
		if err != nil {
			return joboperation.Snapshot{}, err
		}
	}
	envelope["output"], _ = json.Marshal(managed)
	body, err := json.Marshal(envelope)
	if err != nil {
		return joboperation.Snapshot{}, ErrInvalid
	}
	return joboperation.Snapshot{Status: job.Snapshot.Status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}, nil
}

func (manager *Manager) persist(ctx context.Context, job joboperation.Job, index int, collected *Collected) (string, error) {
	id, _ := AssetID(job.ID, index)
	identity := job.ChargeID
	if identity == "" {
		identity = job.ID
	}
	key := fmt.Sprintf("videos/%s/%s/%03d-%s.%s", job.Provider, identity, index, hex.EncodeToString(collected.SHA256[:]), collected.Extension)
	asset, err := manager.assets.Begin(ctx, Asset{ID: id, JobID: job.ID, ChargeID: job.ChargeID, Provider: job.Provider, ChannelID: job.ChannelID, ResultIndex: index, ObjectKey: key, ContentType: collected.ContentType, ByteLength: collected.Size, SHA256: collected.SHA256})
	if err != nil {
		return "", err
	}
	if asset.State == "AVAILABLE" {
		return cdnURL(manager.config.CDNBaseURL, asset.ObjectKey)
	}
	ownerBytes := make([]byte, 16)
	if _, err = rand.Read(ownerBytes); err != nil {
		return "", ErrUnavailable
	}
	owner := hex.EncodeToString(ownerBytes)
	for {
		claimed, ok, claimErr := manager.assets.Claim(ctx, id, owner, manager.config.UploadTimeout)
		if claimErr != nil {
			return "", claimErr
		}
		if ok {
			asset = claimed
			break
		}
		if claimed.State == "AVAILABLE" {
			return cdnURL(manager.config.CDNBaseURL, claimed.ObjectKey)
		}
		select {
		case <-ctx.Done():
			return "", ErrUnavailable
		case <-time.After(25 * time.Millisecond):
		}
	}
	if _, err = collected.File.Seek(0, io.SeekStart); err != nil {
		manager.release(id, owner)
		return "", ErrUnavailable
	}
	stored, err := manager.objects.Put(ctx, imagestorage.Object{Key: key, ContentType: collected.ContentType, Size: collected.Size, SHA256: collected.SHA256}, collected.File)
	if err != nil {
		manager.release(id, owner)
		return "", ErrUnavailable
	}
	finalizeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err = manager.assets.MarkAvailable(finalizeContext, id, owner); err != nil {
		return "", err
	}
	return stored.URL, nil
}

func (manager *Manager) release(id, owner string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.assets.Release(ctx, id, owner)
}

func cdnURL(base, key string) (string, error) {
	base = strings.TrimSuffix(base, "/")
	value := base + "/" + strings.TrimPrefix(key, "/")
	if !validCDNURL(value) || strings.Contains(key, "..") {
		return "", ErrInvalid
	}
	return value, nil
}
