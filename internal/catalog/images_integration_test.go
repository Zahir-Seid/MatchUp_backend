//go:build integration

package catalog_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/catalog"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/media"
	"github.com/Zahir-Seid/MatchUp_backend/internal/testsupport"
)

func TestFieldImagesArePublicCacheableAndRangeable(t *testing.T) {
	store := testsupport.Store(t)
	owner := testsupport.User(t, store)
	field := testsupport.Field(t, store)
	ctx := context.Background()

	mediaSvc := media.NewService(media.Options{
		Store:         store,
		PublicBaseURL: "https://api.example.test",
	})
	staged := uploadImage(t, mediaSvc, owner.ID)
	if err := store.InTx(ctx, func(q *db.Queries) error {
		return mediaSvc.ReplaceFieldImages(ctx, q, owner.ID, field.ID, []uuid.UUID{staged.ID}, staged.ID)
	}); err != nil {
		t.Fatalf("attach image: %v", err)
	}

	router := chi.NewRouter()
	catalog.NewHandler(catalog.NewService(store, mediaSvc)).Routes(router)
	path := "/fields/" + field.ID.String() + "/images/" + staged.ID.String()

	res := serve(router, http.MethodGet, path, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, readBody(t, res))
	}
	body := readBody(t, res)
	if res.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("content type = %q", res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("cache control = %q", res.Header.Get("Cache-Control"))
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("nosniff = %q", res.Header.Get("X-Content-Type-Options"))
	}
	etag := res.Header.Get("ETag")
	if etag == "" || len(body) == 0 {
		t.Fatalf("etag = %q, body = %d bytes", etag, len(body))
	}

	conditional := serve(router, http.MethodGet, path, map[string]string{"If-None-Match": etag})
	defer conditional.Body.Close()
	if conditional.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", conditional.StatusCode)
	}

	ranged := serve(router, http.MethodGet, path, map[string]string{"Range": "bytes=0-9"})
	defer ranged.Body.Close()
	if ranged.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", ranged.StatusCode)
	}
	if got := len(readBody(t, ranged)); got != 10 {
		t.Fatalf("range body = %d bytes, want 10", got)
	}

	thumbnail := serve(router, http.MethodGet, path+"/thumbnail", nil)
	defer thumbnail.Body.Close()
	if thumbnail.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail status = %d", thumbnail.StatusCode)
	}
	if got := len(readBody(t, thumbnail)); got == 0 || got >= len(body) {
		t.Fatalf("thumbnail = %d bytes, full = %d", got, len(body))
	}

	unattached := uploadImage(t, mediaSvc, owner.ID)
	hidden := serve(router, http.MethodGet,
		"/fields/"+field.ID.String()+"/images/"+unattached.ID.String(), nil)
	defer hidden.Body.Close()
	if hidden.StatusCode != http.StatusNotFound {
		t.Fatalf("staged image status = %d, want 404", hidden.StatusCode)
	}
}

func serve(router http.Handler, method, path string, headers map[string]string) *http.Response {
	req := httptest.NewRequest(method, path, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Result()
}

func readBody(t *testing.T, res *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func uploadImage(t *testing.T, svc *media.Service, actor uuid.UUID) *media.StagedImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 70, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	staged, err := svc.Upload(context.Background(), actor, &buf)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return staged
}
