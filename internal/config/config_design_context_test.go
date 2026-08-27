package config

import "testing"

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
