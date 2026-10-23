package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/screenshotter-org/screenshotter_slack_bridge/internal/config"
)

// uploadConfig is baseConfig in card_style "image_upload".
func uploadConfig(baseURL string) *config.Config {
	cfg := baseConfig(baseURL)
	cfg.CardStyle = config.CardStyleImageUpload
	return cfg
}

// sharePrefix is the upload name prefix of linkEvent's share of "abcdef".
const sharePrefix = "abcdef-1700000000.000100-"

func TestImageUploadStyleShowsANewUploadAtFullResolution(t *testing.T) {
	srv, _ := pngServerWithMeta(t, testPNG(t, 2880, 1800), "My page", "")
	defer srv.Close()

	api := &fakeAPI{}
	cfg := uploadConfig(srv.URL)
	cfg.MaxDimension = 0
	b := newTestBridge(t, api, cfg)
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.adds) != 0 {
		t.Errorf("image_upload must not register remote files, got %d", len(api.adds))
	}
	if len(api.uploads) != 1 || len(api.completed) != 1 {
		t.Fatalf("expected one upload, got %d started and %d completed", len(api.uploads), len(api.completed))
	}
	up := api.uploads[0]
	if w, h := previewDims(t, up.data); w != 2880 || h != 1800 {
		t.Errorf("upload is %dx%d, want the full 2880x1800", w, h)
	}
	if !strings.HasPrefix(up.name, sharePrefix) || !strings.HasSuffix(up.name, ".png") {
		t.Errorf("upload name %q should start with %q and end in .png", up.name, sharePrefix)
	}
	if up.altText != "My page" {
		t.Errorf("upload alt text = %q, want the card title", up.altText)
	}
	// The unfurl shares the file; the upload itself must not post it anywhere.
	done := api.completed[0]
	if done.Channel != "" || len(done.Channels) != 0 || done.InitialComment != "" {
		t.Errorf("upload must not be shared on completion, got %#v", done)
	}
	if len(done.Files) != 1 || done.Files[0].ID != "FUP1" || done.Files[0].Title != "My page" {
		t.Errorf("completed files = %#v, want FUP1 titled with the card title", done.Files)
	}

	img, footer := imageCard(t, unfurlFor(t, api, "https://screen.corp.example/abcdef"))
	if img.SlackFile == nil || img.SlackFile.ID != "FUP1" {
		t.Errorf("image block should reference the upload FUP1, got %#v", img.SlackFile)
	}
	if got, want := footerLink(t, footer), "<"+srv.URL+"/abcdef|Open in Screenshotter>"; got != want {
		t.Errorf("footer link = %q, want %q", got, want)
	}
	if len(api.deleted) != 0 {
		t.Errorf("nothing should be deleted, got %v", api.deleted)
	}
}

func TestImageUploadLooksForEarlierUploadsOfTheBotInTheChannel(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{}
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.listCalls) != 1 {
		t.Fatalf("expected one files.list, got %d", len(api.listCalls))
	}
	p := api.listCalls[0]
	if p.User != botUserID || p.Channel != "C1" || p.TimestampFrom != 1700000000 || p.TimestampTo != slack.DEFAULT_FILES_TS_TO {
		t.Errorf("files.list = (user %q, channel %q, from %d, to %d), want the bot's files in C1 since the message",
			p.User, p.Channel, p.TimestampFrom, p.TimestampTo)
	}
}

func TestImageUploadWaitsUntilSlackProcessedTheUpload(t *testing.T) {
	// Slack rejects an unfurl referencing an upload it has not processed yet,
	// so the wait applies even with preview_wait = 0.
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{readyAfter: 2} // processed on the third files.info
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.fileInfos != 3 {
		t.Errorf("expected polling to stop at the third files.info, got %d", api.fileInfos)
	}
	if api.unfurls != 1 {
		t.Errorf("expected the unfurl once the upload was processed, got %d", api.unfurls)
	}
}

func TestImageUploadUnfurlsAnywayWhenTheUploadNeverLooksProcessed(t *testing.T) {
	old := uploadReadyTimeout
	uploadReadyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { uploadReadyTimeout = old })

	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{readyAfter: 1 << 30}
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if api.unfurls != 1 {
		t.Errorf("expected the unfurl to be tried after the timeout, got %d", api.unfurls)
	}
}

func TestImageUploadReusesTheShareEarlierUploadOfTheSameScreenshot(t *testing.T) {
	// A redelivered event, or a message edited before its card appeared, must
	// not upload the unchanged screenshot again.
	data := testPNG(t, 800, 600)
	srv := pngServer(t, data)
	defer srv.Close()

	api := &fakeAPI{listed: []slack.File{
		{ID: "FOLD", Name: uploadName(sharePrefix, data), User: botUserID},
	}}
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.uploads) != 0 {
		t.Errorf("expected no upload, got %d", len(api.uploads))
	}
	if api.fileInfos != 0 {
		t.Errorf("an earlier upload is processed already; expected no files.info, got %d", api.fileInfos)
	}
	img, _ := imageCard(t, unfurlFor(t, api, "https://screen.corp.example/abcdef"))
	if img.SlackFile == nil || img.SlackFile.ID != "FOLD" {
		t.Errorf("image block should reference the earlier upload FOLD, got %#v", img.SlackFile)
	}
	if len(api.deleted) != 0 {
		t.Errorf("the reused upload must not be deleted, got %v", api.deleted)
	}
}

func TestImageUploadReplacesTheUploadOfAChangedScreenshot(t *testing.T) {
	// The screenshot was re-annotated on the server since the share's upload.
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{listed: []slack.File{
		{ID: "FOLD", Name: uploadName(sharePrefix, testPNG(t, 640, 480)), User: botUserID},
		// Neither another share's upload nor someone else's file is touched.
		{ID: "FOTHERMSG", Name: "abcdef-1700000099.000100-0123456789abcdef.png", User: botUserID},
		{ID: "FUSER", Name: sharePrefix + "0123456789abcdef.png", User: "U2"},
	}}
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.uploads) != 1 {
		t.Fatalf("expected the changed screenshot to be uploaded, got %d uploads", len(api.uploads))
	}
	img, _ := imageCard(t, unfurlFor(t, api, "https://screen.corp.example/abcdef"))
	if img.SlackFile == nil || img.SlackFile.ID != "FUP1" {
		t.Errorf("image block should reference the new upload FUP1, got %#v", img.SlackFile)
	}
	if len(api.deleted) != 1 || api.deleted[0].fileID != "FOLD" {
		t.Fatalf("expected only the superseded upload FOLD to be deleted, got %v", api.deleted)
	}
	if api.deleted[0].unfurlsBefore != 1 {
		t.Error("the superseded upload must be deleted only after the new card replaced it")
	}
}

func TestImageUploadKeepsTheSupersededUploadWhenTheUnfurlFails(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{
		listed:    []slack.File{{ID: "FOLD", Name: uploadName(sharePrefix, testPNG(t, 640, 480)), User: botUserID}},
		unfurlErr: errors.New("cannot_parse_attachment"),
	}
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.deleted) != 0 {
		t.Errorf("the card still shows FOLD; it must not be deleted, got %v", api.deleted)
	}
}

func TestImageUploadUploadsAnywayWhenTheLookupFails(t *testing.T) {
	srv := pngServer(t, testPNG(t, 800, 600))
	defer srv.Close()

	api := &fakeAPI{listErr: errors.New("ratelimited")}
	b := newTestBridge(t, api, uploadConfig(srv.URL))
	b.HandleLinkShared(context.Background(), "T1", linkEvent())

	if len(api.uploads) != 1 || api.unfurls != 1 {
		t.Errorf("expected an upload and an unfurl despite the failed lookup, got %d and %d", len(api.uploads), api.unfurls)
	}
}
