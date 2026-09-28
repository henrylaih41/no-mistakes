package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitChange commits files on top of HEAD and returns base and head SHAs.
func commitChange(t *testing.T, dir string, files map[string]string) (string, string) {
	t.Helper()
	base := gitOutput(t, dir, "rev-parse", "HEAD")
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, dir, name, content)
	}
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "change")
	return base, gitOutput(t, dir, "rev-parse", "HEAD")
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func lines(n int) string {
	return strings.Repeat("x\n", n)
}

func TestClassifySize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files map[string]string
		want  SizeTier
	}{
		{"one under the cutoff is small", map[string]string{"a.go": lines(99)}, SizeTier{Small: true, Lines: 99}},
		{"at the cutoff is standard", map[string]string{"a.go": lines(100)}, SizeTier{Lines: 100}},
		{"over the cutoff is standard", map[string]string{"a.go": lines(60), "b.go": lines(41)}, SizeTier{Lines: 101}},
		{"a large docs-only diff is small", map[string]string{"guide.md": lines(300), "docs/api.yaml": lines(200), "notes.txt": lines(50), "x.rst": lines(10)}, SizeTier{Small: true, Lines: 560, DocsOnly: true}},
		{"docs plus one code line is not docs-only", map[string]string{"guide.md": lines(300), "a.go": lines(1)}, SizeTier{Lines: 301}},
		{"a binary file is a changed file with 0 lines", map[string]string{"logo.bin": "\x00\x01\x02\x00", "a.go": lines(3)}, SizeTier{Small: true, Lines: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			initGitRepo(t, dir)
			base, head := commitChange(t, dir, tc.files)
			got, err := ClassifySize(context.Background(), dir, base, head, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ClassifySize = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSizeTierString(t *testing.T) {
	t.Parallel()
	for tier, want := range map[SizeTier]string{
		{Small: true, Lines: 37}:                  "small (37 lines)",
		{Lines: 412}:                              "standard (412 lines)",
		{Small: true, Lines: 612, DocsOnly: true}: "small (docs-only, 612 lines)",
	} {
		if got := tier.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", tier, got, want)
		}
	}
}
