package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGlobalFromBytes_DesignContextParses(t *testing.T) {
	t.Parallel()
	contextPath := filepath.Join(t.TempDir(), "contract.md")
	cfg, err := LoadGlobalFromBytes([]byte(fmt.Sprintf("design_context:\n  files:\n    - %q\n", contextPath)))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() error = %v", err)
	}
	if len(cfg.DesignContext.Files) != 1 || cfg.DesignContext.Files[0] != contextPath {
		t.Fatalf("design_context.files = %v, want [%s]", cfg.DesignContext.Files, contextPath)
	}
}

func TestLoadGlobalFromBytes_DesignContextExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cfg, err := LoadGlobalFromBytes([]byte("design_context:\n  files:\n    - ~/contract.md\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes() error = %v", err)
	}
	want := filepath.Join(home, "contract.md")
	if len(cfg.DesignContext.Files) != 1 || cfg.DesignContext.Files[0] != want {
		t.Fatalf("design_context.files = %v, want [%s]", cfg.DesignContext.Files, want)
	}
}

func TestLoadGlobalFromBytes_DesignContextRejectsInvalidPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "relative", path: "docs/contract.md", want: `design_context.files "docs/contract.md" must be an absolute or ~/ path`},
		{name: "empty", path: "  ", want: "empty design_context.files entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadGlobalFromBytes([]byte(fmt.Sprintf("design_context:\n  files:\n    - %q\n", tt.path)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadGlobalFromBytes() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMergeCarriesGlobalAndRepoDesignContextSeparately(t *testing.T) {
	t.Parallel()
	global := &GlobalConfig{DesignContext: DesignContextRaw{Files: []string{"/machine/QUALITY.md"}}}
	repo := &RepoConfig{DesignContext: DesignContextRaw{Files: []string{"docs/design.md"}}}

	merged := Merge(global, repo)

	if len(merged.DesignContext.GlobalFiles) != 1 || merged.DesignContext.GlobalFiles[0] != "/machine/QUALITY.md" {
		t.Fatalf("global files = %v, want [/machine/QUALITY.md]", merged.DesignContext.GlobalFiles)
	}
	if len(merged.DesignContext.Files) != 1 || merged.DesignContext.Files[0] != "docs/design.md" {
		t.Fatalf("repo files = %v, want [docs/design.md]", merged.DesignContext.Files)
	}
}

func TestLoadRepoFromBytes_DesignContextParses(t *testing.T) {
	t.Parallel()
	cfg, err := LoadRepoFromBytes([]byte(`
design_context:
  files:
    - docs/design/*.md
    - " docs/rulings.md "
`))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes() error = %v", err)
	}
	if len(cfg.DesignContext.Files) != 2 {
		t.Fatalf("files = %v, want 2", cfg.DesignContext.Files)
	}

	merged := Merge(&GlobalConfig{}, cfg)
	want := []string{"docs/design/*.md", "docs/rulings.md"}
	if len(merged.DesignContext.Files) != len(want) {
		t.Fatalf("merged files = %v, want %v", merged.DesignContext.Files, want)
	}
	for i := range want {
		if merged.DesignContext.Files[i] != want[i] {
			t.Fatalf("merged files = %v, want %v", merged.DesignContext.Files, want)
		}
	}
}

func TestEffectiveRepoConfig_KeepsPushedDesignContext(t *testing.T) {
	t.Parallel()
	pushed := &RepoConfig{DesignContext: DesignContextRaw{Files: []string{"docs/design.md"}}}
	trusted := &RepoConfig{DesignContext: DesignContextRaw{Files: []string{"docs/default.md"}}}

	got := EffectiveRepoConfig(pushed, trusted, false)

	if len(got.DesignContext.Files) != 1 || got.DesignContext.Files[0] != "docs/design.md" {
		t.Fatalf("design_context = %v, want pushed design context", got.DesignContext.Files)
	}
}
