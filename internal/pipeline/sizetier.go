package pipeline

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// SizeTier is a run's change size, classified once on the submitted diff.
// A small change is reviewed at size_tiers.small_review_effort and, with live
// validation off, may end the Test step after a passing commands.test.
type SizeTier struct {
	Small    bool
	Lines    int
	DocsOnly bool
}

// String is the one-line rendering the run log, PR body and axi share.
func (t SizeTier) String() string {
	name := "standard"
	if t.Small {
		name = "small"
	}
	if t.DocsOnly {
		return fmt.Sprintf("%s (docs-only, %d lines)", name, t.Lines)
	}
	return fmt.Sprintf("%s (%d lines)", name, t.Lines)
}

// ClassifySize measures base..head: lines are additions plus deletions from
// git's numstat (a binary file counts as a changed file with 0 lines), and the
// change is docs-only when every changed path is prose. A change is small when
// it is docs-only or has fewer than smallMaxLines changed lines.
func ClassifySize(ctx context.Context, workDir, base, head string, smallMaxLines int) (SizeTier, error) {
	_, lines, err := git.DiffStat(ctx, workDir, base, head)
	if err != nil {
		return SizeTier{}, fmt.Errorf("measure change size: %w", err)
	}
	names, err := git.Run(ctx, workDir, "diff", "--name-only", "-z", "--no-renames", base+".."+head)
	if err != nil {
		return SizeTier{}, fmt.Errorf("list changed files: %w", err)
	}
	docsOnly := false
	for _, name := range strings.Split(names, "\x00") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if !isDocsPath(name) {
			docsOnly = false
			break
		}
		docsOnly = true
	}
	return SizeTier{Small: docsOnly || lines < smallMaxLines, Lines: lines, DocsOnly: docsOnly}, nil
}

func isDocsPath(name string) bool {
	if name == "docs" || strings.HasPrefix(name, "docs/") {
		return true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".rst", ".txt":
		return true
	}
	return false
}
