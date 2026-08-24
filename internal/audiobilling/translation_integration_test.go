//go:build integration

package audiobilling

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

func translationFixture(t *testing.T) *TranslationService {
	t.Helper()
	_, pool := transcriptionFixture(t)
	prices, _ := audiopricing.New(pool, 0)
	_, err := prices.PublishTranslation(context.Background(), audiopricing.TranslationPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "whisper-1", CostPerMinute: 60, SalePerMinute: 120, MaximumDurationMilliseconds: 600_000, EffectiveFrom: time.Now().Add(-time.Hour)}, "translation-price")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewTranslationWithControls(pool, prices, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func translationBegin(key string) TranslationBeginRequest {
	return TranslationBeginRequest{RequestID: "request-" + key, OrganizationID: "org_transcription", ProjectID: "project_transcription", APIKeyID: "key_transcription", Model: "whisper-1", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: [32]byte{8}}
}

func TestTranslationConcurrentBeginAndSettlementAreExactlyOnce(t *testing.T) {
	service := translationFixture(t)
	ctx := context.Background()
	var wait sync.WaitGroup
	charges := make(chan TranslationCharge, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			c, err := service.Begin(ctx, translationBegin("same-key"))
			charges <- c
			errs <- err
		}()
	}
	wait.Wait()
	close(charges)
	close(errs)
	var charge TranslationCharge
	for c := range charges {
		if c.ID != "" {
			charge = c
		}
	}
	success, pending := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrPending) {
			pending++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || pending != 1 || charge.ReservedSale != 1200 {
		t.Fatalf("success=%d pending=%d charge=%+v", success, pending, charge)
	}
	evidence := TranslationEvidence{SchemaVersion: "openai-translation-duration-json-v1", DurationMilliseconds: 60_001, Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}, "Set-Cookie": {"secret"}}, SHA256: [32]byte{9}}
	settled, err := service.Complete(ctx, charge.ID, evidence)
	if err != nil || settled.CapturedSale != 121 || settled.ActualCost == nil || *settled.ActualCost != 61 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if _, err = service.Complete(ctx, charge.ID, evidence); err != nil {
		t.Fatal(err)
	}
}

func TestTranslationKnownFailureReleasesAndUncertainOutcomeStaysReserved(t *testing.T) {
	service := translationFixture(t)
	ctx := context.Background()
	released, err := service.Begin(ctx, translationBegin("release-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Release(ctx, released.ID, "provider_non_2xx"); err != nil {
		t.Fatal(err)
	}
	uncertain, err := service.Begin(ctx, translationBegin("uncertain-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkReconciling(ctx, uncertain.ID, "duration_invalid", nil); err != nil {
		t.Fatal(err)
	}
}
