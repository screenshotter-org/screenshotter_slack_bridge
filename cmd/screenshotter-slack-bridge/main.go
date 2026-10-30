// Command screenshotter-slack-bridge connects to Slack over Socket Mode and
// renders screenshotter links (screen/{id}) as Slack remote files, so each link
// unfurls into a card showing the screenshot. The images are fetched from a
// screenshotter server that need not be reachable from the public internet.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/bridge"
	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/config"
	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/version"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to the TOML configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	opts := []slack.Option{
		slack.OptionAppLevelToken(cfg.AppToken),
		slack.OptionDebug(cfg.Debug),
	}
	if cfg.SlackAPIURL != "" {
		opts = append(opts, slack.OptionAPIURL(cfg.SlackAPIURL))
	}
	api := slack.New(cfg.BotToken, opts...)
	sm := socketmode.New(api, socketmode.OptionDebug(cfg.Debug))
	b := bridge.New(cfg, api, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		for evt := range sm.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				logger.Println("connecting to Slack via Socket Mode...")
			case socketmode.EventTypeConnected:
				logger.Println("connected to Slack")
			case socketmode.EventTypeInvalidAuth:
				logger.Fatal("invalid Slack credentials; check bot_token and app_token")
			case socketmode.EventTypeConnectionError:
				logger.Printf("connection error: %v", evt.Data)
			case socketmode.EventTypeEventsAPI:
				eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				if evt.Request != nil {
					sm.Ack(*evt.Request)
				}
				handleEventsAPI(ctx, b, logger, eventsAPI)
			}
		}
	}()

	logger.Printf("screenshotter slack bridge %s starting (unfurl domains: %v)", version.Version, cfg.UnfurlDomains)
	// RunContext only returns once a reconnection has failed, so today it never
	// returns nil and staticcheck flags the check as always true. Keep testing
	// err: a nil-on-clean-exit return is within its documented contract.
	//lint:ignore SA4023 the nil check follows the contract, not the implementation
	if err := sm.RunContext(ctx); err != nil && ctx.Err() == nil {
		logger.Fatalf("socket mode: %v", err)
	}
	logger.Println("shutting down")
}

func handleEventsAPI(ctx context.Context, b *bridge.Bridge, logger *log.Logger, event slackevents.EventsAPIEvent) {
	if event.Type != slackevents.CallbackEvent {
		return
	}
	switch inner := event.InnerEvent.Data.(type) {
	case *slackevents.LinkSharedEvent:
		// Handle in the background so a slow fetch/upload doesn't block the
		// Socket Mode event loop (we have already Ack'd the request). The
		// bridge bounds its own concurrency internally.
		go b.HandleLinkShared(ctx, event.TeamID, inner)
	default:
		logger.Printf("ignoring event of type %T", inner)
	}
}
