// Package pricing implements append-only provider prices and deterministic estimates.
package pricing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const (
	DefaultDimension = "default"
	MaxQuantity      = int64(10)
)

var (
	ErrInvalidPrice        = errors.New("invalid provider price")
	ErrInvalidRequest      = errors.New("invalid price estimate request")
	ErrPriceUnavailable    = errors.New("provider price unavailable")
	ErrMarginViolation     = errors.New("minimum margin violation")
	ErrPublicationConflict = errors.New("price publication conflict")
	ErrPriceOverlap        = errors.New("provider price interval overlaps")
)

type Channel struct {
	ID       string
	Provider providercredentials.ProviderID
	Name     string
	Status   string
}

type Price struct {
	ID             string
	ChannelID      string
	Protocol       string
	Operation      string
	Model          string
	Size           string
	Quality        string
	Currency       string
	UnitCost       int64
	UnitSale       int64
	EffectiveFrom  time.Time
	EffectiveUntil *time.Time
}

type Request struct {
	Protocol  string
	Operation string
	Model     string
	ChannelID string
	Quantity  int64
	Size      string
	Quality   string
	At        time.Time
}

type Estimate struct {
	PriceID       string
	ChannelID     string
	Currency      string
	Quantity      int64
	EstimatedCost int64
	MaximumSale   int64
	EvaluatedAt   time.Time
}

type Service struct {
	pool             *pgxpool.Pool
	minimumMarginBPS int64
	entropy          io.Reader
	now              func() time.Time
}

func NewService(pool *pgxpool.Pool, minimumMarginBPS int64) (*Service, error) {
	return newService(pool, minimumMarginBPS, rand.Reader, time.Now)
}

func newService(pool *pgxpool.Pool, minimumMarginBPS int64, entropy io.Reader, now func() time.Time) (*Service, error) {
	if pool == nil || minimumMarginBPS < 0 || minimumMarginBPS > 10_000 || entropy == nil || now == nil {
		return nil, ErrInvalidPrice
	}
	return &Service{pool: pool, minimumMarginBPS: minimumMarginBPS, entropy: entropy, now: now}, nil
}

func (service *Service) RegisterChannel(ctx context.Context, provider providercredentials.ProviderID, name string) (Channel, error) {
	if !validProvider(provider) || !validText(name, 120) {
		return Channel{}, ErrInvalidPrice
	}
	id, err := service.id("channel_")
	if err != nil {
		return Channel{}, err
	}
	channel := Channel{ID: id, Provider: provider, Name: name, Status: "active"}
	_, err = service.pool.Exec(ctx, `INSERT INTO provider_channels(id,provider,name,status) VALUES($1,$2,$3,$4)`, channel.ID, channel.Provider, channel.Name, channel.Status)
	return channel, err
}

func (service *Service) SetChannelStatus(ctx context.Context, channelID, status string) error {
	if !validID(channelID, "channel_") || (status != "active" && status != "disabled") {
		return ErrInvalidPrice
	}
	result, err := service.pool.Exec(ctx, `UPDATE provider_channels SET status=$2,updated_at=now() WHERE id=$1`, channelID, status)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrPriceUnavailable
	}
	return nil
}

func (service *Service) Publish(ctx context.Context, price Price, publicationKey string) (Price, error) {
	price = canonicalPrice(price)
	if !validPrice(price) || !validText(publicationKey, 200) {
		return Price{}, ErrInvalidPrice
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Price{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "price-publication:"+publicationKey); err != nil {
		return Price{}, err
	}
	existing, found, err := loadPublication(ctx, tx, publicationKey)
	if err != nil {
		return Price{}, err
	}
	if found {
		if !samePrice(existing, price) {
			return Price{}, ErrPublicationConflict
		}
		return existing, nil
	}
	price.ID, err = service.id("price_")
	if err != nil {
		return Price{}, err
	}
	publicationID, err := service.id("publication_")
	if err != nil {
		return Price{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO provider_prices(id,channel_id,protocol,operation,model,size,quality,currency,unit_cost,unit_sale,effective_from,effective_until)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, price.ID, price.ChannelID, price.Protocol, price.Operation, price.Model, price.Size, price.Quality, price.Currency, price.UnitCost, price.UnitSale, price.EffectiveFrom, price.EffectiveUntil)
	if err != nil {
		return Price{}, classifyDatabaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO price_publications(id,publication_key,price_id) VALUES($1,$2,$3)`, publicationID, publicationKey, price.ID); err != nil {
		return Price{}, classifyDatabaseError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Price{}, err
	}
	return price, nil
}

func (service *Service) Estimate(ctx context.Context, request Request) (Estimate, error) {
	return service.estimate(ctx, service.pool, request)
}

// EstimateInTx selects a price using a caller-owned transaction so a larger
// business operation can retain one database snapshot and connection.
func (service *Service) EstimateInTx(ctx context.Context, tx pgx.Tx, request Request) (Estimate, error) {
	if tx == nil {
		return Estimate{}, ErrInvalidRequest
	}
	return service.estimate(ctx, tx, request)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (service *Service) estimate(ctx context.Context, query rowQuerier, request Request) (Estimate, error) {
	request = canonicalRequest(request)
	if !validRequest(request) {
		return Estimate{}, ErrInvalidRequest
	}
	if request.At.IsZero() {
		request.At = service.now().UTC()
	}
	var price Price
	err := query.QueryRow(ctx, `SELECT p.id,p.channel_id,p.protocol,p.operation,p.model,p.size,p.quality,p.currency,p.unit_cost,p.unit_sale,p.effective_from,p.effective_until
		FROM provider_prices p JOIN provider_channels c ON c.id=p.channel_id
		WHERE p.channel_id=$1 AND p.protocol=$2 AND p.operation=$3 AND p.model=$4 AND p.size=$5 AND p.quality=$6
		AND c.status='active' AND p.effective_from <= $7 AND (p.effective_until IS NULL OR p.effective_until > $7)`, request.ChannelID, request.Protocol, request.Operation, request.Model, request.Size, request.Quality, request.At).
		Scan(&price.ID, &price.ChannelID, &price.Protocol, &price.Operation, &price.Model, &price.Size, &price.Quality, &price.Currency, &price.UnitCost, &price.UnitSale, &price.EffectiveFrom, &price.EffectiveUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Estimate{}, ErrPriceUnavailable
	}
	if err != nil {
		return Estimate{}, err
	}
	if !marginAllowed(price.UnitCost, price.UnitSale, service.minimumMarginBPS) {
		return Estimate{}, ErrMarginViolation
	}
	cost, ok := multiply(price.UnitCost, request.Quantity)
	if !ok {
		return Estimate{}, ErrInvalidRequest
	}
	sale, ok := multiply(price.UnitSale, request.Quantity)
	if !ok {
		return Estimate{}, ErrInvalidRequest
	}
	return Estimate{PriceID: price.ID, ChannelID: price.ChannelID, Currency: price.Currency, Quantity: request.Quantity, EstimatedCost: cost, MaximumSale: sale, EvaluatedAt: request.At}, nil
}

func canonicalPrice(price Price) Price {
	price.ID = ""
	price.Size = canonicalDimension(price.Size)
	price.Quality = canonicalDimension(price.Quality)
	price.Currency = ledger.Currency
	price.EffectiveFrom = price.EffectiveFrom.UTC().Truncate(time.Microsecond)
	if price.EffectiveUntil != nil {
		value := price.EffectiveUntil.UTC().Truncate(time.Microsecond)
		price.EffectiveUntil = &value
	}
	return price
}

func canonicalRequest(request Request) Request {
	request.Size = canonicalDimension(request.Size)
	request.Quality = canonicalDimension(request.Quality)
	if request.Quantity == 0 {
		request.Quantity = 1
	}
	if !request.At.IsZero() {
		request.At = request.At.UTC()
	}
	return request
}

func canonicalDimension(value string) string {
	if value == "" {
		return DefaultDimension
	}
	return value
}

func validPrice(price Price) bool {
	return validID(price.ChannelID, "channel_") && validProtocol(price.Protocol) && validOperation(price.Operation) && validText(price.Model, 200) && validText(price.Size, 80) && validText(price.Quality, 80) && price.Currency == ledger.Currency && price.UnitCost >= 0 && price.UnitSale > 0 && price.UnitSale >= price.UnitCost && !price.EffectiveFrom.IsZero() && (price.EffectiveUntil == nil || price.EffectiveUntil.After(price.EffectiveFrom))
}

func validRequest(request Request) bool {
	return validID(request.ChannelID, "channel_") && validProtocol(request.Protocol) && validOperation(request.Operation) && validText(request.Model, 200) && validText(request.Size, 80) && validText(request.Quality, 80) && request.Quantity >= 1 && request.Quantity <= MaxQuantity
}

func validProtocol(value string) bool {
	return value == "openai" || value == "gemini" || value == "anthropic" || value == "replicate" || value == "fal"
}

func validOperation(value string) bool { return value == "image.generate" || value == "image.edit" }

func validProvider(provider providercredentials.ProviderID) bool {
	switch provider {
	case providercredentials.Google, providercredentials.OpenAI, providercredentials.XAI:
		return true
	default:
		return provider == "replicate" || provider == "fal" || provider == "stability"
	}
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}

func validID(value, prefix string) bool {
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func marginAllowed(cost, sale, minimumBPS int64) bool {
	if cost < 0 || sale <= 0 || cost > sale || minimumBPS < 0 || minimumBPS > 10_000 {
		return false
	}
	left := new(big.Int).Mul(big.NewInt(sale-cost), big.NewInt(10_000))
	right := new(big.Int).Mul(big.NewInt(sale), big.NewInt(minimumBPS))
	return left.Cmp(right) >= 0
}

func multiply(unit, quantity int64) (int64, bool) {
	if unit < 0 || quantity < 1 || (unit != 0 && quantity > math.MaxInt64/unit) {
		return 0, false
	}
	return unit * quantity, true
}

func loadPublication(ctx context.Context, tx pgx.Tx, key string) (Price, bool, error) {
	var price Price
	err := tx.QueryRow(ctx, `SELECT p.id,p.channel_id,p.protocol,p.operation,p.model,p.size,p.quality,p.currency,p.unit_cost,p.unit_sale,p.effective_from,p.effective_until
		FROM price_publications publication JOIN provider_prices p ON p.id=publication.price_id WHERE publication.publication_key=$1`, key).
		Scan(&price.ID, &price.ChannelID, &price.Protocol, &price.Operation, &price.Model, &price.Size, &price.Quality, &price.Currency, &price.UnitCost, &price.UnitSale, &price.EffectiveFrom, &price.EffectiveUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Price{}, false, nil
	}
	return price, err == nil, err
}

func samePrice(left, right Price) bool {
	return left.ChannelID == right.ChannelID && left.Protocol == right.Protocol && left.Operation == right.Operation && left.Model == right.Model && left.Size == right.Size && left.Quality == right.Quality && left.Currency == right.Currency && left.UnitCost == right.UnitCost && left.UnitSale == right.UnitSale && left.EffectiveFrom.Equal(right.EffectiveFrom) && equalTime(left.EffectiveUntil, right.EffectiveUntil)
}

func equalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func classifyDatabaseError(err error) error {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) {
		return err
	}
	switch postgresError.Code {
	case "23P01":
		return ErrPriceOverlap
	case "23505":
		return ErrPublicationConflict
	case "23503", "23514":
		return ErrInvalidPrice
	default:
		return err
	}
}

func (service *Service) id(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(service.entropy, value); err != nil {
		return "", fmt.Errorf("generate pricing id: %w", err)
	}
	return prefix + hex.EncodeToString(value), nil
}
