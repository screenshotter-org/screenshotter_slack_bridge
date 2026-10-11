package imageproc

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"
)

// makePNG builds a solid red w*h PNG, so padding (white) is distinguishable
// from the screenshot itself.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 255, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func prepare(t *testing.T, in []byte, maxDim int) []byte {
	t.Helper()
	out, err := PreparePreview(in, maxDim)
	if err != nil {
		t.Fatalf("PreparePreview: %v", err)
	}
	return out
}

func TestPreparePreviewLeavesAnInRangeImageAlone(t *testing.T) {
	in := makePNG(t, 800, 600)
	if out := prepare(t, in, 1600); !bytes.Equal(in, out) {
		t.Error("an image within both bounds should be returned unchanged")
	}
}

func TestPreparePreviewDownscalesToMaxDim(t *testing.T) {
	out := prepare(t, makePNG(t, 2000, 1000), 600)
	if w, h := dims(t, out); w != 600 || h != 300 {
		t.Errorf("got %dx%d, want 600x300", w, h)
	}
}

func TestPreparePreviewDownscalesPortrait(t *testing.T) {
	out := prepare(t, makePNG(t, 1000, 2000), 600)
	if w, h := dims(t, out); w != 300 || h != 600 {
		t.Errorf("got %dx%d, want 300x600", w, h)
	}
}

func TestPreparePreviewUpscalesASmallImageToTheMinimum(t *testing.T) {
	// 2.5x is within the upscale bound, so the short side lands exactly on the
	// minimum and no padding is needed.
	out := prepare(t, makePNG(t, 150, 120), 1600)
	if w, h := dims(t, out); w != 375 || h != MinPreviewDim {
		t.Errorf("got %dx%d, want 375x%d (aspect preserved, no padding)", w, h, MinPreviewDim)
	}
}

func TestPreparePreviewPadsWhatBoundedUpscalingCannotReach(t *testing.T) {
	// A thin crop would need 7.5x to reach the minimum height; the upscale stops
	// at 3x and the rest is padded, so the screenshot stays sharp.
	out := prepare(t, makePNG(t, 900, 40), 0)
	w, h := dims(t, out)
	if w != 2700 || h != MinPreviewDim {
		t.Fatalf("got %dx%d, want 2700x%d (3x upscale, height padded)", w, h, MinPreviewDim)
	}

	img := decode(t, out)
	// Padding is white above and below, the screenshot itself red in the middle.
	if r, _, _, _ := img.At(w/2, 2).RGBA(); r != 0xffff {
		t.Error("the top band should be white padding")
	}
	if _, g, _, _ := img.At(w/2, h/2).RGBA(); g != 0 {
		t.Error("the middle band should be the screenshot, not padding")
	}
	if r, _, _, _ := img.At(w/2, h-3).RGBA(); r != 0xffff {
		t.Error("the bottom band should be white padding")
	}
}

func TestPreparePreviewUpscaleNeverExceedsMaxDim(t *testing.T) {
	// Same thin crop, but the operator's cap is tighter than the 3x upscale:
	// the cap wins and the shortfall is padded instead.
	out := prepare(t, makePNG(t, 900, 40), 1600)
	if w, h := dims(t, out); w != 1600 || h != MinPreviewDim {
		t.Errorf("got %dx%d, want 1600x%d", w, h, MinPreviewDim)
	}
}

func TestPreparePreviewPadsAnImageTooThinToScaleUnderTheCap(t *testing.T) {
	// Already over the cap on its long side, and under the minimum on its short
	// side: it is downscaled to the cap and then padded, never distorted.
	out := prepare(t, makePNG(t, 4000, 100), 1600)
	if w, h := dims(t, out); w != 1600 || h != MinPreviewDim {
		t.Errorf("got %dx%d, want 1600x%d", w, h, MinPreviewDim)
	}
}

func TestPreparePreviewPadsATinyImageOnBothSides(t *testing.T) {
	out := prepare(t, makePNG(t, 20, 20), 1600)
	if w, h := dims(t, out); w != MinPreviewDim || h != MinPreviewDim {
		t.Errorf("got %dx%d, want %dx%d", w, h, MinPreviewDim, MinPreviewDim)
	}
}

// withPNGSize returns a copy of pngData whose header claims w*h pixels. The
// image data no longer matches, which is enough for code that must refuse the
// image from its header alone.
func withPNGSize(t *testing.T, pngData []byte, w, h uint32) []byte {
	t.Helper()
	// The IHDR chunk follows the 8-byte signature: length, type, then the
	// width and height that open its data, and a CRC over type and data.
	out := bytes.Clone(pngData)
	if string(out[12:16]) != "IHDR" {
		t.Fatal("not a PNG starting with IHDR")
	}
	binary.BigEndian.PutUint32(out[16:20], w)
	binary.BigEndian.PutUint32(out[20:24], h)
	binary.BigEndian.PutUint32(out[29:33], crc32.ChecksumIEEE(out[12:29]))
	return out
}

func TestPreparePreviewRejectsBomb(t *testing.T) {
	in := withPNGSize(t, makePNG(t, 10, 10), 7000, 7000) // 49 Mpx > the 40 Mpx cap
	_, err := PreparePreview(in, 1600)
	// A decode error would also fail, but only the header check proves the
	// image was refused before being decoded.
	if err == nil || !strings.Contains(err.Error(), "exceed the allowed maximum") {
		t.Errorf("expected the pixel cap to refuse the image, got %v", err)
	}
}
