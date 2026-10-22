package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

func TestProcessRejectsNonImageBytes(t *testing.T) {
	svc := NewService(Options{})

	cases := map[string][]byte{
		"html":       []byte("<!doctype html><html><body>not an image</body></html>"),
		"svg":        []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`),
		"gif":        []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"empty-ish":  []byte{0xFF, 0xD8, 0xFF},
		"fake webp":  append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte("not really")...),
		"plain text": []byte("hello"),
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.process(context.Background(), raw)
			assertHTTPError(t, err, http.StatusUnsupportedMediaType, httpx.CodeUnsupported)
		})
	}
}

func TestProcessResizesLongEdgeForDisplayAndThumbnail(t *testing.T) {
	svc := NewService(Options{})
	raw := encodeJPEG(t, newOpaqueImage(3000, 10))

	got, err := svc.process(context.Background(), raw)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if got.contentType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", got.contentType)
	}
	if got.width != displayMaxEdge {
		t.Errorf("display width = %d, want %d", got.width, displayMaxEdge)
	}
	if got.height != 8 {
		t.Errorf("display height = %d, want 8", got.height)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(got.thumbnail))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("thumbnail format = %q, want jpeg", format)
	}
	if cfg.Width != thumbnailMaxEdge {
		t.Errorf("thumbnail width = %d, want %d", cfg.Width, thumbnailMaxEdge)
	}
	if got.displaySHA256 == "" || got.thumbnailSHA256 == "" {
		t.Error("digests must be populated for ETags")
	}
}

func TestProcessKeepsTransparencyAsPNG(t *testing.T) {
	svc := NewService(Options{})
	img := image.NewNRGBA(image.Rect(0, 0, 80, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 80; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 40, B: 10, A: uint8(x * 2)})
		}
	}

	got, err := svc.process(context.Background(), encodePNG(t, img))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if got.contentType != "image/png" {
		t.Fatalf("content type = %q, want image/png", got.contentType)
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(got.display)); err != nil || format != "png" {
		t.Fatalf("display decode = %q, %v; want png", format, err)
	}
}

func TestProcessRejectsTooManyPixels(t *testing.T) {
	svc := NewService(Options{})
	raw := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, MaxImageEdge+1, 1)))

	_, err := svc.process(context.Background(), raw)
	assertHTTPError(t, err, http.StatusUnprocessableEntity, httpx.CodeValidation)
}

func TestPrimaryImageURLPrefersPrimaryThenFirst(t *testing.T) {
	images := []Image{
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), URL: "first"},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), URL: "second", IsPrimary: true},
	}
	if got := PrimaryImageURL(images); got == nil || *got != "second" {
		t.Fatalf("PrimaryImageURL = %v, want second", got)
	}
	if got := PrimaryImageURL(images[:1]); got == nil || *got != "first" {
		t.Fatalf("fallback PrimaryImageURL = %v, want first", got)
	}
	if got := PrimaryImageURL(nil); got != nil {
		t.Fatalf("PrimaryImageURL(nil) = %v, want nil", got)
	}
}

func newOpaqueImage(width, height int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func assertHTTPError(t *testing.T, err error, status int, code httpx.Code) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var httpErr *httpx.Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want *httpx.Error", err)
	}
	if httpErr.Status != status || httpErr.Code != code {
		t.Fatalf("error = %d/%s, want %d/%s (%v)", httpErr.Status, httpErr.Code, status, code, err)
	}
}
