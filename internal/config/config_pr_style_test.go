package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGlobalFromBytes_PRStyleFileExpandsHomeAndReachesTheMergedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	global, err := LoadGlobalFromBytes([]byte("pr:\n  style_file: ~/skills/clear-writing/SKILL.md\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() error = %v", err)
	}
	want := filepath.Join(home, "skills", "clear-writing", "SKILL.md")
	if got := Merge(global, &RepoConfig{}).PR.StyleFile; got != want {
		t.Fatalf("merged PR.StyleFile = %q, want %q", got, want)
	}
}

func TestLoadGlobalFromBytes_PRStyleFileUnsetByDefault(t *testing.T) {
	t.Parallel()
	global, err := LoadGlobalFromBytes([]byte("pr:\n  style_file: \"  \"\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() error = %v", err)
	}
	if got := Merge(global, &RepoConfig{}).PR.StyleFile; got != "" {
		t.Fatalf("merged PR.StyleFile = %q, want empty", got)
	}
}

func TestLoadGlobalFromBytes_PRStyleFileRejectsRelativePath(t *testing.T) {
	t.Parallel()
	_, err := LoadGlobalFromBytes([]byte(fmt.Sprintf("pr:\n  style_file: %q\n", "docs/style.md")))
	want := `pr.style_file "docs/style.md" must be an absolute or ~/ path`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadGlobalFromBytes() error = %v, want containing %q", err, want)
	}
}
