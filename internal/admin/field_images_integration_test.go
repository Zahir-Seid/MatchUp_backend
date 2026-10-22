//go:build integration

package admin_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/admin"
	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/media"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

func TestCreateFieldClaimsImagesAndRollsBackOnFailure(t *testing.T) {
	store := testsupport.Store(t)
	log := slog.New(slog.DiscardHandler)
	mediaSvc := media.NewService(media.Options{Store: store, PublicBaseURL: "https://api.example.test"})
	adminSvc := admin.NewService(admin.Options{
		Store:  store,
		Audit:  audit.NewRecorder(store, log),
		Media:  mediaSvc,
		Logger: log,
	})
	owner := testsupport.User(t, store)
	other := testsupport.User(t, store)
	ctx := context.Background()

	first := stageImage(t, mediaSvc, owner.ID)
	second := stageImage(t, mediaSvc, owner.ID)

	field, err := adminSvc.CreateField(ctx, owner.ID, admin.FieldInput{
		Name:           "Atlas Arena",
		Location:       "Addis Ababa",
		ImageIDs:       []uuid.UUID{first.ID, second.ID},
		PrimaryImageID: second.ID,
	})
	if err != nil {
		t.Fatalf("create field: %v", err)
	}
	if len(field.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(field.Images))
	}
	if !field.Images[1].IsPrimary || field.ImageURL == nil {
		t.Fatalf("primary image = %+v, url %v", field.Images[1], field.ImageURL)
	}
	if *field.ImageURL != field.Images[1].URL {
		t.Fatalf("image URL = %q, want primary %q", *field.ImageURL, field.Images[1].URL)
	}

	replacement := stageImage(t, mediaSvc, owner.ID)
	ids := []uuid.UUID{replacement.ID}
	primary := replacement.ID
	updated, err := adminSvc.UpdateField(ctx, owner.ID, field.ID, admin.FieldPatch{
		ImageIDs:       &ids,
		PrimaryImageID: &primary,
	})
	if err != nil {
		t.Fatalf("replace images: %v", err)
	}
	if len(updated.Images) != 1 || updated.Images[0].ID != replacement.ID {
		t.Fatalf("updated images = %+v", updated.Images)
	}

	stolen := stageImage(t, mediaSvc, other.ID)
	name := "Rollback Field"
	_, err = adminSvc.CreateField(ctx, owner.ID, admin.FieldInput{
		Name:           name,
		Location:       "Addis Ababa",
		ImageIDs:       []uuid.UUID{stolen.ID},
		PrimaryImageID: stolen.ID,
	})
	var httpErr *httpx.Error
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create with another admin's image = %v, want 422", err)
	}

	var count int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM fields WHERE name = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count rolled back field: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed create left %d fields behind", count)
	}

	// The empty gallery is rejected before anything is written.
	_, err = adminSvc.CreateField(ctx, owner.ID, admin.FieldInput{Name: name, Location: "Addis Ababa"})
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create without images = %v, want 422", err)
	}
}

func stageImage(t *testing.T, svc *media.Service, actor uuid.UUID) *media.StagedImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 200; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	staged, err := svc.Upload(context.Background(), actor, &buf)
	if err != nil {
		t.Fatalf("stage image: %v", err)
	}
	return staged
}
