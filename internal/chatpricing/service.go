// Package chatpricing publishes immutable per-million-token prices and computes bounded estimates.
package chatpricing

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
	"github.com/nativegatewayhq/gateway/internal/ledger"
)

const Million int64 = 1_000_000

var (
	ErrInvalid     = errors.New("invalid chat token price")
	ErrUnavailable = errors.New("chat token price unavailable")
	ErrConflict    = errors.New("chat token price publication conflict")
	ErrMargin      = errors.New("chat token price margin violation")
)

type Rates struct {
	InputCost, InputSale, CachedInputCost, CachedInputSale int64
	CacheWriteCost, CacheWriteSale, OutputCost, OutputSale int64
}
type Price struct {
	ID, ChannelID, Protocol, Operation, Model, Currency string
	Rates                                               Rates
	EffectiveFrom                                       time.Time
	EffectiveUntil                                      *time.Time
}
type Request struct {
	ChannelID, Protocol, Operation, Model   string
	MaximumInputTokens, MaximumOutputTokens int64
	At                                      time.Time
}
type Estimate struct {
	Price                      Price
	EstimatedCost, MaximumSale int64
}
type Usage struct {
	PromptTokens, CachedInputTokens, CacheWriteTokens, CompletionTokens int64
	ToolUsePromptTokens, ThoughtsTokens                                 int64
}
type Amounts struct{ Cost, Sale int64 }
type Service struct {
	pool             *pgxpool.Pool
	minimumMarginBPS int64
	entropy          io.Reader
	now              func() time.Time
}

func New(pool *pgxpool.Pool, minimumMarginBPS int64) (*Service, error) {
	if pool == nil || minimumMarginBPS < 0 || minimumMarginBPS > 10000 {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, minimumMarginBPS: minimumMarginBPS, entropy: rand.Reader, now: time.Now}, nil
}
func (s *Service) Publish(ctx context.Context, p Price, key string) (Price, error) {
	p = canonical(p)
	if !validPrice(p) || !validText(key, 200) {
		return Price{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Price{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "token-price:"+p.Protocol+":"+p.Operation+":"+key); err != nil {
		return Price{}, err
	}
	var existing Price
	existing.Rates = Rates{}
	err = tx.QueryRow(ctx, selectPrice+` JOIN chat_token_price_publications pub ON pub.price_id=p.id WHERE pub.protocol=$1 AND pub.operation=$2 AND pub.publication_key=$3`, p.Protocol, p.Operation, key).Scan(scanPrice(&existing)...)
	if err == nil {
		if !samePrice(existing, p) {
			return Price{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Price{}, err
	}
	p.ID, err = s.id("ctp_")
	if err != nil {
		return Price{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_token_prices(id,channel_id,protocol,operation,model,currency,input_cost_per_million,input_sale_per_million,cached_input_cost_per_million,cached_input_sale_per_million,cache_write_cost_per_million,cache_write_sale_per_million,output_cost_per_million,output_sale_per_million,effective_from,effective_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, p.ID, p.ChannelID, p.Protocol, p.Operation, p.Model, p.Currency, p.Rates.InputCost, p.Rates.InputSale, p.Rates.CachedInputCost, p.Rates.CachedInputSale, p.Rates.CacheWriteCost, p.Rates.CacheWriteSale, p.Rates.OutputCost, p.Rates.OutputSale, p.EffectiveFrom, p.EffectiveUntil)
	if err != nil {
		return Price{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chat_token_price_publications(protocol,operation,publication_key,price_id) VALUES($1,$2,$3,$4)`, p.Protocol, p.Operation, key, p.ID); err != nil {
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

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Service) estimate(ctx context.Context, q rowQuerier, r Request) (Estimate, error) {
	if r.Operation == "" {
		r.Operation = "chat.completions"
	}
	if r.Protocol == "" {
		r.Protocol = "openai"
	}
	if !validProtocolOperation(r.Protocol, r.Operation) || !validID(r.ChannelID, "channel_") || !validText(r.Model, 200) || r.MaximumInputTokens < 1 || r.MaximumOutputTokens < 1 {
		return Estimate{}, ErrInvalid
	}
	if r.At.IsZero() {
		r.At = s.now().UTC()
	}
	var p Price
	err := q.QueryRow(ctx, selectPrice+` JOIN provider_channels c ON c.id=p.channel_id WHERE p.channel_id=$1 AND p.protocol=$2 AND p.operation=$3 AND p.model=$4 AND c.status='active' AND p.effective_from<=$5 AND (p.effective_until IS NULL OR p.effective_until>$5)`, r.ChannelID, r.Protocol, r.Operation, r.Model, r.At).Scan(scanPrice(&p)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Estimate{}, ErrUnavailable
	}
	if err != nil {
		return Estimate{}, err
	}
	if !ratesMargin(p.Rates, s.minimumMarginBPS) {
		return Estimate{}, ErrMargin
	}
	inputCost := max(p.Rates.InputCost, p.Rates.CachedInputCost, p.Rates.CacheWriteCost)
	inputSale := max(p.Rates.InputSale, p.Rates.CachedInputSale, p.Rates.CacheWriteSale)
	cost, ok := sumTokenAmounts(r.MaximumInputTokens, inputCost, r.MaximumOutputTokens, p.Rates.OutputCost)
	if !ok {
		return Estimate{}, ErrInvalid
	}
	sale, ok := sumTokenAmounts(r.MaximumInputTokens, inputSale, r.MaximumOutputTokens, p.Rates.OutputSale)
	if !ok || sale < 1 {
		return Estimate{}, ErrInvalid
	}
	return Estimate{Price: p, EstimatedCost: cost, MaximumSale: sale}, nil
}
func Calculate(r Rates, u Usage) (Amounts, error) {
	if u.PromptTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteTokens < 0 || u.CachedInputTokens > u.PromptTokens-u.CacheWriteTokens || u.CompletionTokens < 0 || u.ToolUsePromptTokens < 0 || u.ToolUsePromptTokens > u.PromptTokens || u.ThoughtsTokens < 0 || u.ThoughtsTokens > u.CompletionTokens {
		return Amounts{}, ErrInvalid
	}
	regular := u.PromptTokens - u.CachedInputTokens - u.CacheWriteTokens
	cost, ok := sum4(regular, r.InputCost, u.CachedInputTokens, r.CachedInputCost, u.CacheWriteTokens, r.CacheWriteCost, u.CompletionTokens, r.OutputCost)
	if !ok {
		return Amounts{}, ErrInvalid
	}
	sale, ok := sum4(regular, r.InputSale, u.CachedInputTokens, r.CachedInputSale, u.CacheWriteTokens, r.CacheWriteSale, u.CompletionTokens, r.OutputSale)
	if !ok || sale < 1 {
		return Amounts{}, ErrInvalid
	}
	return Amounts{cost, sale}, nil
}
func tokenAmount(tokens, rate int64) (int64, bool) {
	if tokens < 0 || rate < 0 {
		return 0, false
	}
	if tokens == 0 || rate == 0 {
		return 0, true
	}
	value := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate))
	value.Add(value, big.NewInt(Million-1))
	value.Div(value, big.NewInt(Million))
	if !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}
func sumTokenAmounts(a, ar, b, br int64) (int64, bool) {
	x, ok := tokenAmount(a, ar)
	if !ok {
		return 0, false
	}
	y, ok := tokenAmount(b, br)
	if !ok || x > int64(^uint64(0)>>1)-y {
		return 0, false
	}
	return x + y, true
}
func sum3(a, ar, b, br, c, cr int64) (int64, bool) {
	x, ok := sumTokenAmounts(a, ar, b, br)
	if !ok {
		return 0, false
	}
	y, ok := tokenAmount(c, cr)
	if !ok || x > int64(^uint64(0)>>1)-y {
		return 0, false
	}
	return x + y, true
}
func sum4(a, ar, b, br, c, cr, d, dr int64) (int64, bool) {
	x, ok := sum3(a, ar, b, br, c, cr)
	if !ok {
		return 0, false
	}
	y, ok := tokenAmount(d, dr)
	if !ok || x > int64(^uint64(0)>>1)-y {
		return 0, false
	}
	return x + y, true
}
func ratesMargin(r Rates, bps int64) bool {
	return margin(r.InputCost, r.InputSale, bps) && margin(r.CachedInputCost, r.CachedInputSale, bps) && margin(r.CacheWriteCost, r.CacheWriteSale, bps) && margin(r.OutputCost, r.OutputSale, bps)
}
func margin(cost, sale, bps int64) bool {
	left := new(big.Int).Mul(big.NewInt(sale), big.NewInt(10000))
	right := new(big.Int).Mul(big.NewInt(cost), big.NewInt(10000+bps))
	return left.Cmp(right) >= 0
}

const selectPrice = `SELECT p.id,p.channel_id,p.protocol,p.operation,p.model,p.currency,p.input_cost_per_million,p.input_sale_per_million,p.cached_input_cost_per_million,p.cached_input_sale_per_million,p.cache_write_cost_per_million,p.cache_write_sale_per_million,p.output_cost_per_million,p.output_sale_per_million,p.effective_from,p.effective_until FROM chat_token_prices p`

func scanPrice(p *Price) []any {
	return []any{&p.ID, &p.ChannelID, &p.Protocol, &p.Operation, &p.Model, &p.Currency, &p.Rates.InputCost, &p.Rates.InputSale, &p.Rates.CachedInputCost, &p.Rates.CachedInputSale, &p.Rates.CacheWriteCost, &p.Rates.CacheWriteSale, &p.Rates.OutputCost, &p.Rates.OutputSale, &p.EffectiveFrom, &p.EffectiveUntil}
}
func canonical(p Price) Price {
	p.ID = ""
	if p.Protocol == "" {
		p.Protocol = "openai"
	}
	if p.Operation == "" {
		p.Operation = "chat.completions"
	}
	p.Currency = ledger.Currency
	p.EffectiveFrom = p.EffectiveFrom.UTC().Truncate(time.Microsecond)
	if p.EffectiveUntil != nil {
		v := p.EffectiveUntil.UTC().Truncate(time.Microsecond)
		p.EffectiveUntil = &v
	}
	return p
}
func validPrice(p Price) bool {
	return validProtocolOperation(p.Protocol, p.Operation) && validID(p.ChannelID, "channel_") && validText(p.Model, 200) && p.Currency == ledger.Currency && p.EffectiveFrom != time.Time{} && (p.EffectiveUntil == nil || p.EffectiveUntil.After(p.EffectiveFrom)) && p.Rates.InputCost >= 0 && p.Rates.InputSale > 0 && p.Rates.CachedInputCost >= 0 && p.Rates.CachedInputSale > 0 && p.Rates.CacheWriteCost >= 0 && p.Rates.CacheWriteSale >= 0 && p.Rates.OutputCost >= 0 && p.Rates.OutputSale > 0 && p.Rates.InputSale >= p.Rates.InputCost && p.Rates.CachedInputSale >= p.Rates.CachedInputCost && p.Rates.CacheWriteSale >= p.Rates.CacheWriteCost && p.Rates.OutputSale >= p.Rates.OutputCost && (p.Protocol != "anthropic" || p.Rates.CacheWriteSale > 0)
}
func samePrice(a, b Price) bool {
	return a.ChannelID == b.ChannelID && a.Protocol == b.Protocol && a.Operation == b.Operation && a.Model == b.Model && a.Rates == b.Rates && a.EffectiveFrom.Equal(b.EffectiveFrom) && ((a.EffectiveUntil == nil && b.EffectiveUntil == nil) || (a.EffectiveUntil != nil && b.EffectiveUntil != nil && a.EffectiveUntil.Equal(*b.EffectiveUntil)))
}
func validProtocolOperation(protocol, operation string) bool {
	return (protocol == "openai" && (operation == "chat.completions" || operation == "responses.create")) || (protocol == "gemini" && operation == "chat.completions") || (protocol == "anthropic" && operation == "messages.create")
}
func validText(v string, n int) bool { return v != "" && len(v) <= n && strings.TrimSpace(v) == v }
func validID(v, prefix string) bool {
	if !strings.HasPrefix(v, prefix) || len(v) != len(prefix)+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(v, prefix))
	return err == nil
}
func (s *Service) id(prefix string) (string, error) {
	v := make([]byte, 16)
	if _, err := io.ReadFull(s.entropy, v); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(v), nil
}
