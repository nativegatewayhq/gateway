package audiopricing

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	TranscriptionTokenStrategy          = "openai-transcription-token-v1"
	TranscriptionDurationStrategy       = "openai-transcription-duration-v1"
	MillisecondsPerMinute         int64 = 60_000
)

type TranscriptionUsageType string

const (
	TranscriptionTokens   TranscriptionUsageType = "tokens"
	TranscriptionDuration TranscriptionUsageType = "duration"
)

type TranscriptionPrice struct {
	ID, ChannelID, Protocol, Operation, Model, Strategy, Currency string
	CostInputPerMillion, CostOutputPerMillion                     int64
	SaleInputPerMillion, SaleOutputPerMillion                     int64
	CostPerMinute, SalePerMinute                                  int64
	MaximumInputTokens, MaximumOutputTokens                       int64
	MaximumDurationMilliseconds                                   int64
	EffectiveFrom                                                 time.Time
	EffectiveUntil                                                *time.Time
}

type TranscriptionPriceRequest struct {
	ChannelID, Protocol, Operation, Model string
	At                                    time.Time
}

type TranscriptionEstimate struct {
	Price       TranscriptionPrice
	MaximumCost int64
	MaximumSale int64
}

type TranscriptionUsage struct {
	Type                                           TranscriptionUsageType
	InputTokens, AudioInputTokens, TextInputTokens int64
	OutputTokens, TotalTokens                      int64
	DurationMilliseconds                           int64
}

type TranscriptionActual struct {
	Usage      TranscriptionUsage
	Cost, Sale int64
}

func (s *Service) PublishTranscription(ctx context.Context, p TranscriptionPrice, key string) (TranscriptionPrice, error) {
	p = canonicalTranscriptionPrice(p)
	if !validTranscriptionPrice(p) || !validText(key, 200) || !transcriptionRateMarginsOK(p, s.minimumMarginBPS) {
		return TranscriptionPrice{}, ErrInvalid
	}
	estimate, err := estimateTranscriptionAmounts(p)
	if err != nil || !marginOK(estimate.MaximumCost, estimate.MaximumSale, s.minimumMarginBPS) {
		return TranscriptionPrice{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranscriptionPrice{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "audio-transcription-price:"+key); err != nil {
		return TranscriptionPrice{}, err
	}
	var existing TranscriptionPrice
	err = tx.QueryRow(ctx, selectTranscriptionPrice+` JOIN audio_transcription_price_publications pub ON pub.price_id=p.id WHERE pub.publication_key=$1`, key).Scan(scanTranscriptionPrice(&existing)...)
	if err == nil {
		if !sameTranscriptionPrice(existing, p) {
			return TranscriptionPrice{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TranscriptionPrice{}, err
	}
	p.ID, err = s.transcriptionID()
	if err != nil {
		return TranscriptionPrice{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audio_transcription_prices(id,channel_id,protocol,operation,model,strategy,currency,cost_input_per_million_tokens,cost_output_per_million_tokens,sale_input_per_million_tokens,sale_output_per_million_tokens,cost_per_minute,sale_per_minute,maximum_input_tokens,maximum_output_tokens,maximum_duration_milliseconds,effective_from,effective_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, p.ID, p.ChannelID, p.Protocol, p.Operation, p.Model, p.Strategy, p.Currency, p.CostInputPerMillion, p.CostOutputPerMillion, p.SaleInputPerMillion, p.SaleOutputPerMillion, p.CostPerMinute, p.SalePerMinute, p.MaximumInputTokens, p.MaximumOutputTokens, p.MaximumDurationMilliseconds, p.EffectiveFrom, p.EffectiveUntil)
	if err != nil {
		return TranscriptionPrice{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_transcription_price_publications(publication_key,price_id) VALUES($1,$2)`, key, p.ID); err != nil {
		return TranscriptionPrice{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranscriptionPrice{}, err
	}
	return p, nil
}

func (s *Service) EstimateTranscriptionInTx(ctx context.Context, tx pgx.Tx, r TranscriptionPriceRequest) (TranscriptionEstimate, error) {
	if tx == nil {
		return TranscriptionEstimate{}, ErrInvalid
	}
	return s.estimateTranscription(ctx, tx, r)
}

func (s *Service) EstimateTranscription(ctx context.Context, r TranscriptionPriceRequest) (TranscriptionEstimate, error) {
	return s.estimateTranscription(ctx, s.pool, r)
}

func (s *Service) CalculateTranscriptionInTx(ctx context.Context, tx pgx.Tx, priceID string, usage TranscriptionUsage) (TranscriptionActual, error) {
	if tx == nil || len(priceID) != len("atp_")+32 {
		return TranscriptionActual{}, ErrInvalid
	}
	var p TranscriptionPrice
	if err := tx.QueryRow(ctx, selectTranscriptionPrice+` WHERE p.id=$1`, priceID).Scan(scanTranscriptionPrice(&p)...); errors.Is(err, pgx.ErrNoRows) {
		return TranscriptionActual{}, ErrUnavailable
	} else if err != nil {
		return TranscriptionActual{}, err
	}
	return CalculateTranscriptionActual(p, usage, s.minimumMarginBPS)
}

func (s *Service) estimateTranscription(ctx context.Context, q querier, r TranscriptionPriceRequest) (TranscriptionEstimate, error) {
	if r.Protocol == "" {
		r.Protocol = "openai"
	}
	if r.Operation == "" {
		r.Operation = "audio.transcription"
	}
	if r.At.IsZero() {
		r.At = s.now().UTC()
	}
	if r.Protocol != "openai" || r.Operation != "audio.transcription" || !validID(r.ChannelID, "channel_") || !validText(r.Model, 200) {
		return TranscriptionEstimate{}, ErrInvalid
	}
	var p TranscriptionPrice
	err := q.QueryRow(ctx, selectTranscriptionPrice+` JOIN provider_channels c ON c.id=p.channel_id WHERE p.channel_id=$1 AND p.protocol=$2 AND p.operation=$3 AND p.model=$4 AND c.status='active' AND p.effective_from<=$5 AND (p.effective_until IS NULL OR p.effective_until>$5)`, r.ChannelID, r.Protocol, r.Operation, r.Model, r.At).Scan(scanTranscriptionPrice(&p)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return TranscriptionEstimate{}, ErrUnavailable
	}
	if err != nil {
		return TranscriptionEstimate{}, err
	}
	estimate, err := estimateTranscriptionAmounts(p)
	if err != nil {
		return TranscriptionEstimate{}, ErrInvalid
	}
	if !marginOK(estimate.MaximumCost, estimate.MaximumSale, s.minimumMarginBPS) {
		return TranscriptionEstimate{}, ErrMargin
	}
	return estimate, nil
}

func CalculateTranscriptionActual(p TranscriptionPrice, usage TranscriptionUsage, minimumMarginBPS int64) (TranscriptionActual, error) {
	if minimumMarginBPS < 0 || minimumMarginBPS > 10000 || !validTranscriptionUsage(p, usage) {
		return TranscriptionActual{}, ErrInvalid
	}
	var cost, sale int64
	var ok bool
	switch usage.Type {
	case TranscriptionTokens:
		cost, ok = tokenAmounts(usage.InputTokens, usage.OutputTokens, p.CostInputPerMillion, p.CostOutputPerMillion)
		if !ok {
			return TranscriptionActual{}, ErrInvalid
		}
		sale, ok = tokenAmounts(usage.InputTokens, usage.OutputTokens, p.SaleInputPerMillion, p.SaleOutputPerMillion)
	case TranscriptionDuration:
		cost, ok = durationAmount(usage.DurationMilliseconds, p.CostPerMinute)
		if !ok {
			return TranscriptionActual{}, ErrInvalid
		}
		sale, ok = durationAmount(usage.DurationMilliseconds, p.SalePerMinute)
	}
	if !ok || sale < 1 || !marginOK(cost, sale, minimumMarginBPS) {
		return TranscriptionActual{}, ErrMargin
	}
	return TranscriptionActual{Usage: usage, Cost: cost, Sale: sale}, nil
}

func estimateTranscriptionAmounts(p TranscriptionPrice) (TranscriptionEstimate, error) {
	usage := TranscriptionUsage{}
	switch p.Strategy {
	case TranscriptionTokenStrategy:
		usage = TranscriptionUsage{Type: TranscriptionTokens, InputTokens: p.MaximumInputTokens, AudioInputTokens: p.MaximumInputTokens, OutputTokens: p.MaximumOutputTokens, TotalTokens: p.MaximumInputTokens + p.MaximumOutputTokens}
	case TranscriptionDurationStrategy:
		usage = TranscriptionUsage{Type: TranscriptionDuration, DurationMilliseconds: p.MaximumDurationMilliseconds}
	default:
		return TranscriptionEstimate{}, ErrInvalid
	}
	actual, err := CalculateTranscriptionActual(p, usage, 0)
	if err != nil {
		return TranscriptionEstimate{}, err
	}
	return TranscriptionEstimate{Price: p, MaximumCost: actual.Cost, MaximumSale: actual.Sale}, nil
}

func tokenAmounts(input, output, inputRate, outputRate int64) (int64, bool) {
	a, ok := Amount(input, inputRate)
	if !ok {
		return 0, false
	}
	b, ok := Amount(output, outputRate)
	if !ok || a > int64(^uint64(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}

func durationAmount(milliseconds, rate int64) (int64, bool) {
	if milliseconds < 0 || rate < 0 {
		return 0, false
	}
	if milliseconds == 0 || rate == 0 {
		return 0, true
	}
	v := new(big.Int).Mul(big.NewInt(milliseconds), big.NewInt(rate))
	v.Add(v, big.NewInt(MillisecondsPerMinute-1))
	v.Div(v, big.NewInt(MillisecondsPerMinute))
	return v.Int64(), v.IsInt64()
}

func validTranscriptionUsage(p TranscriptionPrice, u TranscriptionUsage) bool {
	switch {
	case p.Strategy == TranscriptionTokenStrategy && u.Type == TranscriptionTokens:
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.AudioInputTokens < 0 || u.TextInputTokens < 0 || u.TotalTokens < 0 {
			return false
		}
		return u.InputTokens <= p.MaximumInputTokens && u.OutputTokens <= p.MaximumOutputTokens && u.AudioInputTokens+u.TextInputTokens == u.InputTokens && u.InputTokens+u.OutputTokens == u.TotalTokens && u.DurationMilliseconds == 0
	case p.Strategy == TranscriptionDurationStrategy && u.Type == TranscriptionDuration:
		return u.DurationMilliseconds > 0 && u.DurationMilliseconds <= p.MaximumDurationMilliseconds && u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 && u.AudioInputTokens == 0 && u.TextInputTokens == 0
	default:
		return false
	}
}

func canonicalTranscriptionPrice(p TranscriptionPrice) TranscriptionPrice {
	if p.Protocol == "" {
		p.Protocol = "openai"
	}
	if p.Operation == "" {
		p.Operation = "audio.transcription"
	}
	if p.Currency == "" {
		p.Currency = "USD_TICKS"
	}
	// PostgreSQL timestamptz preserves microseconds. Canonicalize before both
	// insertion and publication-key equality checks so a nanosecond-bearing
	// caller can replay the same immutable publication exactly.
	p.EffectiveFrom = p.EffectiveFrom.UTC().Truncate(time.Microsecond)
	if p.EffectiveUntil != nil {
		until := p.EffectiveUntil.UTC().Truncate(time.Microsecond)
		p.EffectiveUntil = &until
	}
	return p
}

func validTranscriptionPrice(p TranscriptionPrice) bool {
	if !validID(p.ChannelID, "channel_") || p.Protocol != "openai" || p.Operation != "audio.transcription" || p.Currency != "USD_TICKS" || !validText(p.Model, 200) || p.EffectiveFrom.IsZero() || (p.EffectiveUntil != nil && !p.EffectiveUntil.After(p.EffectiveFrom)) {
		return false
	}
	switch p.Strategy {
	case TranscriptionTokenStrategy:
		return p.CostInputPerMillion >= 0 && p.CostOutputPerMillion >= 0 && p.SaleInputPerMillion > 0 && p.SaleOutputPerMillion > 0 && p.MaximumInputTokens > 0 && p.MaximumOutputTokens > 0 && p.CostPerMinute == 0 && p.SalePerMinute == 0 && p.MaximumDurationMilliseconds == 0 && p.MaximumInputTokens <= 10_000_000 && p.MaximumOutputTokens <= 10_000_000
	case TranscriptionDurationStrategy:
		return p.CostPerMinute >= 0 && p.SalePerMinute > 0 && p.MaximumDurationMilliseconds > 0 && p.MaximumDurationMilliseconds <= 24*60*60*1000 && p.CostInputPerMillion == 0 && p.CostOutputPerMillion == 0 && p.SaleInputPerMillion == 0 && p.SaleOutputPerMillion == 0 && p.MaximumInputTokens == 0 && p.MaximumOutputTokens == 0
	default:
		return false
	}
}

func transcriptionRateMarginsOK(p TranscriptionPrice, minimumMarginBPS int64) bool {
	switch p.Strategy {
	case TranscriptionTokenStrategy:
		return marginOK(p.CostInputPerMillion, p.SaleInputPerMillion, minimumMarginBPS) && marginOK(p.CostOutputPerMillion, p.SaleOutputPerMillion, minimumMarginBPS)
	case TranscriptionDurationStrategy:
		return marginOK(p.CostPerMinute, p.SalePerMinute, minimumMarginBPS)
	default:
		return false
	}
}

func sameTranscriptionPrice(a, b TranscriptionPrice) bool {
	return a.ChannelID == b.ChannelID && a.Protocol == b.Protocol && a.Operation == b.Operation && a.Model == b.Model && a.Strategy == b.Strategy && a.Currency == b.Currency && a.CostInputPerMillion == b.CostInputPerMillion && a.CostOutputPerMillion == b.CostOutputPerMillion && a.SaleInputPerMillion == b.SaleInputPerMillion && a.SaleOutputPerMillion == b.SaleOutputPerMillion && a.CostPerMinute == b.CostPerMinute && a.SalePerMinute == b.SalePerMinute && a.MaximumInputTokens == b.MaximumInputTokens && a.MaximumOutputTokens == b.MaximumOutputTokens && a.MaximumDurationMilliseconds == b.MaximumDurationMilliseconds && a.EffectiveFrom.Equal(b.EffectiveFrom) && ((a.EffectiveUntil == nil && b.EffectiveUntil == nil) || (a.EffectiveUntil != nil && b.EffectiveUntil != nil && a.EffectiveUntil.Equal(*b.EffectiveUntil)))
}

const selectTranscriptionPrice = `SELECT p.id,p.channel_id,p.protocol,p.operation,p.model,p.strategy,p.currency,p.cost_input_per_million_tokens,p.cost_output_per_million_tokens,p.sale_input_per_million_tokens,p.sale_output_per_million_tokens,p.cost_per_minute,p.sale_per_minute,p.maximum_input_tokens,p.maximum_output_tokens,p.maximum_duration_milliseconds,p.effective_from,p.effective_until FROM audio_transcription_prices p`

func scanTranscriptionPrice(p *TranscriptionPrice) []any {
	return []any{&p.ID, &p.ChannelID, &p.Protocol, &p.Operation, &p.Model, &p.Strategy, &p.Currency, &p.CostInputPerMillion, &p.CostOutputPerMillion, &p.SaleInputPerMillion, &p.SaleOutputPerMillion, &p.CostPerMinute, &p.SalePerMinute, &p.MaximumInputTokens, &p.MaximumOutputTokens, &p.MaximumDurationMilliseconds, &p.EffectiveFrom, &p.EffectiveUntil}
}

func (s *Service) transcriptionID() (string, error) {
	id, err := s.id()
	if err != nil {
		return "", err
	}
	return "atp_" + id[len("asp_"):], nil
}
