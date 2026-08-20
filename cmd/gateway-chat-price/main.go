// Command gateway-chat-price publishes immutable OpenAI Chat token prices.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("gateway-chat-price", flag.ContinueOnError)
	flags.SetOutput(stderr)
	channel := flags.String("channel-id", "", "Provider channel ID")
	model := flags.String("model", "", "logical OpenAI Chat model")
	publication := flags.String("publication-key", "", "idempotent publication key")
	effective := flags.String("effective-from", "", "RFC3339 effective time")
	inputCost := flags.String("input-cost", "", "input cost per million tokens")
	inputSale := flags.String("input-sale", "", "input sale per million tokens")
	cachedCost := flags.String("cached-input-cost", "", "cached input cost per million tokens")
	cachedSale := flags.String("cached-input-sale", "", "cached input sale per million tokens")
	outputCost := flags.String("output-cost", "", "output cost per million tokens")
	outputSale := flags.String("output-sale", "", "output sale per million tokens")
	if flags.Parse(args) != nil {
		return 2
	}
	parse := func(raw string) (int64, bool) {
		v, err := strconv.ParseInt(raw, 10, 64)
		return v, err == nil && v >= 0
	}
	ic, ok1 := parse(*inputCost)
	is, ok2 := parse(*inputSale)
	cc, ok3 := parse(*cachedCost)
	cs, ok4 := parse(*cachedSale)
	oc, ok5 := parse(*outputCost)
	osale, ok6 := parse(*outputSale)
	at, err := time.Parse(time.RFC3339, *effective)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || is == 0 || cs == 0 || osale == 0 || err != nil || *channel == "" || *model == "" || *publication == "" {
		_, _ = fmt.Fprintln(stderr, "chat price arguments are invalid")
		return 2
	}
	url := getenv("GATEWAY_DATABASE_URL")
	if url == "" {
		_, _ = fmt.Fprintln(stderr, "GATEWAY_DATABASE_URL is required")
		return 1
	}
	margin, err := strconv.ParseInt(getenv("GATEWAY_MINIMUM_MARGIN_BPS"), 10, 64)
	if err != nil {
		margin = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "database unavailable")
		return 1
	}
	defer pool.Close()
	if database.Migrate(ctx, pool) != nil {
		_, _ = fmt.Fprintln(stderr, "database migration failed")
		return 1
	}
	service, err := chatpricing.New(pool, margin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chat pricing configuration invalid")
		return 1
	}
	price, err := service.Publish(ctx, chatpricing.Price{ChannelID: *channel, Model: *model, EffectiveFrom: at, Rates: chatpricing.Rates{InputCost: ic, InputSale: is, CachedInputCost: cc, CachedInputSale: cs, OutputCost: oc, OutputSale: osale}}, *publication)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chat price publication failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, price.ID)
	return 0
}
