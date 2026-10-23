// Package config loads and validates the Slack bridge configuration from a
// TOML file, resolving secrets from environment variables where requested.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/imageproc"
)

// CardStyle selects how an unfurled screenshot is presented in Slack, and
// how the screenshot is stored there: as the preview of a remote file, or as
// an uploaded file.
type CardStyle string

const (
	// CardStyleImage shows the preview through an image block, uncropped, with
	// a footer linking to the screenshot's page. The default.
	CardStyleImage CardStyle = "image"
	// CardStyleFile shows the remote file's own card: Slack's file header
	// (icon, title, filetype) over a cropped preview.
	CardStyleFile CardStyle = "file"
	// CardStyleImageUpload shows the same card as CardStyleImage, but backed
	// by a regular Slack upload instead of a remote file: Slack serves remote
	// file previews at most 1024 px wide, uploads at full resolution. It needs
	// the files:write and files:read scopes, and the uploads count towards the
	// workspace's file storage.
	CardStyleImageUpload CardStyle = "image_upload"
)

// defaultMaxDimension is max_dimension's default for the remote file styles;
// CardStyleImageUpload defaults to no cap, since it is meant to keep the
// resolution.
const defaultMaxDimension = 1600

// DefaultCardFaviconURL is the default icon in the image card's footer. Slack
// fetches it from its own servers when rendering the card, so it must be
// publicly reachable, which a (typically private) screenshotter server is not.
const DefaultCardFaviconURL = "https://www.screenshotter.org/favicon-32x32.png"

// maxCardCaptionLen bounds card_caption and card_source_caption, in runes, well inside the 2000
// characters Slack allows in a context block's text element.
const maxCardCaptionLen = 150

// MaxCardTitleLength is the largest card_title_max_length accepted, in runes.
// The server does not cap the title it stores, and while files.remote.add
// accepted 4000 characters in testing it documents a bad_title error for
// overlong titles.
const MaxCardTitleLength = 250

// Config holds the bridge's runtime configuration.
type Config struct {
	// ScreenshotterBaseURL is the canonical base URL of the screenshotter server
	// (no trailing slash); the posted link only supplies the image ID. It is used
	// as the remote file's external URL — the address people land on when they
	// click the card — and, unless ScreenshotterFetchBaseURL overrides it, as the
	// address the bridge fetches the PNG from. Slack itself never fetches it, so
	// it may be, and is meant to be, a private address (e.g.
	// "http://screenshotter.internal:8080"): only Slack members who can reach it
	// get the full screenshot. Required.
	ScreenshotterBaseURL string `toml:"screenshotter_base_url"`

	// ScreenshotterFetchBaseURL, when set, is the base URL the bridge itself uses
	// to fetch images from the server (no trailing slash), leaving
	// ScreenshotterBaseURL to name only the address the card links to. Set it
	// where the two differ — typically a Kubernetes deployment in which the
	// bridge reaches the server on its in-cluster Service
	// ("http://screenshotter.default.svc") while readers open it on the address
	// their browser resolves. Default: empty, i.e. fetch from
	// ScreenshotterBaseURL.
	ScreenshotterFetchBaseURL string `toml:"screenshotter_fetch_base_url"`

	// UnfurlDomains is the list of hostnames that, when seen in a Slack message,
	// the bridge will unfurl. These must match the "App unfurl domains"
	// registered in the Slack app and the hostnames used in your screen/{id}
	// links.
	UnfurlDomains []string `toml:"unfurl_domains"`

	// BotToken is the Slack bot token (xoxb-...). Prefer BotTokenEnvVar.
	BotToken string `toml:"bot_token"`
	// BotTokenEnvVar is the name of an environment variable holding the bot
	// token. When set and non-empty, it takes precedence over BotToken.
	BotTokenEnvVar string `toml:"bot_token_env_var"`

	// AppToken is the Slack app-level token (xapp-...) used for Socket Mode.
	// Prefer AppTokenEnvVar.
	AppToken string `toml:"app_token"`
	// AppTokenEnvVar is the name of an environment variable holding the app
	// token. When set and non-empty, it takes precedence over AppToken.
	AppTokenEnvVar string `toml:"app_token_env_var"`

	// MaxDimension caps the largest side (width or height), in pixels, of the
	// preview image sent to Slack; larger screenshots are downscaled to it,
	// preserving aspect ratio. The card links through to the full-resolution
	// image on the server, so the preview does not need to be pixel-exact.
	// 0 disables the cap. Must be 0 or at least 300, the smallest side Slack
	// accepts. Default: 1600, or 0 with CardStyleImageUpload.
	MaxDimension int `toml:"max_dimension"`

	// PreviewWait bounds how long the bridge waits for Slack to finish parsing
	// the preview image (files.remote.add returns before it is ready) before
	// unfurling anyway. Unfurling too early still works — Slack fills the card
	// in on its own — but the screenshot then takes noticeably longer to appear
	// (~12s observed, against ~3s when the unfurl waits). 0 unfurls immediately.
	// Unused by CardStyleImageUpload, which always waits (Slack rejects an
	// unfurl referencing an upload it has not processed yet). Default: 8s.
	PreviewWait TOMLDuration `toml:"preview_wait"`

	// CardStyle selects the unfurl's presentation: CardStyleImage (the
	// preview, uncropped, over a footer linking to the screenshot's page),
	// CardStyleImageUpload (the same card at full resolution), or
	// CardStyleFile (Slack's file card, which crops the preview). Default:
	// CardStyleImage.
	CardStyle CardStyle `toml:"card_style"`

	// CardFaviconURL is the icon shown in the image card's footer. Slack fetches
	// it itself when rendering, so it must be an absolute http(s) URL reachable
	// from the internet. Empty drops the icon. Unused by the file style.
	// Default: DefaultCardFaviconURL.
	CardFaviconURL string `toml:"card_favicon_url"`

	// CardCaption is the text of the image card's footer link to the
	// screenshot's page. Empty drops that link. Unused by the file style.
	// Default: "Open in Screenshotter".
	CardCaption string `toml:"card_caption"`

	// CardSourceCaption is the text of the image card's footer link to the page
	// the screenshot was taken from, shown when the server reports that page.
	// Empty drops that link. With both captions empty the image card has no
	// footer, favicon included. Unused by the file style. Default: "Open
	// source page".
	CardSourceCaption string `toml:"card_source_caption"`

	// CardTitleMaxLength caps the card's title, in characters (runes); longer
	// titles are cut after that many and followed by an ellipsis ("…"). Applies to every style. Must be
	// between 1 and MaxCardTitleLength. Default: 90.
	CardTitleMaxLength int `toml:"card_title_max_length"`

	// RequestTimeout bounds each outbound request the bridge makes — both
	// fetching an image from the screenshotter server and the Slack API calls
	// (file uploads, remote file add/info, unfurl, user/usergroup lookups).
	// Default: 30s.
	RequestTimeout TOMLDuration `toml:"request_timeout"`

	// MaxConcurrency caps how many link_shared events the bridge handles in
	// parallel; this bounds the network/Slack work ("the rest"). Events beyond
	// this are dropped (logged) rather than queued, so handler goroutines can't
	// pile up unbounded under a flood. Default: 50.
	MaxConcurrency int `toml:"max_concurrency"`

	// MaxImageWorkers caps how many images are decoded/rescaled in parallel.
	// Image conversion is the memory-heavy step (a decoded image can be far
	// larger than its PNG), so it is bounded separately from, and more tightly
	// than, MaxConcurrency. Default: 4.
	MaxImageWorkers int `toml:"max_image_workers"`

	// MaxLinksPerMessage caps how many screenshot links the bridge renders from
	// a single Slack message, limiting amplification from one post. Slack itself
	// unfurls at most 5 links per message. Default: 5.
	MaxLinksPerMessage int `toml:"max_links_per_message"`

	// BlockExternalUsers, when true, makes the bridge ignore link_shared events
	// triggered by users that are external to the workspace: Slack Connect
	// strangers, users from a different team, and guests (single- and
	// multi-channel). Requires the users:read scope. Default: false.
	BlockExternalUsers bool `toml:"block_external_users"`

	// AllowedUserGroups, when non-empty, restricts rendering to users who belong
	// to at least one of the listed Slack user group IDs (e.g. "S0123ABCD").
	// Requires the usergroups:read scope. Default: empty (no restriction).
	AllowedUserGroups []string `toml:"allowed_usergroups"`

	// ExcludedPaths lists additional first path segments that must never be
	// treated as image IDs, supplementing the built-in set of known server
	// endpoints. Matched case-insensitively. Default: empty.
	ExcludedPaths []string `toml:"excluded_paths"`

	// Debug enables verbose logging of the Socket Mode connection and Slack API
	// traffic. WARNING: debug output can include the bot token and other
	// sensitive data; do not enable it in production. Default: false.
	Debug bool `toml:"debug"`
}

// TOMLDuration is a time.Duration that unmarshals from a TOML string like
// "15s". Mirrors the server's config type for consistency.
type TOMLDuration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *TOMLDuration) UnmarshalText(b []byte) error {
	dur, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// Load reads, decodes, and validates the configuration at path.
func Load(path string) (*Config, error) {
	c := &Config{
		MaxDimension:       defaultMaxDimension,
		PreviewWait:        TOMLDuration{8 * time.Second},
		CardStyle:          CardStyleImage,
		CardFaviconURL:     DefaultCardFaviconURL,
		CardCaption:        "Open in Screenshotter",
		CardSourceCaption:  "Open source page",
		CardTitleMaxLength: 90,
		RequestTimeout:     TOMLDuration{30 * time.Second},
		MaxConcurrency:     50,
		MaxImageWorkers:    4,
		MaxLinksPerMessage: 5,
	}

	meta, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}
	if c.CardStyle == CardStyleImageUpload && !meta.IsDefined("max_dimension") {
		c.MaxDimension = 0
	}

	c.BotToken, err = resolveSecret("bot_token", c.BotToken, "bot_token_env_var", c.BotTokenEnvVar)
	if err != nil {
		return nil, err
	}
	c.AppToken, err = resolveSecret("app_token", c.AppToken, "app_token_env_var", c.AppTokenEnvVar)
	if err != nil {
		return nil, err
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.ScreenshotterBaseURL == "" {
		return fmt.Errorf("screenshotter_base_url is required")
	}
	normalized, err := normalizeBaseURL("screenshotter_base_url", c.ScreenshotterBaseURL)
	if err != nil {
		return err
	}
	c.ScreenshotterBaseURL = normalized

	if c.ScreenshotterFetchBaseURL != "" {
		normalized, err := normalizeBaseURL("screenshotter_fetch_base_url", c.ScreenshotterFetchBaseURL)
		if err != nil {
			return err
		}
		c.ScreenshotterFetchBaseURL = normalized
	}

	if len(c.UnfurlDomains) == 0 {
		return fmt.Errorf("unfurl_domains must list at least one domain")
	}
	for i, d := range c.UnfurlDomains {
		c.UnfurlDomains[i] = strings.ToLower(strings.TrimSpace(d))
		if c.UnfurlDomains[i] == "" {
			return fmt.Errorf("unfurl_domains contains an empty entry")
		}
	}

	if c.BotToken == "" {
		return fmt.Errorf("bot_token (or bot_token_env_var) is required")
	}
	if c.AppToken == "" {
		return fmt.Errorf("app_token (or app_token_env_var) is required")
	}
	if !strings.HasPrefix(c.AppToken, "xapp-") {
		return fmt.Errorf("app_token must be a Socket Mode app-level token starting with \"xapp-\"")
	}
	if c.MaxDimension != 0 && c.MaxDimension < imageproc.MinPreviewDim {
		return fmt.Errorf("max_dimension must be 0 or at least %d, the smallest preview side Slack accepts", imageproc.MinPreviewDim)
	}
	if c.PreviewWait.Duration < 0 {
		return fmt.Errorf("preview_wait must not be negative")
	}
	switch c.CardStyle {
	case CardStyleImage, CardStyleImageUpload, CardStyleFile:
	default:
		return fmt.Errorf("card_style must be %q, %q or %q", CardStyleImage, CardStyleImageUpload, CardStyleFile)
	}
	if c.CardFaviconURL != "" {
		u, err := url.Parse(c.CardFaviconURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("card_favicon_url must be empty or an absolute http(s) URL")
		}
	}
	c.CardCaption = strings.TrimSpace(c.CardCaption)
	if utf8.RuneCountInString(c.CardCaption) > maxCardCaptionLen {
		return fmt.Errorf("card_caption must be at most %d characters", maxCardCaptionLen)
	}
	c.CardSourceCaption = strings.TrimSpace(c.CardSourceCaption)
	if utf8.RuneCountInString(c.CardSourceCaption) > maxCardCaptionLen {
		return fmt.Errorf("card_source_caption must be at most %d characters", maxCardCaptionLen)
	}
	if c.CardTitleMaxLength < 1 || c.CardTitleMaxLength > MaxCardTitleLength {
		return fmt.Errorf("card_title_max_length must be between 1 and %d", MaxCardTitleLength)
	}
	if c.RequestTimeout.Duration <= 0 {
		return fmt.Errorf("request_timeout must be positive")
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("max_concurrency must be at least 1")
	}
	if c.MaxImageWorkers < 1 {
		return fmt.Errorf("max_image_workers must be at least 1")
	}
	if c.MaxLinksPerMessage < 1 {
		return fmt.Errorf("max_links_per_message must be at least 1")
	}
	for i, g := range c.AllowedUserGroups {
		c.AllowedUserGroups[i] = strings.TrimSpace(g)
		if c.AllowedUserGroups[i] == "" {
			return fmt.Errorf("allowed_usergroups contains an empty entry")
		}
	}
	for i, p := range c.ExcludedPaths {
		c.ExcludedPaths[i] = strings.ToLower(strings.Trim(strings.TrimSpace(p), "/"))
		if c.ExcludedPaths[i] == "" {
			return fmt.Errorf("excluded_paths contains an empty entry")
		}
	}
	return nil
}

// resolveSecret returns the value from the named environment variable when
// envVar is set, otherwise the inline value. It is an error to set both.
func resolveSecret(inlineKey, inlineVal, envKey, envVar string) (string, error) {
	if envVar != "" {
		if inlineVal != "" {
			return "", fmt.Errorf("set only one of %s or %s", inlineKey, envKey)
		}
		v := os.Getenv(envVar)
		if v == "" {
			return "", fmt.Errorf("%s names environment variable %q which is empty or unset", envKey, envVar)
		}
		return v, nil
	}
	return inlineVal, nil
}

// FetchBaseURL is the base URL the bridge fetches images from: the dedicated
// screenshotter_fetch_base_url when the deployment sets one, otherwise the
// canonical screenshotter_base_url the card links to.
func (c *Config) FetchBaseURL() string {
	if c.ScreenshotterFetchBaseURL != "" {
		return c.ScreenshotterFetchBaseURL
	}
	return c.ScreenshotterBaseURL
}

// normalizeBaseURL checks that a base URL is absolute and strips any trailing
// slashes, so callers can append "/<id>" to it.
func normalizeBaseURL(key, value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%s must be an absolute URL like \"https://host\"", key)
	}
	return strings.TrimRight(value, "/"), nil
}
