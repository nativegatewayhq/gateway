// Command gateway-video-price publishes immutable Runway video credit prices.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"io"
	"os"
	"strconv"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }
func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("gateway-video-price", flag.ContinueOnError)
	flags.SetOutput(stderr)
	channel := flags.String("channel-id", "", "Runway channel ID")
	model := flags.String("model", "", "logical model")
	kind := flags.String("task-kind", "", "text_to_video or image_to_video")
	quality := flags.String("quality", "", "exact ratio/audio selector")
	publication := flags.String("publication-key", "", "idempotent publication key")
	effective := flags.String("effective-from", "", "RFC3339 effective time")
	perSecond := flags.String("credits-per-second-micros", "0", "Provider microcredits per second")
	fixed := flags.String("fixed-credits-micros", "0", "fixed Provider microcredits")
	minimum := flags.String("minimum-credits-micros", "0", "minimum Provider microcredits")
	unitCost := flags.String("credit-cost", "", "USD_TICKS cost per Provider credit")
	unitSale := flags.String("credit-sale", "", "USD_TICKS sale per Provider credit")
	validateOnly := flags.Bool("validate-only", false, "validate without database publication")
	if flags.Parse(args) != nil {
		return 2
	}
	parse := func(raw string) (int64, bool) {
		v, err := strconv.ParseInt(raw, 10, 64)
		return v, err == nil && v >= 0
	}
	ps, ok1 := parse(*perSecond)
	fx, ok2 := parse(*fixed)
	mn, ok3 := parse(*minimum)
	cost, ok4 := parse(*unitCost)
	sale, ok5 := parse(*unitSale)
	at, err := time.Parse(time.RFC3339, *effective)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || sale <= 0 || cost > sale || (ps == 0 && fx == 0) || (*kind != "text_to_video" && *kind != "image_to_video") || *channel == "" || *model == "" || *quality == "" || *publication == "" || err != nil {
		_, _ = fmt.Fprintln(stderr, "video price arguments are invalid")
		return 2
	}
	if *validateOnly {
		_, _ = fmt.Fprintln(stdout, "valid")
		return 0
	}
	url := getenv("GATEWAY_DATABASE_URL")
	if url == "" {
		_, _ = fmt.Fprintln(stderr, "GATEWAY_DATABASE_URL is required")
		return 1
	}
	margin, _ := strconv.ParseInt(getenv("GATEWAY_MINIMUM_MARGIN_BPS"), 10, 64)
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
	service, err := pricing.NewService(pool, margin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pricing configuration invalid")
		return 1
	}
	value, err := service.PublishVideo(ctx, pricing.VideoPrice{Price: pricing.Price{ChannelID: *channel, Protocol: "runway", Operation: "video.generate", Model: *model, Size: *kind, Quality: *quality, UnitCost: cost, UnitSale: sale, EffectiveFrom: at}, CreditsPerSecondMicros: ps, FixedCreditsMicros: fx, MinimumCreditsMicros: mn}, *publication)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "video price publication failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, value.ID)
	return 0
}
