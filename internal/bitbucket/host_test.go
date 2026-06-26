package bitbucket

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestReplyToReviewCommentUnsupported(t *testing.T) {
	t.Parallel()
	h := &Host{}
	if err := h.ReplyToReviewComment(context.Background(), 1, 2, "body"); !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("ReplyToReviewComment() error = %v, want ErrUnsupported", err)
	}
}
