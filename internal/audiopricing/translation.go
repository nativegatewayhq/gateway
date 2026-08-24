package audiopricing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

const TranslationDurationStrategy = "openai-translation-duration-v1"

type TranslationPrice struct {
	ID, ChannelID, Protocol, Operation, Model, Strategy, Currency string
	CostPerMinute, SalePerMinute                                  int64
	MaximumDurationMilliseconds                                   int64
	EffectiveFrom                                                 time.Time
	EffectiveUntil                                                *time.Time
}

type TranslationPriceRequest struct {
	ChannelID, Protocol, Operation, Model string
	At                                    time.Time
}

type TranslationEstimate struct {
	Price                    TranslationPrice
	MaximumCost, MaximumSale int64
}

type TranslationActual struct {
	DurationMilliseconds int64
	Cost, Sale           int64
}

func (service *Service) PublishTranslation(ctx context.Context, price TranslationPrice, publicationKey string) (TranslationPrice, error) {
	price = canonicalTranslationPrice(price)
	if !validTranslationPrice(price) || !validText(publicationKey, 200) || !marginOK(price.CostPerMinute, price.SalePerMinute, service.minimumMarginBPS) {
		return TranslationPrice{}, ErrInvalid
	}
	estimate, err := calculateTranslation(price, price.MaximumDurationMilliseconds, service.minimumMarginBPS)
	if err != nil || !marginOK(estimate.Cost, estimate.Sale, service.minimumMarginBPS) {
		return TranslationPrice{}, ErrInvalid
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranslationPrice{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "audio-translation-price:"+publicationKey); err != nil {
		return TranslationPrice{}, err
	}
	var existing TranslationPrice
	err = tx.QueryRow(ctx, selectTranslationPrice+` JOIN audio_translation_price_publications pub ON pub.price_id=p.id WHERE pub.publication_key=$1`, publicationKey).Scan(scanTranslationPrice(&existing)...)
	if err == nil {
		if !sameTranslationPrice(existing, price) {
			return TranslationPrice{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TranslationPrice{}, err
	}
	price.ID, err = translationPriceID()
	if err != nil {
		return TranslationPrice{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audio_translation_prices(id,channel_id,protocol,operation,model,strategy,currency,cost_per_minute,sale_per_minute,maximum_duration_milliseconds,effective_from,effective_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, price.ID, price.ChannelID, price.Protocol, price.Operation, price.Model, price.Strategy, price.Currency, price.CostPerMinute, price.SalePerMinute, price.MaximumDurationMilliseconds, price.EffectiveFrom, price.EffectiveUntil)
	if err != nil {
		return TranslationPrice{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_translation_price_publications(publication_key,price_id) VALUES($1,$2)`, publicationKey, price.ID); err != nil {
		return TranslationPrice{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranslationPrice{}, err
	}
	return price, nil
}

func (service *Service) EstimateTranslation(ctx context.Context, request TranslationPriceRequest) (TranslationEstimate, error) {
	return service.estimateTranslation(ctx, service.pool, request)
}

func (service *Service) EstimateTranslationInTx(ctx context.Context, tx pgx.Tx, request TranslationPriceRequest) (TranslationEstimate, error) {
	return service.estimateTranslation(ctx, tx, request)
}

func (service *Service) estimateTranslation(ctx context.Context, query querier, request TranslationPriceRequest) (TranslationEstimate, error) {
	if request.Protocol == "" {
		request.Protocol = "openai"
	}
	if request.Operation == "" {
		request.Operation = "audio.translation"
	}
	if request.At.IsZero() {
		request.At = service.now().UTC()
	}
	if request.Protocol != "openai" || request.Operation != "audio.translation" || !validID(request.ChannelID, "channel_") || !validText(request.Model, 200) {
		return TranslationEstimate{}, ErrInvalid
	}
	var price TranslationPrice
	err := query.QueryRow(ctx, selectTranslationPrice+` JOIN provider_channels c ON c.id=p.channel_id WHERE p.channel_id=$1 AND p.protocol=$2 AND p.operation=$3 AND p.model=$4 AND c.status='active' AND p.effective_from<=$5 AND (p.effective_until IS NULL OR p.effective_until>$5)`, request.ChannelID, request.Protocol, request.Operation, request.Model, request.At).Scan(scanTranslationPrice(&price)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return TranslationEstimate{}, ErrUnavailable
	}
	if err != nil {
		return TranslationEstimate{}, err
	}
	actual, err := calculateTranslation(price, price.MaximumDurationMilliseconds, service.minimumMarginBPS)
	if err != nil {
		return TranslationEstimate{}, err
	}
	return TranslationEstimate{Price: price, MaximumCost: actual.Cost, MaximumSale: actual.Sale}, nil
}

func (service *Service) CalculateTranslationInTx(ctx context.Context, tx pgx.Tx, priceID string, durationMilliseconds int64) (TranslationActual, error) {
	if !validID(priceID, "altp_") {
		return TranslationActual{}, ErrInvalid
	}
	var price TranslationPrice
	if err := tx.QueryRow(ctx, selectTranslationPrice+` WHERE p.id=$1`, priceID).Scan(scanTranslationPrice(&price)...); errors.Is(err, pgx.ErrNoRows) {
		return TranslationActual{}, ErrUnavailable
	} else if err != nil {
		return TranslationActual{}, err
	}
	return calculateTranslation(price, durationMilliseconds, service.minimumMarginBPS)
}

func calculateTranslation(price TranslationPrice, durationMilliseconds, minimumMarginBPS int64) (TranslationActual, error) {
	if !validTranslationPrice(price) || durationMilliseconds < 1 || durationMilliseconds > price.MaximumDurationMilliseconds {
		return TranslationActual{}, ErrInvalid
	}
	cost, ok := durationAmount(durationMilliseconds, price.CostPerMinute)
	if !ok {
		return TranslationActual{}, ErrInvalid
	}
	sale, ok := durationAmount(durationMilliseconds, price.SalePerMinute)
	if !ok || sale < 1 || !marginOK(cost, sale, minimumMarginBPS) {
		return TranslationActual{}, ErrMargin
	}
	return TranslationActual{DurationMilliseconds: durationMilliseconds, Cost: cost, Sale: sale}, nil
}

func canonicalTranslationPrice(price TranslationPrice) TranslationPrice {
	if price.Protocol == "" {
		price.Protocol = "openai"
	}
	if price.Operation == "" {
		price.Operation = "audio.translation"
	}
	if price.Strategy == "" {
		price.Strategy = TranslationDurationStrategy
	}
	if price.Currency == "" {
		price.Currency = "USD_TICKS"
	}
	price.EffectiveFrom = price.EffectiveFrom.UTC().Truncate(time.Microsecond)
	if price.EffectiveUntil != nil {
		until := price.EffectiveUntil.UTC().Truncate(time.Microsecond)
		price.EffectiveUntil = &until
	}
	return price
}

func validTranslationPrice(price TranslationPrice) bool {
	return validID(price.ChannelID, "channel_") && price.Protocol == "openai" && price.Operation == "audio.translation" && price.Strategy == TranslationDurationStrategy && price.Currency == "USD_TICKS" && validText(price.Model, 200) && price.CostPerMinute >= 0 && price.SalePerMinute > 0 && price.MaximumDurationMilliseconds > 0 && price.MaximumDurationMilliseconds <= 24*60*60*1000 && !price.EffectiveFrom.IsZero() && (price.EffectiveUntil == nil || price.EffectiveUntil.After(price.EffectiveFrom))
}

func sameTranslationPrice(a, b TranslationPrice) bool {
	return a.ChannelID == b.ChannelID && a.Protocol == b.Protocol && a.Operation == b.Operation && a.Model == b.Model && a.Strategy == b.Strategy && a.Currency == b.Currency && a.CostPerMinute == b.CostPerMinute && a.SalePerMinute == b.SalePerMinute && a.MaximumDurationMilliseconds == b.MaximumDurationMilliseconds && a.EffectiveFrom.Equal(b.EffectiveFrom) && ((a.EffectiveUntil == nil && b.EffectiveUntil == nil) || (a.EffectiveUntil != nil && b.EffectiveUntil != nil && a.EffectiveUntil.Equal(*b.EffectiveUntil)))
}

const selectTranslationPrice = `SELECT p.id,p.channel_id,p.protocol,p.operation,p.model,p.strategy,p.currency,p.cost_per_minute,p.sale_per_minute,p.maximum_duration_milliseconds,p.effective_from,p.effective_until FROM audio_translation_prices p`

func scanTranslationPrice(price *TranslationPrice) []any {
	return []any{&price.ID, &price.ChannelID, &price.Protocol, &price.Operation, &price.Model, &price.Strategy, &price.Currency, &price.CostPerMinute, &price.SalePerMinute, &price.MaximumDurationMilliseconds, &price.EffectiveFrom, &price.EffectiveUntil}
}

func translationPriceID() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return "altp_" + hex.EncodeToString(value), nil
}
