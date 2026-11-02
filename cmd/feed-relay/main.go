// Command feed-relay sends new RSS, Atom and JSON Feed items (including
// RSSHub routes) to Hibira as realtime events, push notifications and
// inbox notifications.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hibira-io/feed-relay/internal/relay"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG", "feed-relay.yaml"), "config file")
	once := flag.Bool("once", false, "fetch every feed once and exit (for cron)")
	check := flag.Bool("check", false, "check the config and exit")
	version := flag.Bool("version", false, "print the version")
	jsonLogs := flag.Bool("json", os.Getenv("LOG_FORMAT") == "json", "log as JSON")
	flag.Parse()

	if *version {
		fmt.Println(relay.Version)
		return
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *jsonLogs {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	cfg, err := relay.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "feed-relay: %s: %v\n", *configPath, err)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("%s: %d feeds, ok\n", *configPath, len(cfg.Feeds))
		return
	}
	store, err := relay.OpenStore(cfg.StateFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "feed-relay: state %s: %v\n", cfg.StateFile, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := relay.New(cfg, store, log)
	if *once {
		err = r.Once(ctx)
	} else {
		err = r.Run(ctx)
	}
	if err != nil {
		log.Error("feed-relay stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
