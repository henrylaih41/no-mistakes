package designcontext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeReadsCLIAndRepoFilesDeterministically(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "b.md"), "repo b")
	writeFile(t, filepath.Join(root, "docs", "a.md"), "repo a")
	cli := filepath.Join(t.TempDir(), "design.md")
	writeFile(t, cli, "cli context")

	ctx, err := Materialize(root, []string{cli}, []string{"docs/*.md"})
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	if len(ctx.Files) != 3 {
		t.Fatalf("files = %d, want 3", len(ctx.Files))
	}
	if ctx.Files[0].Source != cli || ctx.Files[0].Content != "cli context" {
		t.Fatalf("cli file = %+v", ctx.Files[0])
	}
	if ctx.Files[1].Source != "docs/a.md" || ctx.Files[2].Source != "docs/b.md" {
		t.Fatalf("repo files not sorted by source: %+v", ctx.Files)
	}
}

func TestMaterializeRepoPathMustStayInsideWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeFile(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(root, "secret.md")); err != nil {
		t.Fatal(err)
	}

	_, err := Materialize(root, nil, []string{"secret.md"})
	if err == nil {
		t.Fatal("expected repo design context symlink outside worktree to fail")
	}
	if !strings.Contains(err.Error(), "outside the worktree") {
		t.Fatalf("error = %v, want outside-worktree message", err)
	}
}

func TestMaterializeRejectsAbsoluteRepoSelectorsOnAnyHost(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, selector := range []string{"/tmp/design.md", `C:\tmp\design.md`, `docs\design.md`, "../design.md"} {
		if _, err := Materialize(root, nil, []string{selector}); err == nil {
			t.Fatalf("Materialize(%q) error = nil, want rejection", selector)
		}
	}
}

func TestMaterializeFailsLoudlyForMissingAndInvalidContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := Materialize(root, nil, []string{"docs/*.md"}); err == nil {
		t.Fatal("expected unmatched repo glob to fail")
	}

	invalid := filepath.Join(root, "invalid.md")
	if err := os.WriteFile(invalid, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(root, nil, []string{"invalid.md"}); err == nil {
		t.Fatal("expected invalid UTF-8 to fail")
	}
}

func TestMaterializeTruncatesWithVisibleMarker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "big.md"), strings.Repeat("x", MaxFileBytes+10))

	ctx, err := Materialize(root, nil, []string{"big.md"})
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	if len(ctx.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(ctx.Files))
	}
	if !ctx.Files[0].Truncated {
		t.Fatal("expected file to be marked truncated")
	}
	if !strings.Contains(ctx.Files[0].Content, "design context truncated") {
		t.Fatalf("missing truncation marker: %q", ctx.Files[0].Content)
	}
}

func TestMaterializeTotalCapUsesIncludedBytesNotOriginalSize(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "big.md"), strings.Repeat("x", MaxFileBytes+10))
	writeFile(t, filepath.Join(root, "small.md"), "still included")

	ctx, err := Materialize(root, nil, []string{"big.md", "small.md"})
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	if len(ctx.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(ctx.Files))
	}
	if !strings.Contains(ctx.Files[1].Content, "still included") {
		t.Fatalf("second file content = %q, want included", ctx.Files[1].Content)
	}
}

func TestPushOptionRoundTrip(t *testing.T) {
	t.Parallel()
	paths := []string{"/tmp/a.md", "/tmp/b.md"}
	opt := FormatPushOption(paths)
	got, err := ParsePushOptions([]string{"no-mistakes.skip=test", opt})
	if err != nil {
		t.Fatalf("ParsePushOptions() error = %v", err)
	}
	if strings.Join(got, "|") != strings.Join(paths, "|") {
		t.Fatalf("paths = %v, want %v", got, paths)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
