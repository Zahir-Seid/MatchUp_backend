//go:build integration

package media

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

func TestUploadAttachServeReplaceAndCleanup(t *testing.T) {
	store := testsupport.Store(t)
	owner := testsupport.User(t, store)
	other := testsupport.User(t, store)
	field := testsupport.Field(t, store)

	svc := NewService(Options{Store: store, PublicBaseURL: "https://api.example.test"})
	ctx := context.Background()

	first := uploadTestImage(t, svc, owner.ID)
	second := uploadTestImage(t, svc, owner.ID)

	staged, err := svc.GetPublicImage(ctx, field.ID, first.ID)
	if staged != nil {
		t.Fatalf("staged image must not be public, got %v", staged)
	}
	assertHTTPError(t, err, http.StatusNotFound, httpx.CodeNotFound)

	assertHTTPError(t, svc.DeleteStaged(ctx, other.ID, first.ID), http.StatusNotFound, httpx.CodeNotFound)

	attach := func(ids []uuid.UUID, primary uuid.UUID) error {
		return store.InTx(ctx, func(q *db.Queries) error {
			return svc.ReplaceFieldImages(ctx, q, owner.ID, field.ID, ids, primary)
		})
	}
	if err := attach([]uuid.UUID{first.ID, second.ID}, second.ID); err != nil {
		t.Fatalf("attach images: %v", err)
	}

	gallery, err := svc.ListFieldImages(ctx, field.ID)
	if err != nil {
		t.Fatalf("list gallery: %v", err)
	}
	if len(gallery) != 2 {
		t.Fatalf("gallery length = %d, want 2", len(gallery))
	}
	if !gallery[1].IsPrimary || gallery[0].IsPrimary {
		t.Fatalf("primary flags = %v, %v; want only the second", gallery[0].IsPrimary, gallery[1].IsPrimary)
	}
	wantURL := "https://api.example.test/v1/fields/" + field.ID.String() + "/images/" + second.ID.String()
	if got := PrimaryImageURL(gallery); got == nil || *got != wantURL {
		t.Fatalf("primary URL = %v, want %s", got, wantURL)
	}
	if !strings.HasSuffix(gallery[0].ThumbnailURL, "/thumbnail") {
		t.Fatalf("thumbnail URL = %q", gallery[0].ThumbnailURL)
	}

	full, err := svc.GetPublicImage(ctx, field.ID, second.ID)
	if err != nil {
		t.Fatalf("get public image: %v", err)
	}
	if len(full.Data) == 0 || full.ETag == "" || full.ContentType != "image/jpeg" {
		t.Fatalf("public image = %d bytes, %q, %q", len(full.Data), full.ContentType, full.ETag)
	}
	thumb, err := svc.GetPublicThumbnail(ctx, field.ID, second.ID)
	if err != nil {
		t.Fatalf("get public thumbnail: %v", err)
	}
	if len(thumb.Data) == 0 || thumb.ETag == full.ETag {
		t.Fatalf("thumbnail = %d bytes, etag %q (full %q)", len(thumb.Data), thumb.ETag, full.ETag)
	}

	if err := attach([]uuid.UUID{second.ID}, second.ID); err != nil {
		t.Fatalf("replace gallery: %v", err)
	}
	_, err = svc.GetPublicImage(ctx, field.ID, first.ID)
	assertHTTPError(t, err, http.StatusNotFound, httpx.CodeNotFound)

	pending := uploadTestImage(t, svc, other.ID)
	if err := attach([]uuid.UUID{second.ID, pending.ID}, pending.ID); err == nil {
		t.Fatal("another user's staged image must not be attachable")
	}
	after, err := svc.ListFieldImages(ctx, field.ID)
	if err != nil {
		t.Fatalf("list after failed attach: %v", err)
	}
	if len(after) != 1 || after[0].ID != second.ID {
		t.Fatalf("failed attach must roll back, gallery = %+v", after)
	}

	if _, err := store.Pool().Exec(ctx, `UPDATE field_images SET expires_at = now() - interval '1 hour' WHERE id = $1`, pending.ID); err != nil {
		t.Fatalf("expire staged image: %v", err)
	}
	deleted, err := svc.CleanupExpired(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("cleanup deleted = %d, want 1", deleted)
	}
	assertHTTPError(t, svc.DeleteStaged(ctx, owner.ID, pending.ID), http.StatusNotFound, httpx.CodeNotFound)
}

func uploadTestImage(t *testing.T, svc *Service, actor uuid.UUID) *StagedImage {
	t.Helper()
	raw := encodeJPEG(t, newOpaqueImage(1200, 800))
	image, err := svc.Upload(context.Background(), actor, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if image.ID == uuid.Nil || image.ExpiresAt.Before(time.Now()) {
		t.Fatalf("staged image = %+v", image)
	}
	return image
}
