package audioassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
)

type Upload struct {
	ContentType string
	Size        int64
	SHA256      [32]byte
	Body        io.ReadSeeker
}
type Materialized struct {
	Asset Asset
	Lease Lease
	Body  io.ReadCloser
}
type Service struct {
	repository       *Repository
	objects          ObjectStore
	retention, lease time.Duration
	maximumBytes     int64
	workerOwner      string
	telemetry        *telemetry.Recorder
}

func NewService(repository *Repository, objects ObjectStore, retention, lease time.Duration, maximumBytes int64, workerOwner string) (*Service, error) {
	if repository == nil || objects == nil || retention < time.Hour || retention > 30*24*time.Hour || lease <= 0 || lease > 10*time.Minute || maximumBytes < 1 || maximumBytes > 512<<20 || strings.TrimSpace(workerOwner) == "" {
		return nil, ErrInvalid
	}
	return &Service{repository: repository, objects: objects, retention: retention, lease: lease, maximumBytes: maximumBytes, workerOwner: workerOwner}, nil
}
func (service *Service) SetTelemetry(recorder *telemetry.Recorder) { service.telemetry = recorder }
func (service *Service) record(ctx context.Context, stage string, err error) {
	if service.telemetry == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	service.telemetry.Storage(ctx, telemetry.StorageRecord{Protocol: "openai", Stage: stage, Outcome: outcome})
}
func (service *Service) Create(ctx context.Context, owner apikey.Principal, key string, upload Upload) (result Asset, returnedErr error) {
	defer func() { service.record(ctx, "upload", returnedErr) }()
	if upload.Body == nil || upload.Size < 1 || upload.Size > service.maximumBytes || !strings.HasPrefix(upload.ContentType, "audio/") || upload.SHA256 == ([32]byte{}) {
		return Asset{}, ErrInvalid
	}
	keyDigest := sha256.Sum256([]byte(key))
	objectKey := "audio/" + owner.OrganizationID + "/" + hex.EncodeToString(keyDigest[:16]) + "/" + hex.EncodeToString(upload.SHA256[:]) + ".bin"
	fingerprint := sha256.Sum256([]byte(upload.ContentType + "\x00" + hex.EncodeToString(upload.SHA256[:]) + "\x00" + strconv.FormatInt(upload.Size, 10)))
	asset, err := service.repository.Begin(ctx, BeginRequest{Owner: owner, IdempotencyKey: key, Fingerprint: fingerprint, ObjectKey: objectKey, ContentType: upload.ContentType, ByteLength: upload.Size, SHA256: upload.SHA256, ExpiresAt: time.Now().UTC().Add(service.retention)})
	if err == nil && asset.State == Available {
		return asset, nil
	}
	if err != nil {
		return Asset{}, err
	}
	if _, err = upload.Body.Seek(0, io.SeekStart); err != nil {
		_ = service.repository.MarkFailed(context.WithoutCancel(ctx), asset.ID, "spool_seek_failed")
		return Asset{}, ErrStorage
	}
	if err = service.objects.Put(ctx, asset.ObjectKey, asset.ContentType, asset.ByteLength, asset.SHA256, upload.Body); err != nil {
		_ = service.repository.MarkFailed(context.WithoutCancel(ctx), asset.ID, "object_put_failed")
		return Asset{}, err
	}
	return service.repository.MarkAvailable(context.WithoutCancel(ctx), asset.ID)
}
func (service *Service) Materialize(ctx context.Context, owner apikey.Principal, id string) (result Materialized, returnedErr error) {
	defer func() { service.record(ctx, "fetch", returnedErr) }()
	asset, lease, err := service.repository.Acquire(ctx, owner, id, service.workerOwner, service.lease)
	if err != nil {
		return Materialized{}, err
	}
	body, err := service.objects.Get(ctx, asset.ObjectKey, asset.ByteLength)
	if err != nil {
		_ = service.repository.Release(context.WithoutCancel(ctx), lease)
		return Materialized{}, err
	}
	return Materialized{Asset: asset, Lease: lease, Body: body}, nil
}
func (service *Service) Release(ctx context.Context, materialized Materialized) error {
	if materialized.Body != nil {
		_ = materialized.Body.Close()
	}
	return service.repository.Release(ctx, materialized.Lease)
}
func (service *Service) Delete(ctx context.Context, owner apikey.Principal, id string) (asset Asset, returnedErr error) {
	defer func() { service.record(ctx, "delete", returnedErr) }()
	return service.repository.RequestDelete(ctx, owner, id)
}
func (service *Service) Get(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	return service.repository.Resolve(ctx, owner, id)
}
func (service *Service) RunCleanup(ctx context.Context) (processed bool, returnedErr error) {
	defer func() { service.record(ctx, "cleanup", returnedErr) }()
	asset, lease, found, err := service.repository.ClaimCleanup(ctx, service.workerOwner+":cleanup", service.lease)
	if err != nil || !found {
		return found, err
	}
	if err = service.objects.Delete(ctx, asset.ObjectKey); err != nil {
		_ = service.repository.Release(context.WithoutCancel(ctx), lease)
		return true, err
	}
	return true, service.repository.MarkDeleted(ctx, lease)
}
func (service *Service) Ready(ctx context.Context) error { return service.objects.Ready(ctx) }
