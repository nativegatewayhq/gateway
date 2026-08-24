package speechstorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audioassets"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
)

type CaptureResult struct {
	Asset         Asset
	Bytes         int64
	SHA256        [32]byte
	DownstreamErr error
	StorageErr    error
}
type Service struct {
	repository           *Repository
	objects              audioassets.ObjectStore
	retention, lease     time.Duration
	maximumBytes         int64
	tempDir, workerOwner string
	slots                chan struct{}
	telemetry            *telemetry.Recorder
}

func NewService(repository *Repository, objects audioassets.ObjectStore, config Config, workerOwner string) (*Service, error) {
	if repository == nil || objects == nil || config.Validate() != nil || config.Mode != Managed || strings.TrimSpace(workerOwner) == "" {
		return nil, ErrInvalid
	}
	return &Service{repository: repository, objects: objects, retention: config.Retention, lease: config.CleanupLease, maximumBytes: config.MaximumBytes, tempDir: config.TemporaryDirectory, workerOwner: workerOwner, slots: make(chan struct{}, config.MaximumConcurrentCaptures)}, nil
}
func (s *Service) SetTelemetry(r *telemetry.Recorder) { s.telemetry = r }
func (s *Service) record(ctx context.Context, stage string, err error) {
	if s.telemetry == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	s.telemetry.Storage(ctx, telemetry.StorageRecord{Protocol: "openai", Stage: stage, Outcome: outcome})
}
func (s *Service) Begin(ctx context.Context, owner apikey.Principal, key, chargeID string, fingerprint [32]byte) (Asset, error) {
	return s.repository.Begin(ctx, BeginRequest{Owner: owner, ChargeID: chargeID, IdempotencyKey: key, Fingerprint: fingerprint, ExpiresAt: time.Now().UTC().Add(s.retention)})
}
func (s *Service) Capture(ctx context.Context, asset Asset, contentType string, source io.Reader, downstream io.Writer, expected int64) (result CaptureResult, returnedErr error) {
	defer func() {
		recordErr := returnedErr
		if recordErr == nil {
			recordErr = result.StorageErr
		}
		s.record(ctx, "capture", recordErr)
	}()
	if asset.State == Available {
		return CaptureResult{Asset: asset, Bytes: asset.ByteLength, SHA256: asset.SHA256}, nil
	}
	if asset.State != Capturing || source == nil || !strings.HasPrefix(contentType, "audio/") {
		return CaptureResult{}, ErrInvalid
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return CaptureResult{}, ErrPending
	}
	file, err := os.CreateTemp(s.tempDir, "gateway-speech-output-*")
	if err != nil {
		return CaptureResult{}, err
	}
	name := file.Name()
	defer func() { file.Close(); os.Remove(name) }()
	_ = file.Chmod(0600)
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var total int64
	var downstreamErr error
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > s.maximumBytes {
				_ = s.repository.MarkFailure(context.WithoutCancel(ctx), asset.ID, "response_too_large", true)
				return CaptureResult{}, errors.New("speech output too large")
			}
			if _, err = file.Write(buffer[:n]); err != nil {
				_ = s.repository.MarkFailure(context.WithoutCancel(ctx), asset.ID, "spool_write_failed", true)
				return CaptureResult{}, err
			}
			_, _ = hash.Write(buffer[:n])
			if downstream != nil && downstreamErr == nil {
				written, writeErr := downstream.Write(buffer[:n])
				if writeErr != nil || written != n {
					downstreamErr = io.ErrClosedPipe
				} else if flusher, ok := downstream.(interface{ Flush() }); ok {
					flusher.Flush()
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = s.repository.MarkFailure(context.WithoutCancel(ctx), asset.ID, "provider_stream_failed", true)
			return CaptureResult{}, readErr
		}
	}
	if total < 1 || (expected >= 0 && total != expected) {
		_ = s.repository.MarkFailure(context.WithoutCancel(ctx), asset.ID, "provider_stream_truncated", true)
		return CaptureResult{}, io.ErrUnexpectedEOF
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	objectKey := "audio/speech/" + asset.OrganizationID + "/" + asset.ID + "/" + hex.EncodeToString(digest[:]) + extension(contentType)
	if err = s.repository.MarkCaptured(context.WithoutCancel(ctx), asset.ID, objectKey, contentType, total, digest); err != nil {
		return CaptureResult{Asset: asset, Bytes: total, SHA256: digest, DownstreamErr: downstreamErr, StorageErr: err}, nil
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return CaptureResult{}, err
	}
	if err = s.objects.Put(context.WithoutCancel(ctx), objectKey, contentType, total, digest, file); err != nil {
		_ = s.repository.MarkFailure(context.WithoutCancel(ctx), asset.ID, "object_put_failed", true)
		return CaptureResult{Asset: asset, Bytes: total, SHA256: digest, DownstreamErr: downstreamErr, StorageErr: err}, nil
	}
	asset, err = s.repository.MarkAvailable(context.WithoutCancel(ctx), asset.ID)
	if err != nil {
		return CaptureResult{Asset: asset, Bytes: total, SHA256: digest, DownstreamErr: downstreamErr, StorageErr: err}, nil
	}
	return CaptureResult{Asset: asset, Bytes: total, SHA256: digest, DownstreamErr: downstreamErr}, nil
}
func (s *Service) Get(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	return s.repository.Resolve(ctx, owner, id)
}
func (s *Service) Open(ctx context.Context, owner apikey.Principal, id string) (Asset, io.ReadCloser, error) {
	a, lease, err := s.repository.AcquireRead(ctx, owner, id, s.workerOwner+":download", s.lease)
	if err != nil {
		return Asset{}, nil, err
	}
	body, err := s.objects.Get(ctx, a.ObjectKey, a.ByteLength)
	if err != nil {
		_ = s.repository.Release(context.WithoutCancel(ctx), lease)
		return Asset{}, nil, err
	}
	return a, &leasedBody{ReadCloser: body, release: func() error { return s.repository.Release(context.WithoutCancel(ctx), lease) }}, nil
}
func (s *Service) Delete(ctx context.Context, owner apikey.Principal, id string) (Asset, error) {
	return s.repository.RequestDelete(ctx, owner, id)
}
func (s *Service) RunCleanup(ctx context.Context) (bool, error) {
	recoveryAsset, recoveryLease, recovering, recoveryErr := s.repository.ClaimRecovery(ctx, s.workerOwner+":recovery", s.lease)
	if recoveryErr != nil {
		return false, recoveryErr
	}
	if recovering {
		body, openErr := s.objects.Get(ctx, recoveryAsset.ObjectKey, recoveryAsset.ByteLength)
		if openErr != nil {
			_ = s.repository.Release(context.WithoutCancel(ctx), recoveryLease)
			return true, openErr
		}
		hash := sha256.New()
		read, readErr := io.Copy(hash, body)
		_ = body.Close()
		var digest [32]byte
		copy(digest[:], hash.Sum(nil))
		if readErr != nil || read != recoveryAsset.ByteLength || digest != recoveryAsset.SHA256 {
			_ = s.repository.Release(context.WithoutCancel(ctx), recoveryLease)
			return true, audioassets.ErrStorage
		}
		_, markErr := s.repository.MarkAvailable(ctx, recoveryAsset.ID)
		_ = s.repository.Release(context.WithoutCancel(ctx), recoveryLease)
		return true, markErr
	}
	a, l, found, err := s.repository.ClaimCleanup(ctx, s.workerOwner+":cleanup", s.lease)
	if err != nil || !found {
		return found, err
	}
	if a.ObjectKey != "" {
		if err = s.objects.Delete(ctx, a.ObjectKey); err != nil {
			return true, err
		}
	}
	return true, s.repository.MarkDeleted(ctx, l)
}
func (s *Service) Ready(ctx context.Context) error { return s.objects.Ready(ctx) }
func extension(contentType string) string {
	switch contentType {
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/flac":
		return ".flac"
	case "audio/aac":
		return ".aac"
	case "audio/opus":
		return ".opus"
	}
	return ".bin"
}

type leasedBody struct {
	io.ReadCloser
	release func() error
}

func (body *leasedBody) Close() error {
	closeErr := body.ReadCloser.Close()
	releaseErr := body.release()
	if closeErr != nil {
		return closeErr
	}
	return releaseErr
}
