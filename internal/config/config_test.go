package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	t.Setenv("SS_TEST_BOT", "xoxb-bot")
	t.Setenv("SS_TEST_APP", "xapp-app")
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080/"
unfurl_domains = ["Screen.Corp.Example"]
bot_token_env_var = "SS_TEST_BOT"
app_token_env_var = "SS_TEST_APP"
max_dimension = 1600
request_timeout = "10s"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ScreenshotterBaseURL != "http://internal:8080" {
		t.Errorf("trailing slash not trimmed: %q", cfg.ScreenshotterBaseURL)
	}
	if cfg.ScreenshotterFetchBaseURL != "" {
		t.Errorf("screenshotter_fetch_base_url should default to empty, got %q", cfg.ScreenshotterFetchBaseURL)
	}
	if cfg.FetchBaseURL() != "http://internal:8080" {
		t.Errorf("FetchBaseURL should fall back to the base URL, got %q", cfg.FetchBaseURL())
	}
	if cfg.UnfurlDomains[0] != "screen.corp.example" {
		t.Errorf("domain not lowercased: %q", cfg.UnfurlDomains[0])
	}
	if cfg.BotToken != "xoxb-bot" || cfg.AppToken != "xapp-app" {
		t.Errorf("tokens not resolved from env: %q %q", cfg.BotToken, cfg.AppToken)
	}
	if cfg.RequestTimeout.Duration != 10*time.Second {
		t.Errorf("request_timeout not parsed: %v", cfg.RequestTimeout.Duration)
	}
	if cfg.MaxConcurrency != 50 {
		t.Errorf("default max_concurrency should be 50, got %d", cfg.MaxConcurrency)
	}
	if cfg.MaxImageWorkers != 4 {
		t.Errorf("default max_image_workers should be 4, got %d", cfg.MaxImageWorkers)
	}
	if cfg.MaxLinksPerMessage != 5 {
		t.Errorf("default max_links_per_message should be 5, got %d", cfg.MaxLinksPerMessage)
	}
	if cfg.MaxDimension != 1600 {
		t.Errorf("max_dimension not parsed: %d", cfg.MaxDimension)
	}
}

func TestLoadDefaults(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxDimension != 1600 {
		t.Errorf("default max_dimension should be 1600, got %d", cfg.MaxDimension)
	}
	if cfg.PreviewWait.Duration != 8*time.Second {
		t.Errorf("default preview_wait should be 8s, got %v", cfg.PreviewWait.Duration)
	}
	if cfg.CardStyle != CardStyleImage {
		t.Errorf("default card_style should be %q, got %q", CardStyleImage, cfg.CardStyle)
	}
	if cfg.CardFaviconURL != DefaultCardFaviconURL {
		t.Errorf("default card_favicon_url should be %q, got %q", DefaultCardFaviconURL, cfg.CardFaviconURL)
	}
	if cfg.CardCaption != "Open in Screenshotter" {
		t.Errorf("default card_caption should be \"Open in Screenshotter\", got %q", cfg.CardCaption)
	}
	if cfg.CardSourceCaption != "Open source page" {
		t.Errorf("default card_source_caption should be \"Open source page\", got %q", cfg.CardSourceCaption)
	}
	if cfg.CardTitleMaxLength != 90 {
		t.Errorf("default card_title_max_length should be 90, got %d", cfg.CardTitleMaxLength)
	}
}

// minimalConfig is a valid configuration to which a test appends the keys under
// test.
const minimalConfig = `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`

func TestLoadCardSettings(t *testing.T) {
	p := writeConfig(t, minimalConfig+`
card_style = "file"
card_favicon_url = ""
card_caption = "  View screenshot  "
card_source_caption = " Source "
card_title_max_length = 40
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CardStyle != CardStyleFile {
		t.Errorf("card_style not parsed: %q", cfg.CardStyle)
	}
	if cfg.CardFaviconURL != "" {
		t.Errorf("an empty card_favicon_url must disable the icon, got %q", cfg.CardFaviconURL)
	}
	if cfg.CardCaption != "View screenshot" {
		t.Errorf("card_caption not trimmed: %q", cfg.CardCaption)
	}
	if cfg.CardSourceCaption != "Source" {
		t.Errorf("card_source_caption not trimmed: %q", cfg.CardSourceCaption)
	}
	if cfg.CardTitleMaxLength != 40 {
		t.Errorf("card_title_max_length not parsed: %d", cfg.CardTitleMaxLength)
	}
}

func TestLoadImageUploadStyleLiftsTheDefaultMaxDimension(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig+`card_style = "image_upload"`+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CardStyle != CardStyleImageUpload {
		t.Errorf("card_style not parsed: %q", cfg.CardStyle)
	}
	if cfg.MaxDimension != 0 {
		t.Errorf("image_upload should default to no max_dimension cap, got %d", cfg.MaxDimension)
	}

	cfg, err = Load(writeConfig(t, minimalConfig+`card_style = "image_upload"
max_dimension = 2400
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxDimension != 2400 {
		t.Errorf("an explicit max_dimension must be kept, got %d", cfg.MaxDimension)
	}
}

func TestLoadAcceptsEmptyCardCaptions(t *testing.T) {
	// An empty caption drops its footer link rather than falling back to the
	// default.
	p := writeConfig(t, minimalConfig+`
card_caption = ""
card_source_caption = "  "
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CardCaption != "" || cfg.CardSourceCaption != "" {
		t.Errorf("empty captions should stay empty, got %q and %q", cfg.CardCaption, cfg.CardSourceCaption)
	}
}

func TestLoadRejectsBadCardSettings(t *testing.T) {
	cases := map[string]string{
		"unknown style":           `card_style = "thumbnail"`,
		"overlong caption":        `card_caption = "` + strings.Repeat("x", 151) + `"`,
		"overlong source caption": `card_source_caption = "` + strings.Repeat("x", 151) + `"`,
		"zero title length":       `card_title_max_length = 0`,
		"huge title length":       `card_title_max_length = 251`,
		"non-http favicon":        `card_favicon_url = "ftp://example.com/icon.png"`,
		"relative favicon":        `card_favicon_url = "/assets/icon-64.png"`,
		"favicon with no host":    `card_favicon_url = "https:///icon.png"`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, minimalConfig+line+"\n")); err == nil {
				t.Errorf("expected an error for %s", line)
			}
		})
	}
}

func TestLoadRejectsMaxDimensionBelowSlacksMinimum(t *testing.T) {
	// A cap under the smallest preview side Slack accepts cannot be honoured.
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
max_dimension = 200
`)
	if _, err := Load(p); err == nil {
		t.Error("expected an error for max_dimension below the Slack preview minimum")
	}
}

func TestLoadAcceptsMaxDimensionZero(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
max_dimension = 0
`)
	if _, err := Load(p); err != nil {
		t.Errorf("max_dimension = 0 disables the cap and must be accepted: %v", err)
	}
}

func TestLoadInlineTokens(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BotToken != "xoxb-bot" {
		t.Errorf("inline bot token not used: %q", cfg.BotToken)
	}
}

func TestLoadRejectsBadAppToken(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "not-an-app-token"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error for app_token without xapp- prefix")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
bogus_key = true
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error for unknown config key")
	}
}

func TestResolveSecretRejectsBoth(t *testing.T) {
	t.Setenv("SS_TEST_BOT", "xoxb-bot")
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-inline"
bot_token_env_var = "SS_TEST_BOT"
app_token = "xapp-app"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error when both bot_token and bot_token_env_var are set")
	}
}

func TestLoadRequiresBaseURL(t *testing.T) {
	p := writeConfig(t, `
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error when screenshotter_base_url is missing")
	}
}

func TestLoadFetchBaseURL(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "https://screen.corp.example"
screenshotter_fetch_base_url = "http://screenshotter.default.svc/"
unfurl_domains = ["screen.corp.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ScreenshotterFetchBaseURL != "http://screenshotter.default.svc" {
		t.Errorf("trailing slash not trimmed: %q", cfg.ScreenshotterFetchBaseURL)
	}
	if cfg.FetchBaseURL() != "http://screenshotter.default.svc" {
		t.Errorf("FetchBaseURL should be the override, got %q", cfg.FetchBaseURL())
	}
	if cfg.ScreenshotterBaseURL != "https://screen.corp.example" {
		t.Errorf("the fetch override must not touch the canonical base URL: %q", cfg.ScreenshotterBaseURL)
	}
}

func TestLoadRejectsRelativeFetchBaseURL(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "https://screen.corp.example"
screenshotter_fetch_base_url = "screenshotter.default.svc"
unfurl_domains = ["screen.corp.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected an error when screenshotter_fetch_base_url is not absolute")
	}
}
