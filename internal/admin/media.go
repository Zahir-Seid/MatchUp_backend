package admin

import (
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/Zahir-Seid/MatchUp_backend/internal/media"
)

// StageImage validates, re-encodes and holds an image until a field claims it.
func (s *Service) StageImage(ctx context.Context, actor uuid.UUID, r io.Reader) (*media.StagedImage, error) {
	if s.media == nil {
		return nil, fmt.Errorf("stage image: image service is not configured")
	}
	return s.media.Upload(ctx, actor, r)
}

// DeleteStagedImage removes the caller's unclaimed upload.
func (s *Service) DeleteStagedImage(ctx context.Context, actor, id uuid.UUID) error {
	if s.media == nil {
		return fmt.Errorf("delete staged image: image service is not configured")
	}
	return s.media.DeleteStaged(ctx, actor, id)
}
