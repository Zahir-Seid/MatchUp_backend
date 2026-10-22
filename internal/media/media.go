// Package media owns field artwork: upload validation and re-encoding, staged
// uploads that are claimed when a field is saved, and the public byte stream.
//
// Uploads are never stored as received. Every image is decoded, bounded,
// re-encoded without metadata, and stored together with a smaller thumbnail.
// The original filename, declared content type and EXIF block do not survive
// the request.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp" // registers the WebP decoder used by image.Decode

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

// Bounds on what may be uploaded and how many images a field may carry.
const (
	MaxUploadBytes     = 5 << 20 // 5 MiB, checked against the file part itself
	MaxImagePixels     = 24_000_000
	MaxImageEdge       = 8192
	MaxImagesPerField  = 5
	MaxPendingPerAdmin = 10
	StagedTTL          = 24 * time.Hour

	displayMaxEdge   = 2400
	thumbnailMaxEdge = 640
	jpegQuality      = 85
)

// Service validates, stores and serves field images. It is safe for concurrent
// use; heavy image decoding is capped so a burst of uploads cannot exhaust the
// process.
type Service struct {
	store         *db.Store
	publicBaseURL string
	log           *slog.Logger
	decodeSlots   chan struct{}
}

// Options configures the image service.
type Options struct {
	Store *db.Store
	// PublicBaseURL is the origin clients use, for example
	// http://localhost:8080. Empty yields relative /v1 URLs, which is useful in
	// tests and behind a same-origin proxy.
	PublicBaseURL string
	Logger        *slog.Logger
}

// NewService builds the image service.
func NewService(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:         opts.Store,
		publicBaseURL: strings.TrimRight(strings.TrimSpace(opts.PublicBaseURL), "/"),
		log:           log,
		decodeSlots:   make(chan struct{}, 2),
	}
}

// Image is an attached image as the API renders it.
type Image struct {
	ID           uuid.UUID `json:"id"`
	URL          string    `json:"url"`
	ThumbnailURL string    `json:"thumbnail_url"`
	ContentType  string    `json:"content_type"`
	Width        int32     `json:"width"`
	Height       int32     `json:"height"`
	ByteSize     int32     `json:"byte_size"`
	IsPrimary    bool      `json:"is_primary"`
	Position     int16     `json:"position"`
} //	@name	FieldImage

// StagedImage is the response to an upload before the field is saved. It has no
// public URL yet: staged bytes are invisible until a field claims them.
type StagedImage struct {
	ID                uuid.UUID `json:"id"`
	ContentType       string    `json:"content_type"`
	Width             int32     `json:"width"`
	Height            int32     `json:"height"`
	ByteSize          int32     `json:"byte_size"`
	ThumbnailByteSize int32     `json:"thumbnail_byte_size"`
	ExpiresAt         time.Time `json:"expires_at"`
} //	@name	StagedFieldImage

// ImageBytes is one public image response.
type ImageBytes struct {
	Data        []byte
	ContentType string
	ETag        string
}

// Upload validates and re-encodes one file, then stages it for the uploading
// admin. The returned row is not public and expires after StagedTTL.
func (s *Service) Upload(ctx context.Context, actor uuid.UUID, r io.Reader) (*StagedImage, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return nil, httpx.ErrBadRequest.WithCause(err)
	}
	if len(raw) == 0 {
		return nil, httpx.ErrValidation.WithField("file", "an image file is required")
	}
	if len(raw) > MaxUploadBytes {
		return nil, ErrTooLarge
	}

	processed, err := s.process(ctx, raw)
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().UTC().Add(StagedTTL)
	uploader := actor
	row, err := s.store.CreateFieldImage(ctx, db.CreateFieldImageParams{
		UploadedBy:        &uploader,
		Data:              processed.display,
		ThumbnailData:     processed.thumbnail,
		ContentType:       processed.contentType,
		Width:             processed.width,
		Height:            processed.height,
		ByteSize:          int32(len(processed.display)),
		ThumbnailByteSize: int32(len(processed.thumbnail)),
		DataSha256:        processed.displaySHA256,
		ThumbnailSha256:   processed.thumbnailSHA256,
		ExpiresAt:         expiresAt,
		MaxPending:        MaxPendingPerAdmin,
	})
	if err != nil {
		if db.IsNoRows(err) {
			return nil, errTooManyPending
		}
		return nil, fmt.Errorf("stage image: %w", err)
	}

	return &StagedImage{
		ID:                row.ID,
		ContentType:       row.ContentType,
		Width:             row.Width,
		Height:            row.Height,
		ByteSize:          row.ByteSize,
		ThumbnailByteSize: row.ThumbnailByteSize,
		ExpiresAt:         expiresAt,
	}, nil
}

// DeleteStaged removes an upload that has not been attached to a field. It is
// owner-scoped, so one admin cannot delete another's pending file.
func (s *Service) DeleteStaged(ctx context.Context, actor, id uuid.UUID) error {
	deleted, err := s.store.DeleteStagedFieldImage(ctx, db.DeleteStagedFieldImageParams{
		ID:         id,
		UploadedBy: &actor,
	})
	if err != nil {
		return fmt.Errorf("delete staged image: %w", err)
	}
	if deleted == 0 {
		return httpx.ErrNotFound
	}
	return nil
}

// ReplaceFieldImages makes ids the field's complete image set in the given
// order, removes anything omitted, and marks primaryID primary. Every new id
// must be an unexpired staged image uploaded by actor; existing images on the
// field may be reordered by any admin.
//
// It runs inside the caller's transaction so a field and its artwork commit
// together.
func (s *Service) ReplaceFieldImages(ctx context.Context, q *db.Queries, actor, fieldID uuid.UUID, ids []uuid.UUID, primaryID uuid.UUID) error {
	if err := validateSet(ids, primaryID); err != nil {
		return err
	}

	if err := q.ClearFieldImagePrimary(ctx, &fieldID); err != nil {
		return fmt.Errorf("clear primary image: %w", err)
	}
	if _, err := q.DeleteFieldImagesNotIn(ctx, db.DeleteFieldImagesNotInParams{
		FieldID: &fieldID,
		KeepIds: ids,
	}); err != nil {
		return fmt.Errorf("remove omitted images: %w", err)
	}

	for position, id := range ids {
		placed, err := q.PlaceFieldImage(ctx, db.PlaceFieldImageParams{
			FieldID:    &fieldID,
			Position:   int16(position),
			ID:         id,
			UploadedBy: &actor,
		})
		if err != nil {
			return fmt.Errorf("attach image %s: %w", id, err)
		}
		if placed != 1 {
			return httpx.ErrValidation.WithField("image_ids",
				"one or more images no longer exist or have expired; upload them again")
		}
	}

	primary, err := q.SetFieldImagePrimary(ctx, db.SetFieldImagePrimaryParams{
		ID:      primaryID,
		FieldID: &fieldID,
	})
	if err != nil {
		return fmt.Errorf("set primary image: %w", err)
	}
	if primary != 1 {
		return httpx.ErrValidation.WithField("primary_image_id",
			"must be one of the field's images")
	}
	return nil
}

func validateSet(ids []uuid.UUID, primaryID uuid.UUID) error {
	if len(ids) == 0 {
		return httpx.ErrValidation.WithField("image_ids", "at least one image is required")
	}
	if len(ids) > MaxImagesPerField {
		return httpx.ErrValidation.WithField("image_ids",
			fmt.Sprintf("at most %d images are allowed", MaxImagesPerField))
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	foundPrimary := false
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			return httpx.ErrValidation.WithField("image_ids", "must not contain duplicate images")
		}
		seen[id] = struct{}{}
		if id == primaryID {
			foundPrimary = true
		}
	}
	if !foundPrimary {
		return httpx.ErrValidation.WithField("primary_image_id", "must be one of the images")
	}
	return nil
}

// ListFieldImages returns a field's gallery in display order.
func (s *Service) ListFieldImages(ctx context.Context, fieldID uuid.UUID) ([]Image, error) {
	return s.ListFieldImagesTx(ctx, s.store.Queries, fieldID)
}

// ListFieldImagesTx reads a field's gallery through the caller's transaction,
// so a field and the images just attached to it are visible together.
func (s *Service) ListFieldImagesTx(ctx context.Context, q *db.Queries, fieldID uuid.UUID) ([]Image, error) {
	rows, err := q.ListFieldImages(ctx, &fieldID)
	if err != nil {
		return nil, fmt.Errorf("list field images: %w", err)
	}
	images := make([]Image, 0, len(rows))
	for _, row := range rows {
		images = append(images, s.image(
			row.ID, row.FieldID, row.ContentType, row.Width, row.Height,
			row.ByteSize, row.IsPrimary, row.Position,
		))
	}
	return images, nil
}

// FieldImageMap fetches galleries for many fields in one query, keyed by field.
func (s *Service) FieldImageMap(ctx context.Context, fieldIDs []uuid.UUID) (map[uuid.UUID][]Image, error) {
	if len(fieldIDs) == 0 {
		return map[uuid.UUID][]Image{}, nil
	}
	rows, err := s.store.ListFieldImagesForFields(ctx, fieldIDs)
	if err != nil {
		return nil, fmt.Errorf("list images for fields: %w", err)
	}
	byField := make(map[uuid.UUID][]Image, len(fieldIDs))
	for _, row := range rows {
		if row.FieldID == nil {
			continue
		}
		byField[*row.FieldID] = append(byField[*row.FieldID], s.image(
			row.ID, row.FieldID, row.ContentType, row.Width, row.Height,
			row.ByteSize, row.IsPrimary, row.Position,
		))
	}
	return byField, nil
}

func (s *Service) image(id uuid.UUID, fieldID *uuid.UUID, contentType string, width, height, byteSize int32, isPrimary bool, position int16) Image {
	var field uuid.UUID
	if fieldID != nil {
		field = *fieldID
	}
	return Image{
		ID:           id,
		URL:          s.imageURL(field, id, false),
		ThumbnailURL: s.imageURL(field, id, true),
		ContentType:  contentType,
		Width:        width,
		Height:       height,
		ByteSize:     byteSize,
		IsPrimary:    isPrimary,
		Position:     position,
	}
}

func (s *Service) imageURL(fieldID, imageID uuid.UUID, thumbnail bool) string {
	base := strings.TrimRight(s.publicBaseURL, "/")
	prefix := base + "/v1/fields/" + fieldID.String() + "/images/" + imageID.String()
	if strings.HasSuffix(base, "/v1") {
		prefix = base + "/fields/" + fieldID.String() + "/images/" + imageID.String()
	}
	if thumbnail {
		return prefix + "/thumbnail"
	}
	return prefix
}

// PrimaryImageURL returns the primary image's URL, falling back to the first
// image. Nil means the field has no artwork.
func PrimaryImageURL(images []Image) *string {
	if len(images) == 0 {
		return nil
	}
	for _, img := range images {
		if img.IsPrimary {
			url := img.URL
			return &url
		}
	}
	url := images[0].URL
	return &url
}

// GetPublicImage returns the full display copy. Staged images are not visible:
// the query only matches attached rows.
func (s *Service) GetPublicImage(ctx context.Context, fieldID, imageID uuid.UUID) (*ImageBytes, error) {
	row, err := s.store.GetPublicFieldImage(ctx, db.GetPublicFieldImageParams{
		FieldID: &fieldID,
		ID:      imageID,
	})
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get field image: %w", err)
	}
	return &ImageBytes{Data: row.Data, ContentType: row.ContentType, ETag: `"` + row.DataSha256 + `"`}, nil
}

// GetPublicThumbnail returns the small copy used by lists and cards.
func (s *Service) GetPublicThumbnail(ctx context.Context, fieldID, imageID uuid.UUID) (*ImageBytes, error) {
	row, err := s.store.GetPublicFieldThumbnail(ctx, db.GetPublicFieldThumbnailParams{
		FieldID: &fieldID,
		ID:      imageID,
	})
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.ErrNotFound.WithCause(err)
		}
		return nil, fmt.Errorf("get field thumbnail: %w", err)
	}
	return &ImageBytes{Data: row.ThumbnailData, ContentType: row.ContentType, ETag: `"` + row.ThumbnailSha256 + `"`}, nil
}

// CleanupExpired removes staged uploads whose deadline has passed. Attached
// images never expire.
func (s *Service) CleanupExpired(ctx context.Context) (int64, error) {
	deleted, err := s.store.DeleteExpiredStagedFieldImages(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete expired staged images: %w", err)
	}
	return deleted, nil
}

type processedImage struct {
	display         []byte
	thumbnail       []byte
	contentType     string
	width           int32
	height          int32
	displaySHA256   string
	thumbnailSHA256 string
}

// process validates and re-encodes an upload. It is the security boundary:
// nothing from the original file is trusted except the bytes the decoders
// successfully produce.
func (s *Service) process(ctx context.Context, raw []byte) (*processedImage, error) {
	select {
	case s.decodeSlots <- struct{}{}:
		defer func() { <-s.decodeSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	format, ok := detectFormat(raw)
	if !ok {
		return nil, ErrUnsupported
	}
	cfg, decodedFormat, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || decodedFormat != format {
		return nil, ErrUnsupported.WithCause(err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > MaxImageEdge || cfg.Height > MaxImageEdge ||
		int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return nil, ErrDimensions
	}

	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, ErrUnsupported.WithCause(err)
	}

	display := resizeLongEdge(img, displayMaxEdge)
	thumbnail := resizeLongEdge(img, thumbnailMaxEdge)
	usePNG := hasAlpha(img)
	contentType := "image/jpeg"
	if usePNG {
		contentType = "image/png"
	}

	displayBytes, err := encode(display, usePNG)
	if err != nil {
		return nil, fmt.Errorf("encode image: %w", err)
	}
	thumbnailBytes, err := encode(thumbnail, usePNG)
	if err != nil {
		return nil, fmt.Errorf("encode thumbnail: %w", err)
	}

	return &processedImage{
		display:         displayBytes,
		thumbnail:       thumbnailBytes,
		contentType:     contentType,
		width:           int32(display.Bounds().Dx()),
		height:          int32(display.Bounds().Dy()),
		displaySHA256:   digest(displayBytes),
		thumbnailSHA256: digest(thumbnailBytes),
	}, nil
}

func detectFormat(data []byte) (string, bool) {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "jpeg", true
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "png", true
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "webp", true
	default:
		return "", false
	}
}

func resizeLongEdge(img image.Image, max int) image.Image {
	bounds := img.Bounds()
	if bounds.Dx() <= max && bounds.Dy() <= max {
		return img
	}
	return imaging.Fit(img, max, max, imaging.Lanczos)
}

func encode(img image.Image, asPNG bool) ([]byte, error) {
	var buf bytes.Buffer
	if asPNG {
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	if err := jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flatten puts a transparent image on white before JPEG encoding, which has no
// alpha channel. Without this, transparent pixels encode as black.
func flatten(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

func hasAlpha(img image.Image) bool {
	switch im := img.(type) {
	case *image.NRGBA:
		for i := 3; i < len(im.Pix); i += 4 {
			if im.Pix[i] != 0xFF {
				return true
			}
		}
		return false
	case *image.RGBA:
		for i := 3; i < len(im.Pix); i += 4 {
			if im.Pix[i] != 0xFF {
				return true
			}
		}
		return false
	case *image.Paletted:
		for _, c := range im.Palette {
			if _, _, _, a := c.RGBA(); a != 0xFFFF {
				return true
			}
		}
		return false
	case *image.Gray, *image.Gray16, *image.YCbCr, *image.CMYK:
		return false
	}

	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xFFFF {
				return true
			}
		}
	}
	return false
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Error values returned by Upload. They are HTTP errors so the admin handler
// can return them unchanged.
var (
	ErrTooLarge = httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodePayloadTooBig,
		"Image must be 5 MB or smaller.")
	ErrUnsupported = httpx.NewError(http.StatusUnsupportedMediaType, httpx.CodeUnsupported,
		"Use a JPEG, PNG or WebP image.")
	ErrDimensions = httpx.ErrValidation.WithField("file",
		"Image must be at most 24 megapixels and 8192 pixels on each side.")
	errTooManyPending = httpx.NewError(http.StatusConflict, httpx.CodeConflict,
		"Too many images are waiting to be saved. Save or remove some and try again.")
)
