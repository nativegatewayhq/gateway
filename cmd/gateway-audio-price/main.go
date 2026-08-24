// Command gateway-audio-price publishes and inspects immutable Speech and
// transcription and translation prices.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	f := flag.NewFlagSet("gateway-audio-price", flag.ContinueOnError)
	f.SetOutput(stderr)
	operation := f.String("operation", "speech", "speech, transcription, or translation")
	action := f.String("action", "publish", "publish, estimate, or inspect")
	channel := f.String("channel-id", "", "Provider channel ID")
	model := f.String("model", "", "Audio model")
	key := f.String("publication-key", "", "idempotent publication key")
	effective := f.String("effective-from", "", "RFC3339 effective time")
	cost := f.String("cost", "", "cost per million characters")
	sale := f.String("sale", "", "sale per million characters")
	strategy := f.String("strategy", "", "transcription token or duration strategy")
	costInput := f.String("cost-input", "", "cost per million transcription input tokens")
	costOutput := f.String("cost-output", "", "cost per million transcription output tokens")
	saleInput := f.String("sale-input", "", "sale per million transcription input tokens")
	saleOutput := f.String("sale-output", "", "sale per million transcription output tokens")
	maximumInput := f.String("maximum-input-tokens", "", "maximum transcription input tokens")
	maximumOutput := f.String("maximum-output-tokens", "", "maximum transcription output tokens")
	costMinute := f.String("cost-per-minute", "", "transcription cost per minute")
	saleMinute := f.String("sale-per-minute", "", "transcription sale per minute")
	maximumDuration := f.String("maximum-duration-milliseconds", "", "maximum transcription duration")
	if f.Parse(args) != nil {
		return 2
	}
	if (*operation != "speech" && *operation != "transcription" && *operation != "translation") || (*action != "publish" && *action != "estimate" && *action != "inspect") || *channel == "" || *model == "" {
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
	if *operation == "transcription" {
		return runTranscription(service, ctx, stdout, stderr, *action, *channel, *model, *key, *effective, *strategy, *costInput, *costOutput, *saleInput, *saleOutput, *maximumInput, *maximumOutput, *costMinute, *saleMinute, *maximumDuration)
	}
	if *operation == "translation" {
		return runTranslation(service, ctx, stdout, stderr, *action, *channel, *model, *key, *effective, *costMinute, *saleMinute, *maximumDuration)
	}
	if *action != "publish" {
		fmt.Fprintln(stderr, "speech only supports publish")
		return 2
	}
	c, e1 := strconv.ParseInt(*cost, 10, 64)
	saleValue, e2 := strconv.ParseInt(*sale, 10, 64)
	at, e3 := time.Parse(time.RFC3339, *effective)
	if e1 != nil || e2 != nil || e3 != nil || c < 0 || saleValue < 1 || *key == "" {
		fmt.Fprintln(stderr, "audio price arguments are invalid")
		return 2
	}
	p, err := service.Publish(ctx, audiopricing.Price{ChannelID: *channel, Model: *model, CostPerMillion: c, SalePerMillion: saleValue, EffectiveFrom: at}, *key)
	if err != nil {
		fmt.Fprintln(stderr, "audio price publication failed")
		return 1
	}
	fmt.Fprintln(stdout, p.ID)
	return 0
}

func runTranslation(service *audiopricing.Service, ctx context.Context, stdout, stderr io.Writer, action, channel, model, key, effective, costMinute, saleMinute, maximumDuration string) int {
	if action != "publish" {
		estimate, err := service.EstimateTranslation(ctx, audiopricing.TranslationPriceRequest{ChannelID: channel, Model: model})
		if err != nil {
			fmt.Fprintln(stderr, "translation price unavailable")
			return 1
		}
		if action == "inspect" {
			fmt.Fprintf(stdout, "%s %s %d %d\n", estimate.Price.ID, estimate.Price.Strategy, estimate.MaximumCost, estimate.MaximumSale)
		} else {
			fmt.Fprintf(stdout, "%d %d\n", estimate.MaximumCost, estimate.MaximumSale)
		}
		return 0
	}
	cost, costErr := strconv.ParseInt(costMinute, 10, 64)
	sale, saleErr := strconv.ParseInt(saleMinute, 10, 64)
	maximum, maximumErr := strconv.ParseInt(maximumDuration, 10, 64)
	at, timeErr := time.Parse(time.RFC3339, effective)
	if key == "" || costErr != nil || saleErr != nil || maximumErr != nil || timeErr != nil || cost < 0 || sale < 1 || maximum < 1 {
		fmt.Fprintln(stderr, "translation price arguments are invalid")
		return 2
	}
	published, err := service.PublishTranslation(ctx, audiopricing.TranslationPrice{ChannelID: channel, Model: model, CostPerMinute: cost, SalePerMinute: sale, MaximumDurationMilliseconds: maximum, EffectiveFrom: at}, key)
	if err != nil {
		fmt.Fprintln(stderr, "translation price publication failed")
		return 1
	}
	fmt.Fprintln(stdout, published.ID)
	return 0
}

func runTranscription(service *audiopricing.Service, ctx context.Context, stdout, stderr io.Writer, action, channel, model, key, effective, strategy string, values ...string) int {
	if action != "publish" {
		estimate, err := service.EstimateTranscription(ctx, audiopricing.TranscriptionPriceRequest{ChannelID: channel, Model: model})
		if err != nil {
			fmt.Fprintln(stderr, "transcription price unavailable")
			return 1
		}
		if action == "inspect" {
			fmt.Fprintf(stdout, "%s %s %d %d\n", estimate.Price.ID, estimate.Price.Strategy, estimate.MaximumCost, estimate.MaximumSale)
		} else {
			fmt.Fprintf(stdout, "%d %d\n", estimate.MaximumCost, estimate.MaximumSale)
		}
		return 0
	}
	if key == "" || len(values) != 9 {
		fmt.Fprintln(stderr, "transcription price arguments are invalid")
		return 2
	}
	at, err := time.Parse(time.RFC3339, effective)
	if err != nil {
		fmt.Fprintln(stderr, "transcription price arguments are invalid")
		return 2
	}
	parsed := make([]int64, len(values))
	for i, value := range values {
		if value == "" {
			continue
		}
		parsed[i], err = strconv.ParseInt(value, 10, 64)
		if err != nil || parsed[i] < 0 {
			fmt.Fprintln(stderr, "transcription price arguments are invalid")
			return 2
		}
	}
	price := audiopricing.TranscriptionPrice{ChannelID: channel, Model: model, Strategy: strategy, CostInputPerMillion: parsed[0], CostOutputPerMillion: parsed[1], SaleInputPerMillion: parsed[2], SaleOutputPerMillion: parsed[3], MaximumInputTokens: parsed[4], MaximumOutputTokens: parsed[5], CostPerMinute: parsed[6], SalePerMinute: parsed[7], MaximumDurationMilliseconds: parsed[8], EffectiveFrom: at}
	published, err := service.PublishTranscription(ctx, price, key)
	if err != nil {
		fmt.Fprintln(stderr, "transcription price publication failed")
		return 1
	}
	fmt.Fprintln(stdout, published.ID)
	return 0
}
