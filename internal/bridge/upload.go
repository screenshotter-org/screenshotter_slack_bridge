package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// uploadReadyTimeout bounds the wait for Slack to process an uploaded image
// (~1-2s in testing). Unlike a remote file's preview, an upload cannot be
// unfurled before it is processed: chat.unfurl fails with
// cannot_parse_attachment, so when this elapses the unfurl is tried anyway in
// case only the readiness check was off. A variable so tests can shorten it.
var uploadReadyTimeout = 15 * time.Second

// uploadListCount is how many of the bot's most recent images in a channel
// are searched for an earlier upload of the same share. Past that many
// uploads in the channel since the message, a share is uploaded again.
const uploadListCount = 100

// uploadNamePrefix returns the start of the file name of every upload backing
// one share of image id: the image ID and the message timestamp. The channel
// is not in it, since the lookup is already scoped to the channel.
func uploadNamePrefix(id string, ev *slackevents.LinkSharedEvent) string {
	return id + "-" + ev.MessageTimeStamp + "-"
}

// uploadName returns the file name of the upload backing one share of an
// image whose PNG is data: the share's prefix followed by a hash of the
// screenshot, so that a screenshot changed on the server (re-annotated, say)
// is told apart from one already uploaded.
func uploadName(prefix string, data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + hex.EncodeToString(sum[:8]) + ".png"
}

// botIdentity caches the bot's own user ID, which scopes the search for
// earlier uploads to files the bridge made.
type botIdentity struct {
	mu     sync.Mutex
	userID string
}

// get returns the bot's user ID, asking Slack (auth.test) until it once
// succeeds.
func (bi *botIdentity) get(ctx context.Context, api slackAPI) (string, error) {
	bi.mu.Lock()
	defer bi.mu.Unlock()
	if bi.userID == "" {
		resp, err := api.AuthTestContext(ctx)
		if err != nil {
			return "", err
		}
		if resp.UserID == "" {
			return "", errors.New("auth.test returned no user ID")
		}
		bi.userID = resp.UserID
	}
	return bi.userID, nil
}

// uploadShare points sh at an upload of the screenshot data for this share:
// the earlier upload when one with the same content backs this message
// already (a redelivered event, or a message edited before its card
// appeared), else a new upload. Earlier uploads of other content are recorded
// in sh.stale, to be deleted once the new card is in place.
func (b *Bridge) uploadShare(ctx context.Context, ev *slackevents.LinkSharedEvent, sh *pendingShare, data []byte) error {
	prefix := uploadNamePrefix(sh.id, ev)
	name := uploadName(prefix, data)
	earlier, err := b.findUploads(ctx, ev, prefix)
	if err != nil {
		// Not fatal: at worst the share is uploaded a second time.
		b.log.Printf("looking up earlier uploads of %q failed (%s); uploading anew", sh.id, slackErr(err))
	}
	for _, f := range earlier {
		if f.Name == name && sh.fileID == "" {
			sh.fileID = f.ID
			sh.ready = true
		} else {
			sh.stale = append(sh.stale, f.ID)
		}
	}
	if sh.fileID != "" {
		return nil
	}

	preview, err := b.preparePreview(ctx, data)
	if err != nil {
		return fmt.Errorf("prepare preview: %w", err)
	}
	sh.fileID, err = b.uploadFile(ctx, name, sh.title, preview)
	return err
}

// findUploads returns the bot's uploads in the event's channel whose name
// starts with prefix. Slack lists an upload in a channel only once it is
// shared there, which the unfurl does, so an upload whose unfurl failed is
// not found.
func (b *Bridge) findUploads(ctx context.Context, ev *slackevents.LinkSharedEvent, prefix string) ([]slack.File, error) {
	lctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	botID, err := b.bot.get(lctx, b.api)
	if err != nil {
		return nil, fmt.Errorf("auth.test: %w", err)
	}
	// The uploads were all made after the message was posted.
	secs, _, _ := strings.Cut(ev.MessageTimeStamp, ".")
	from, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("message timestamp %q: %w", ev.MessageTimeStamp, err)
	}
	params := slack.NewGetFilesParameters()
	params.User = botID
	params.Channel = ev.Channel
	params.TimestampFrom = slack.JSONTime(from)
	params.Types = "images"
	params.Count = uploadListCount
	files, _, err := b.api.GetFilesContext(lctx, params)
	if err != nil {
		return nil, fmt.Errorf("files.list: %w", err)
	}
	var found []slack.File
	for _, f := range files {
		if f.User == botID && strings.HasPrefix(f.Name, prefix) {
			found = append(found, f)
		}
	}
	return found, nil
}

// uploadFile uploads preview as a file named name, shared nowhere (the unfurl
// shares it into the message's conversation), and returns its ID.
func (b *Bridge) uploadFile(ctx context.Context, name, title string, preview []byte) (string, error) {
	uctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
	defer cancel()
	up, err := b.api.GetUploadURLExternalContext(uctx, slack.GetUploadURLExternalParameters{
		FileName: name,
		FileSize: len(preview),
		AltTxt:   title,
	})
	if err != nil {
		return "", fmt.Errorf("files.getUploadURLExternal: %s", slackErr(err))
	}
	if err := b.api.UploadToURL(uctx, slack.UploadToURLParameters{
		UploadURL: up.UploadURL,
		Reader:    bytes.NewReader(preview),
		Filename:  name,
	}); err != nil {
		return "", fmt.Errorf("upload: %s", slackErr(err))
	}
	if _, err := b.api.CompleteUploadExternalContext(uctx, slack.CompleteUploadExternalParameters{
		Files: []slack.FileSummary{{ID: up.FileID, Title: title}},
	}); err != nil {
		return "", fmt.Errorf("files.completeUploadExternal: %s", slackErr(err))
	}
	return up.FileID, nil
}

// uploadReady reports whether Slack has processed the upload backing sh:
// until then files.info reports no image dimensions.
func (b *Bridge) uploadReady(ctx context.Context, sh pendingShare) (bool, error) {
	file, _, _, err := b.api.GetFileInfoContext(ctx, sh.fileID, 0, 0)
	if err != nil {
		return false, err
	}
	return file != nil && file.OriginalW > 0, nil
}

// deleteStale deletes the uploads that backed sh before its current card
// replaced them. Failures are only logged: a stale upload left behind is
// harmless, if untidy.
func (b *Bridge) deleteStale(ctx context.Context, sh pendingShare) {
	for _, id := range sh.stale {
		dctx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout.Duration)
		err := b.api.DeleteFileContext(dctx, id)
		cancel()
		if err != nil {
			b.log.Printf("deleting superseded upload %s of %q failed: %s", id, sh.id, slackErr(err))
		}
	}
}
