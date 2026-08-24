package jobs

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type ImageResultManager struct{ Manager *imagestorage.Manager }

func (manager ImageResultManager) Transform(ctx context.Context, job joboperation.Job) (joboperation.Snapshot, error) {
	if manager.Manager == nil {
		return joboperation.Snapshot{}, errors.New("image result manager unavailable")
	}
	body, err := manager.Manager.Transform(ctx, imagestorage.TransformInput{Protocol: job.Protocol, Provider: job.Provider, ChannelID: job.ChannelID, RequestID: job.RequestID, ChargeID: job.ChargeID, Body: job.Snapshot.Body})
	if err != nil {
		return joboperation.Snapshot{}, err
	}
	return joboperation.Snapshot{Status: job.Snapshot.Status, Headers: job.Snapshot.Headers, Body: body, SHA256: sha256.Sum256(body)}, nil
}

type ResultRouter struct{ Image, Video TerminalResultManager }

func (router ResultRouter) Transform(ctx context.Context, job joboperation.Job) (joboperation.Snapshot, error) {
	switch job.Protocol {
	case "replicate", "fal":
		if router.Image != nil {
			return router.Image.Transform(ctx, job)
		}
	case "runway":
		if router.Video != nil {
			return router.Video.Transform(ctx, job)
		}
	}
	return joboperation.Snapshot{}, errors.New("terminal result manager unavailable")
}
