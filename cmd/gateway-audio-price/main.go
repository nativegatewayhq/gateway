// Command gateway-audio-price publishes immutable character-priced Speech rates.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/database"
	"io"
	"os"
	"strconv"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	f := flag.NewFlagSet("gateway-audio-price", flag.ContinueOnError)
	f.SetOutput(stderr)
	channel := f.String("channel-id", "", "Provider channel ID")
	model := f.String("model", "", "Speech model")
	key := f.String("publication-key", "", "idempotent publication key")
	effective := f.String("effective-from", "", "RFC3339 effective time")
	cost := f.String("cost", "", "cost per million characters")
	sale := f.String("sale", "", "sale per million characters")
	if f.Parse(args) != nil {
		return 2
	}
	c, e1 := strconv.ParseInt(*cost, 10, 64)
	s, e2 := strconv.ParseInt(*sale, 10, 64)
	at, e3 := time.Parse(time.RFC3339, *effective)
	if e1 != nil || e2 != nil || e3 != nil || c < 0 || s < 1 || *channel == "" || *model == "" || *key == "" {
		fmt.Fprintln(stderr, "audio price arguments are invalid")
		return 2
	}
	url := getenv("GATEWAY_DATABASE_URL")
	if url == "" {
		fmt.Fprintln(stderr, "GATEWAY_DATABASE_URL is required")
		return 1
	}
	margin, _ := strconv.ParseInt(getenv("GATEWAY_MINIMUM_MARGIN_BPS"), 10, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		fmt.Fprintln(stderr, "database unavailable")
		return 1
	}
	defer pool.Close()
	if database.Migrate(ctx, pool) != nil {
		fmt.Fprintln(stderr, "database migration failed")
		return 1
	}
	service, err := audiopricing.New(pool, margin)
	if err != nil {
		fmt.Fprintln(stderr, "audio pricing configuration invalid")
		return 1
	}
	p, err := service.Publish(ctx, audiopricing.Price{ChannelID: *channel, Model: *model, CostPerMillion: c, SalePerMillion: s, EffectiveFrom: at}, *key)
	if err != nil {
		fmt.Fprintln(stderr, "audio price publication failed")
		return 1
	}
	fmt.Fprintln(stdout, p.ID)
	return 0
}
