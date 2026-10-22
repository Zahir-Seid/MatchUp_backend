package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
	"github.com/Zahir-Seid/MatchUp_backend/internal/media"
)

// maxUploadEnvelope leaves room for multipart boundaries and headers around the
// 5 MiB file. The file part itself is checked against media.MaxUploadBytes.
const maxUploadEnvelope = media.MaxUploadBytes + (1 << 20)

// FieldImageUploadResponse mirrors media.StagedImage for the generated schema.
// The handler writes the service value directly; this type exists so swag can
// name the response without reaching across packages.
type FieldImageUploadResponse struct {
	ID                string `json:"id"`
	ContentType       string `json:"content_type"`
	Width             int32  `json:"width"`
	Height            int32  `json:"height"`
	ByteSize          int32  `json:"byte_size"`
	ThumbnailByteSize int32  `json:"thumbnail_byte_size"`
	ExpiresAt         string `json:"expires_at"`
} //	@name	StagedFieldImage

// uploadFieldImage stages one image for a later field create or update.
//
//	@Summary		Upload a field image
//	@Description	Validates and stores one JPEG, PNG or WebP image, up to 5 MiB, and returns a staging id. The image is not public yet: pass its id in image_ids when creating or updating a field. Staged uploads expire after 24 hours.
//	@Tags			fields
//	@Security		BearerAuth
//	@Accept			multipart/form-data
//	@Produce		json
//	@Param			file	formData	file	true	"Image file (JPEG, PNG or WebP; max 5 MiB)"
//	@Success		201		{object}	admin.FieldImageUploadResponse
//	@Failure		400		{object}	httpx.ErrorEnvelope	"Malformed multipart body"
//	@Failure		401		{object}	httpx.ErrorEnvelope
//	@Failure		403		{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		409		{object}	httpx.ErrorEnvelope	"too many images are already pending"
//	@Failure		413		{object}	httpx.ErrorEnvelope	"payload_too_large"
//	@Failure		415		{object}	httpx.ErrorEnvelope	"unsupported_media_type"
//	@Failure		422		{object}	httpx.ErrorEnvelope	"validation_failed"
//	@Failure		429		{object}	httpx.ErrorEnvelope
//	@Failure		500		{object}	httpx.ErrorEnvelope
//	@Router			/field-image-uploads [post]
func (h *Handler) uploadFieldImage(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadEnvelope)
	if err := r.ParseMultipartForm(media.MaxUploadBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || strings.Contains(err.Error(), "request body too large") {
			return media.ErrTooLarge.WithCause(err)
		}
		return httpx.ErrBadRequest.WithCause(err)
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		return httpx.ErrValidation.WithField("file", "exactly one image file is required")
	}
	header := files[0]
	if header.Size > media.MaxUploadBytes {
		return media.ErrTooLarge
	}

	file, err := header.Open()
	if err != nil {
		return httpx.ErrBadRequest.WithCause(err)
	}
	defer file.Close()

	staged, err := h.svc.StageImage(r.Context(), principal.UserID, file)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, staged)
	return nil
}

// deleteFieldImageUpload discards a staged upload.
//
//	@Summary		Discard a staged field image
//	@Description	Deletes one of the caller's unclaimed uploads, for example when the admin cancels the field form. Attached images are not deleted here; remove them by updating the field with the remaining image_ids.
//	@Tags			fields
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path	string	true	"Staged image id (UUID)"
//	@Success		204
//	@Failure		401	{object}	httpx.ErrorEnvelope
//	@Failure		403	{object}	httpx.ErrorEnvelope	"forbidden: the account is not an admin"
//	@Failure		404	{object}	httpx.ErrorEnvelope	"not_found"
//	@Failure		429	{object}	httpx.ErrorEnvelope
//	@Failure		500	{object}	httpx.ErrorEnvelope
//	@Router			/field-image-uploads/{id} [delete]
func (h *Handler) deleteFieldImageUpload(w http.ResponseWriter, r *http.Request) error {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return err
	}
	id, err := parseIDParam(r, "id")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteStagedImage(r.Context(), principal.UserID, id); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}
