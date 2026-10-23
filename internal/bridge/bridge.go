// Package bridge contains the core logic that turns a Slack link_shared event
// for a screenshotter link into an inline unfurl backed by a Slack file: the
// screenshot is either registered with files.remote.add, whose preview Slack
// stores workspace-privately, or uploaded (card_style "image_upload"), and the
// link is replaced by a card pointing back at the (possibly private)
// screenshotter page.
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/config"
	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/imageproc"
)

// maxImageBytes bounds how much we read from the screenshotter server, as a
// safety net against an unexpectedly huge response.
const maxImageBytes = 64 << 20 // 64 MiB

// errNotFound is returned when the screenshotter server has no such image.
var errNotFound = errors.New("image not found")

// previewPollInterval is how often files.remote.info (or files.info, for an
// upload) is polled while waiting for Slack to finish parsing the preview
// image. Parsing took ~1.7-2.1s in testing.
const previewPollInterval = 250 * time.Millisecond

// idPattern matches the random alphanumeric image IDs the server generates.
// Keeping IDs strictly alphanumeric also prevents path traversal or injection
// into the fetch URL.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// slackTSPattern matches a real Slack message timestamp ("1700000000.000100").
// The compose-area variant of link_shared carries a UUID instead, which cannot
// be unfurled, so we use this to skip those events.
var slackTSPattern = regexp.MustCompile(`^\d+\.\d+$`)

// defaultReservedPaths are first path segments that are known server endpoints
// rather than image IDs, so a link to one must never be fetched as an image.
//
// IMPORTANT: keep this in sync with the screenshotter server's routes. If a new
// top-level route is added to the server (see internal/server/server.go), add
// its first path segment here. Operators can also extend the list via the
// excluded_paths config key without editing this file.
var defaultReservedPaths = []string{
	"upload", "assets", "static", "thumb", "admin", "auth", "favicon.ico",
}

// slackAPI is the subset of the Slack client the bridge uses.
type slackAPI interface {
	AddRemoteFileContext(ctx context.Context, params slack.RemoteFileParameters) (*slack.RemoteFile, error)
	GetRemoteFileInfoContext(ctx context.Context, externalID, fileID string) (*slack.RemoteFile, error)
	GetUploadURLExternalContext(ctx context.Context, params slack.GetUploadURLExternalParameters) (*slack.GetUploadURLExternalResponse, error)
	UploadToURL(ctx context.Context, params slack.UploadToURLParameters) error
	CompleteUploadExternalContext(ctx context.Context, params slack.CompleteUploadExternalParameters) (*slack.CompleteUploadExternalResponse, error)
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
	GetFilesContext(ctx context.Context, params slack.GetFilesParameters) ([]slack.File, *slack.Paging, error)
	DeleteFileContext(ctx context.Context, fileID string) error
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	UnfurlMessageContext(ctx context.Context, channelID, timestamp string, unfurls map[string]slack.Attachment, options ...slack.MsgOption) (string, string, string, error)
	GetUserInfoContext(ctx context.Context, user string, options ...slack.GetUserInfoOption) (*slack.User, error)
	GetUserGroupMembersContext(ctx context.Context, userGroup string, options ...slack.GetUserGroupMembersOption) ([]string, error)
}

// Bridge renders screenshotter links into Slack.
type Bridge struct {
	cfg      *config.Config
	api      slackAPI
	http     *http.Client
	domains  map[string]bool
	reserved map[string]bool
	// admit bounds the number of link_shared events handled concurrently
	// ("the rest": fetch + Slack calls). It is acquired non-blocking, so a
	// flood drops events instead of accumulating goroutines.
	admit chan struct{}
	// convert bounds the number of concurrent image decode/resize operations,
	// the memory-heavy step, independently of admit.
	convert chan struct{}
	authz   *authorizer // nil when no user restrictions are configured
	bot     botIdentity // the bot's user ID, looked up for card_style "image_upload"
	log     *log.Logger
}

// New creates a Bridge. api is typically a *slack.Client.
func New(cfg *config.Config, api slackAPI, logger *log.Logger) *Bridge {
	domains := make(map[string]bool, len(cfg.UnfurlDomains))
	for _, d := range cfg.UnfurlDomains {
		domains[d] = true
	}
	reserved := make(map[string]bool, len(defaultReservedPaths)+len(cfg.ExcludedPaths))
	for _, p := range defaultReservedPaths {
		reserved[p] = true
	}
	for _, p := range cfg.ExcludedPaths {
		reserved[p] = true
	}

	var az *authorizer
	if cfg.BlockExternalUsers || len(cfg.AllowedUserGroups) > 0 {
		az = newAuthorizer(api, cfg, logger)
	}

	return &Bridge{
		cfg:      cfg,
		api:      api,
		http:     newHTTPClient(cfg.RequestTimeout.Duration),
		domains:  domains,
		reserved: reserved,
		admit:    make(chan struct{}, cfg.MaxConcurrency),
		convert:  make(chan struct{}, cfg.MaxImageWorkers),
		authz:    az,
		log:      logger,
	}
}

// newHTTPClient builds the client used to fetch images. It refuses to follow
// redirects: the screenshotter server serves PNGs directly, and following a
// redirect could send the request to an unintended host.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// HandleLinkShared processes every recognised link in a link_shared event.
// teamID is the workspace the event was delivered to, used for external-user
// checks.
func (b *Bridge) HandleLinkShared(ctx context.Context, teamID string, ev *slackevents.LinkSharedEvent) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Printf("recovered from panic handling link_shared: %v", r)
		}
	}()

	if !slackTSPattern.MatchString(ev.MessageTimeStamp) {
		// Compose-area preview (source "composer"): the message_ts is a
		// userID-uuid-hash unfurl_id rather than a real timestamp, there is no
		// posted message to attach to, and chat.unfurl by channel+ts cannot act
		// on it. Slack fires one per keystroke while a link is being typed, so
		// log only under debug to avoid flooding normal operation.
		if b.cfg.Debug {
			b.log.Printf("ignoring composer-area link_shared (non-message ts %q)", ev.MessageTimeStamp)
		}
		return
	}

	// Admission control: bound concurrent handlers. A non-blocking acquire means
	// that under a flood we drop events instead of letting goroutines (and their
	// downstream Slack work) accumulate without limit.
	select {
	case b.admit <- struct{}{}:
		defer func() { <-b.admit }()
	default:
		b.log.Printf("at capacity (%d concurrent), dropping link_shared for message %q",
			cap(b.admit), ev.MessageTimeStamp)
		return
	}

	if b.authz != nil && !b.authz.Allow(ctx, ev.User, teamID) {
		b.log.Printf("skipping link_shared from unauthorized user %q", ev.User)
		return
	}

	// Register every screenshot first, then wait for Slack to parse the previews,
	// then unfurl. Slack parses previews concurrently, so one wait covers the
	// whole message instead of one wait per link.
	var shares []pendingShare
	attempted := 0
	for _, link := range ev.Links {
		id, ok := b.extractID(link)
		if !ok {
			continue
		}
		if attempted >= b.cfg.MaxLinksPerMessage {
			b.log.Printf("link cap (%d) reached for message %q; skipping remaining links",
				b.cfg.MaxLinksPerMessage, ev.MessageTimeStamp)
			break
		}
		attempted++
		sh, err := b.registerShare(ctx, ev, link.URL, id)
		if err != nil {
			if errors.Is(err, errNotFound) {
				b.log.Printf("image %q not found, skipping", id)
				continue
			}
			b.log.Printf("registering %q failed: %v", id, err)
			continue
		}
		shares = append(shares, sh)
	}
	if len(shares) == 0 {
		return
	}

	b.waitForPreviews(ctx, shares)

	// Unfurl each link in its own chat.unfurl call. Slack accumulates unfurls per
	// URL across calls, so independent calls let every good link render even if
	// another link in the same message fails.
	for _, sh := range shares {
		if err := b.unfurlShare(ctx, ev, sh); err != nil {
			b.log.Printf("unfurling %q failed: %v", sh.id, err)
		}
	}
}

// pendingShare is a screenshot registered with Slack and awaiting its unfurl.
type pendingShare struct {
	link   string // the URL as posted, which keys the unfurl
	id     string // the image ID, for logging
	extID  string // the remote file registered for this share; "" for an upload
	fileID string // Slack's ID for the remote file or upload, which an image block references
	title  string // the card's title, reused as the attachment's plain-text fallback
	source string // the page the screenshot was taken from, or "" if unknown
	// ready is set when Slack is known to have processed the file already (an
	// earlier upload being reused), so there is nothing to wait for.
	ready bool
	// stale lists earlier uploads backing this share, to delete once the
	// unfurl has replaced them.
	stale []string
}

// registerShare fetches the screenshot and registers this share of it as a
// remote file or an upload, returning the share to unfurl. The memory-heavy
// image conversion is bounded by preparePreview.
func (b *Bridge) registerShare(ctx context.Context, ev *slackevents.LinkSharedEvent, link, id string) (pendingShare, error) {
	data, meta, err := b.fetchPNG(ctx, id)
	if err != nil {
		return pendingShare{}, err // may be errNotFound; let the caller classify it
	}
	sh := pendingShare{link: link, id: id, title: previewTitle(id, meta, b.cfg.CardTitleMaxLength), source: meta.sourceURL}
	if b.cfg.CardStyle == config.CardStyleImageUpload {
		if err := b.uploadShare(ctx, ev, &sh, data); err != nil {
			return pendingShare{}, err
		}
		return sh, nil
	}

	preview, err := b.preparePreview(ctx, data)
	if err != nil {
		return pendingShare{}, fmt.Errorf("prepare preview: %w", err)
	}
	sh.extID = shareExternalID(id, ev)
	file, err := b.addRemoteFile(ctx, sh, preview, meta)
	if err != nil {
		return pendingShare{}, fmt.Errorf("files.remote.add: %s", slackErr(err))
	}
	if file != nil {
		sh.fileID = file.ID
	}
	if sh.fileID == "" && b.cfg.CardStyle != config.CardStyleFile {
		return pendingShare{}, errors.New("files.remote.add: response carries no file ID for the image block")
	}
	return sh, nil
}

// unfurlShare replaces the posted link with a card for its file, then deletes
// the uploads the card supersedes.
func (b *Bridge) unfurlShare(ctx context.Context, ev *slackevents.LinkSharedEvent, sh pendingShare) error {
	unfurls := map[string]slack.Attachment{sh.link: b.cardAttachment(sh)}
	if err := b.unfurl(ctx, ev.Channel, ev.MessageTimeStamp, unfurls); err != nil {
		return fmt.Errorf("unfurl: %s", slackErr(err))
	}
	b.deleteStale(ctx, sh)
	return nil
}

// cardAttachment builds the unfurl for a share in the configured card style.
// In both, Fallback is what clients show where blocks cannot be rendered —
// notably the chip standing in for the card while the message is being edited,
// which otherwise reads "[no preview available]".
func (b *Bridge) cardAttachment(sh pendingShare) slack.Attachment {
	if b.cfg.CardStyle == config.CardStyleFile {
		// A file block must be the only block in the unfurl, so the title and
		// the click-through both come from the remote file itself. HideColor
		// drops the coloured bar down the card's left edge, which Slack allows
		// only for a lone file block.
		return slack.Attachment{
			Fallback:  sh.title,
			HideColor: true,
			Blocks:    slack.Blocks{BlockSet: []slack.Block{slack.NewFileBlock("", sh.extID, "remote")}},
		}
	}

	// The image styles draw the file uncropped through an image block's
	// slack_file. For a remote file that is its stored preview, which Slack
	// accepts although it documents slack_file only for uploaded files. The
	// picture is not a link, so the click-throughs are a footer line of our
	// own. HideColor
	// is rejected alongside an image block (cannot_parse_attachment), so this
	// card keeps Slack's colour bar.
	image := slack.NewImageBlockSlackFile(&slack.SlackFileObject{ID: sh.fileID}, sh.title, "",
		slack.NewTextBlockObject(slack.PlainTextType, sh.title, false, false))
	blocks := []slack.Block{image}
	if footer := b.cardFooter(sh); footer != nil {
		blocks = append(blocks, footer)
	}
	return slack.Attachment{
		Fallback: sh.title,
		Blocks:   slack.Blocks{BlockSet: blocks},
	}
}

// footerSeparator sits between the links of the image card's footer.
const footerSeparator = "   •   "

// cardFooter returns the image card's footer: the favicon, then the links to
// the screenshot's page and to its source page, each present only when its
// caption is set (and, for the source, when the server reported an http(s)
// source URL). It returns nil when there is no link to show.
func (b *Bridge) cardFooter(sh pendingShare) *slack.ContextBlock {
	var links []string
	if b.cfg.CardCaption != "" {
		links = append(links, mrkdwnLink(b.pageURL(sh.id), b.cfg.CardCaption))
	}
	if b.cfg.CardSourceCaption != "" && isWebURL(sh.source) {
		links = append(links, mrkdwnLink(sh.source, b.cfg.CardSourceCaption))
	}
	if len(links) == 0 {
		return nil
	}
	var elems []slack.MixedElement
	if b.cfg.CardFaviconURL != "" {
		// Slack fetches this itself: a remote file renders blank here.
		elems = append(elems, slack.NewImageBlockElement(b.cfg.CardFaviconURL, "Screenshotter"))
	}
	elems = append(elems, slack.NewTextBlockObject(slack.MarkdownType,
		strings.Join(links, footerSeparator), false, false))
	return slack.NewContextBlock("", elems...)
}

// isWebURL reports whether s is an absolute http(s) URL, the only kind of
// source page the card links to.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// mrkdwnEscaper escapes the three characters Slack's mrkdwn gives a meaning in
// link text.
var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeMrkdwn makes s safe to embed as literal mrkdwn text.
func escapeMrkdwn(s string) string {
	return mrkdwnEscaper.Replace(s)
}

// mrkdwnLink returns a mrkdwn link to target showing text. A "|" in target is
// percent-encoded, since Slack would otherwise take it as the end of the URL.
func mrkdwnLink(target, text string) string {
	return "<" + escapeMrkdwn(strings.ReplaceAll(target, "|", "%7C")) + "|" + escapeMrkdwn(text) + ">"
}

// shareExternalID returns the external_id registering one share of image id.
// It is unique per (screenshot, message) rather than per screenshot, because a
// remote file accumulates the channel, timestamp and author of every share it
// backs: reusing one file across shares would expose each share to viewers of
// all the others, and — since files.remote.add upserts — would rewrite the
// preview and title of cards already posted elsewhere. Deriving it from the
// message rather than at random keeps the add idempotent, so a redelivered
// event or an edited message refreshes its own card instead of leaking a
// duplicate file.
func shareExternalID(id string, ev *slackevents.LinkSharedEvent) string {
	return id + "-" + ev.Channel + "-" + ev.MessageTimeStamp
}

// addRemoteFile registers the share as a remote file under its external ID,
// returning the file Slack created. The image ID names the preview upload and
// builds the external URL — the screenshot's page on the canonical server,
// where the file card's click-through leads.
func (b *Bridge) addRemoteFile(ctx context.Context, sh pendingShare, preview []byte, meta imageMeta) (*slack.RemoteFile, error) {
	actx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	return b.api.AddRemoteFileContext(actx, slack.RemoteFileParameters{
		ExternalID:  sh.extID,
		ExternalURL: b.pageURL(sh.id),
		Title:       sh.title,
		Filetype:    "png",
		// The file card has no room for the source URL, so it is also stored as
		// the file's indexable contents.
		IndexableFileContents: strings.TrimSpace(meta.title + " " + meta.sourceURL),
		PreviewImageReader:    bytes.NewReader(preview),
		PreviewImageName:      sh.id + ".png",
	})
}

// waitForPreviews blocks until Slack has processed the image of every share,
// or until the wait times out. Both files.remote.add and an upload return
// before the image is ready. For a remote file, unfurling in that window still
// works — Slack fills the card in on its own — but the screenshot takes far
// longer to show up, so waiting is worth a few polls, up to preview_wait. An
// upload cannot be unfurled before it is ready, so it is always waited for, up
// to uploadReadyTimeout. Slack processes the images concurrently, so one
// deadline covers the whole message. Failures are not fatal: the unfurls
// proceed anyway.
func (b *Bridge) waitForPreviews(ctx context.Context, shares []pendingShare) {
	timeout, ready := b.cfg.PreviewWait.Duration, b.remotePreviewReady
	if b.cfg.CardStyle == config.CardStyleImageUpload {
		timeout, ready = uploadReadyTimeout, b.uploadReady
	}
	if timeout <= 0 {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var pending []pendingShare
	for _, sh := range shares {
		if !sh.ready {
			pending = append(pending, sh)
		}
	}

	for len(pending) > 0 {
		var notReady []pendingShare
		for _, sh := range pending {
			ok, err := ready(wctx, sh)
			switch {
			case wctx.Err() != nil:
				b.log.Printf("previews for %d file(s) not ready after %v; unfurling anyway",
					len(pending), timeout)
				return
			case err != nil:
				// Stop tracking this one rather than holding up the others.
				b.log.Printf("preview status for %q unavailable (%s); unfurling anyway", sh.id, slackErr(err))
			case !ok:
				notReady = append(notReady, sh)
			}
		}
		if len(notReady) == 0 {
			return
		}
		pending = notReady

		select {
		case <-time.After(previewPollInterval):
		case <-wctx.Done():
			b.log.Printf("previews for %d file(s) not ready after %v; unfurling anyway",
				len(pending), timeout)
			return
		}
	}
}

// remotePreviewReady reports whether Slack has parsed the preview of the
// remote file backing sh.
func (b *Bridge) remotePreviewReady(ctx context.Context, sh pendingShare) (bool, error) {
	file, err := b.api.GetRemoteFileInfoContext(ctx, sh.extID, "")
	if err != nil {
		return false, err
	}
	return file != nil && file.HasRichPreview, nil
}

// pageURL is the screenshot's HTML page on the screenshotter server — the
// remote file's external URL, and so where clicking the card leads. It is
// always built on screenshotter_base_url, the address readers open, never on
// the bridge's own screenshotter_fetch_base_url. base_url is validated and
// trailing-slash-trimmed by config.
func (b *Bridge) pageURL(id string) string {
	return b.cfg.ScreenshotterBaseURL + "/" + id
}

// imageFileURL is the PNG URL on the screenshotter server, which the bridge
// fetches over the (possibly private) network. It is built on the config's
// FetchBaseURL, which is screenshotter_fetch_base_url where a deployment gives
// the bridge its own address for the server (an in-cluster Service, say) and
// screenshotter_base_url otherwise. no_redirect=1 tells the server to serve the
// image on whichever address the request arrived on rather than 301-ing to its
// canonical_address: the bridge reaches the server over a private network, on
// which the canonical address typically does not resolve, and it does not
// follow redirects (see newHTTPClient). It is a no-op on a server whose
// canonical address is the one configured here.
func (b *Bridge) imageFileURL(id string) string {
	return b.cfg.FetchBaseURL() + "/" + id + ".png?no_redirect=1"
}

// previewTitle returns the card's title: the title the server supplied, else
// the page the screenshot was taken from (more informative than the ID alone),
// else a fallback naming the image. A title longer than maxLen runes is cut after
// maxLen of them and followed by an ellipsis ("…").
func previewTitle(id string, meta imageMeta, maxLen int) string {
	title := meta.title
	if title == "" {
		title = meta.sourceURL
	}
	if title == "" {
		return "Screenshot " + id
	}
	if runes := []rune(title); len(runes) > maxLen {
		return strings.TrimRightFunc(string(runes[:maxLen]), unicode.IsSpace) + "…"
	}
	return title
}

// slackErr renders a Slack API error with the detail Slack attaches in
// response_metadata.messages. The slack-go error's own string is only the short
// code (e.g. "invalid_blocks"); the per-block validation messages, which say
// exactly what was rejected, live in the metadata and are otherwise dropped.
func slackErr(err error) string {
	var se slack.SlackErrorResponse
	if errors.As(err, &se) && len(se.ResponseMetadata.Messages) > 0 {
		return fmt.Sprintf("%s: %s", se.Err, strings.Join(se.ResponseMetadata.Messages, "; "))
	}
	return err.Error()
}

// preparePreview runs the image conversion under the convert semaphore, so the
// number of simultaneous decode/rescale operations — the memory-heavy step —
// stays bounded independently of the overall admission limit.
func (b *Bridge) preparePreview(ctx context.Context, data []byte) ([]byte, error) {
	select {
	case b.convert <- struct{}{}:
		defer func() { <-b.convert }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return imageproc.PreparePreview(data, b.cfg.MaxDimension)
}

// unfurl replaces a shared link with an inline preview, with a per-request
// timeout.
func (b *Bridge) unfurl(ctx context.Context, channel, ts string, unfurls map[string]slack.Attachment) error {
	uctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	_, _, _, err := b.api.UnfurlMessageContext(uctx, channel, ts, unfurls)
	return err
}

// imageMeta holds the optional screenshot metadata the server exposes as headers
// on the image response. Empty strings mean the header was absent.
type imageMeta struct {
	title     string // X-Screenshot-Title
	sourceURL string // X-Screenshot-Source-Url
}

// parseImageMeta reads the screenshot metadata headers, percent-decoding them
// (the server percent-encodes the values so arbitrary text is a valid header).
// A missing or malformed value yields an empty string.
func parseImageMeta(h http.Header) imageMeta {
	return imageMeta{
		title:     decodeMetaHeader(h.Get("X-Screenshot-Title")),
		sourceURL: decodeMetaHeader(h.Get("X-Screenshot-Source-Url")),
	}
}

func decodeMetaHeader(v string) string {
	if v == "" {
		return ""
	}
	decoded, err := url.QueryUnescape(v)
	if err != nil {
		return "" // malformed; treat as absent rather than surfacing raw bytes
	}
	return decoded
}

// fetchPNG downloads the full-size image for id from the screenshotter server,
// returning the bytes and the metadata headers.
func (b *Bridge) fetchPNG(ctx context.Context, id string) ([]byte, imageMeta, error) {
	imgURL := b.imageFileURL(id)

	fctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return nil, imageMeta{}, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, imageMeta{}, fmt.Errorf("fetch %s: %w", imgURL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, imageMeta{}, errNotFound
	default:
		return nil, imageMeta{}, fmt.Errorf("fetch %s: unexpected status %d", imgURL, resp.StatusCode)
	}

	// Safeguard: only forward responses the server labels as an image.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return nil, imageMeta{}, fmt.Errorf("fetch %s: unexpected content-type %q", imgURL, ct)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, imageMeta{}, fmt.Errorf("read %s: %w", imgURL, err)
	}
	if len(data) > maxImageBytes {
		return nil, imageMeta{}, fmt.Errorf("image %s exceeds %d bytes", id, maxImageBytes)
	}
	return data, parseImageMeta(resp.Header), nil
}

// extractID returns the image ID encoded in a shared link, and whether the
// link is one this bridge should handle. It accepts both /{id} and /{id}.png on
// any configured domain, and rejects known server endpoints.
func (b *Bridge) extractID(link slackevents.SharedLinks) (string, bool) {
	u, err := url.Parse(link.URL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if !b.domains[host] {
		return "", false
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	path = strings.TrimSuffix(path, "/")
	if strings.HasSuffix(strings.ToLower(path), ".png") {
		path = path[:len(path)-len(".png")]
	}
	if path == "" || strings.Contains(path, "/") {
		return "", false
	}
	if !idPattern.MatchString(path) {
		return "", false
	}
	if b.reserved[strings.ToLower(path)] {
		return "", false
	}
	return path, true
}
