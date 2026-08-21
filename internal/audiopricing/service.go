// Package audiopricing publishes immutable character-priced Speech rates.
package audiopricing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Million  int64 = 1_000_000
	Strategy       = "openai-speech-input-character-v1"
)

var (
	ErrInvalid     = errors.New("invalid audio speech price")
	ErrUnavailable = errors.New("audio speech price unavailable")
	ErrConflict    = errors.New("audio speech price publication conflict")
	ErrMargin      = errors.New("audio speech price margin violation")
)

type Price struct {
	ID, ChannelID, Protocol, Operation, Model, Strategy, Currency string
	CostPerMillion, SalePerMillion                                int64
	EffectiveFrom                                                 time.Time
	EffectiveUntil                                                *time.Time
}
type Request struct {
	ChannelID, Protocol, Operation, Model string
	Quantity                              int64
	At                                    time.Time
}
type Estimate struct {
	Price                Price
	Quantity, Cost, Sale int64
}
type Service struct {
	pool             *pgxpool.Pool
	minimumMarginBPS int64
	entropy          io.Reader
	now              func() time.Time
}

func New(pool *pgxpool.Pool, margin int64) (*Service, error) {
	if pool == nil || margin < 0 || margin > 10000 {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, minimumMarginBPS: margin, entropy: rand.Reader, now: time.Now}, nil
}

func (s *Service) Publish(ctx context.Context, p Price, key string) (Price, error) {
	p = canonical(p)
	if !validPrice(p) || !validText(key, 200) || !marginOK(p.CostPerMillion, p.SalePerMillion, s.minimumMarginBPS) {
		return Price{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Price{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "audio-price:"+key); err != nil {
		return Price{}, err
	}
	var existing Price
	err = tx.QueryRow(ctx, selectPrice+` JOIN audio_speech_price_publications pub ON pub.price_id=p.id WHERE pub.publication_key=$1`, key).Scan(scan(&existing)...)
	if err == nil {
		if !same(existing, p) {
			return Price{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Price{}, err
	}
	p.ID, err = s.id()
	if err != nil {
		return Price{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audio_speech_prices(id,channel_id,protocol,operation,model,strategy,currency,cost_per_million_characters,sale_per_million_characters,effective_from,effective_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, p.ID, p.ChannelID, p.Protocol, p.Operation, p.Model, p.Strategy, p.Currency, p.CostPerMillion, p.SalePerMillion, p.EffectiveFrom, p.EffectiveUntil)
	if err != nil {
		return Price{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_speech_price_publications(publication_key,price_id) VALUES($1,$2)`, key, p.ID); err != nil {
		return Price{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Price{}, err
	}
	return p, nil
}

func (s *Service) EstimateInTx(ctx context.Context, tx pgx.Tx, r Request) (Estimate, error) {
	if tx == nil {
		return Estimate{}, ErrInvalid
	}
	return s.estimate(ctx, tx, r)
}
func (s *Service) Estimate(ctx context.Context, r Request) (Estimate, error) {
	return s.estimate(ctx, s.pool, r)
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Service) estimate(ctx context.Context, q querier, r Request) (Estimate, error) {
	if r.Protocol == "" {
		r.Protocol = "openai"
	}
	if r.Operation == "" {
		r.Operation = "audio.speech"
	}
	if r.At.IsZero() {
		r.At = s.now().UTC()
	}
	if r.Protocol != "openai" || r.Operation != "audio.speech" || !validID(r.ChannelID, "channel_") || !validText(r.Model, 200) || r.Quantity < 1 || r.Quantity > 4096 {
		return Estimate{}, ErrInvalid
	}
	var p Price
	err := q.QueryRow(ctx, selectPrice+` JOIN provider_channels c ON c.id=p.channel_id WHERE p.channel_id=$1 AND p.protocol=$2 AND p.operation=$3 AND p.model=$4 AND p.strategy=$5 AND c.status='active' AND p.effective_from<=$6 AND (p.effective_until IS NULL OR p.effective_until>$6)`, r.ChannelID, r.Protocol, r.Operation, r.Model, Strategy, r.At).Scan(scan(&p)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Estimate{}, ErrUnavailable
	}
	if err != nil {
		return Estimate{}, err
	}
	if !marginOK(p.CostPerMillion, p.SalePerMillion, s.minimumMarginBPS) {
		return Estimate{}, ErrMargin
	}
	cost, ok := Amount(r.Quantity, p.CostPerMillion)
	if !ok {
		return Estimate{}, ErrInvalid
	}
	sale, ok := Amount(r.Quantity, p.SalePerMillion)
	if !ok || sale < 1 {
		return Estimate{}, ErrInvalid
	}
	return Estimate{p, r.Quantity, cost, sale}, nil
}

func Amount(quantity, rate int64) (int64, bool) {
	if quantity < 0 || rate < 0 {
		return 0, false
	}
	if quantity == 0 || rate == 0 {
		return 0, true
	}
	v := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(rate))
	v.Add(v, big.NewInt(Million-1))
	v.Div(v, big.NewInt(Million))
	return v.Int64(), v.IsInt64()
}
func marginOK(cost, sale, bps int64) bool {
	l := new(big.Int).Mul(big.NewInt(sale), big.NewInt(10000))
	r := new(big.Int).Mul(big.NewInt(cost), big.NewInt(10000+bps))
	return l.Cmp(r) >= 0
}
func canonical(p Price) Price {
	if p.Protocol == "" {
		p.Protocol = "openai"
	}
	if p.Operation == "" {
		p.Operation = "audio.speech"
	}
	if p.Strategy == "" {
		p.Strategy = Strategy
	}
	if p.Currency == "" {
		p.Currency = "USD_TICKS"
	}
	p.EffectiveFrom = p.EffectiveFrom.UTC()
	if p.EffectiveUntil != nil {
		v := p.EffectiveUntil.UTC()
		p.EffectiveUntil = &v
	}
	return p
}
func validPrice(p Price) bool {
	return validID(p.ChannelID, "channel_") && p.Protocol == "openai" && p.Operation == "audio.speech" && p.Strategy == Strategy && p.Currency == "USD_TICKS" && validText(p.Model, 200) && p.CostPerMillion >= 0 && p.SalePerMillion > 0 && !p.EffectiveFrom.IsZero() && (p.EffectiveUntil == nil || p.EffectiveUntil.After(p.EffectiveFrom))
}
func validText(v string, n int) bool { return v != "" && len(v) <= n && v == strings.TrimSpace(v) }
func validID(v, p string) bool       { return strings.HasPrefix(v, p) && len(v) == len(p)+32 }
func same(a, b Price) bool {
	return a.ChannelID == b.ChannelID && a.Protocol == b.Protocol && a.Operation == b.Operation && a.Model == b.Model && a.Strategy == b.Strategy && a.Currency == b.Currency && a.CostPerMillion == b.CostPerMillion && a.SalePerMillion == b.SalePerMillion && a.EffectiveFrom.Equal(b.EffectiveFrom) && ((a.EffectiveUntil == nil && b.EffectiveUntil == nil) || (a.EffectiveUntil != nil && b.EffectiveUntil != nil && a.EffectiveUntil.Equal(*b.EffectiveUntil)))
}

const selectPrice = `SELECT p.id,p.channel_id,p.protocol,p.operation,p.model,p.strategy,p.currency,p.cost_per_million_characters,p.sale_per_million_characters,p.effective_from,p.effective_until FROM audio_speech_prices p`

func scan(p *Price) []any {
	return []any{&p.ID, &p.ChannelID, &p.Protocol, &p.Operation, &p.Model, &p.Strategy, &p.Currency, &p.CostPerMillion, &p.SalePerMillion, &p.EffectiveFrom, &p.EffectiveUntil}
}
func (s *Service) id() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(s.entropy, b); err != nil {
		return "", err
	}
	return "asp_" + hex.EncodeToString(b), nil
}
