package config

import (
	"strings"
	"testing"
)

func TestReviewMaxFixRoundsMergePrecedence(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("review:\n  max_fix_rounds: 3\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}

	inherited := Merge(global, &RepoConfig{})
	if inherited.Review.MaxFixRounds != 3 {
		t.Fatalf("inherited max_fix_rounds = %d, want 3", inherited.Review.MaxFixRounds)
	}

	zero := 0
	overridden := Merge(global, &RepoConfig{Review: ReviewRaw{MaxFixRounds: &zero}})
	if overridden.Review.MaxFixRounds != 0 {
		t.Fatalf("repo override max_fix_rounds = %d, want 0", overridden.Review.MaxFixRounds)
	}
}

func TestReviewMaxFixRoundsRejectsNegativeValues(t *testing.T) {
	for _, load := range []struct {
		name string
		fn   func() error
	}{
		{name: "global", fn: func() error {
			_, err := LoadGlobalFromBytes([]byte("review:\n  max_fix_rounds: -1\n"))
			return err
		}},
		{name: "repo", fn: func() error {
			_, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: -1\n"))
			return err
		}},
	} {
		t.Run(load.name, func(t *testing.T) {
			err := load.fn()
			if err == nil || !strings.Contains(err.Error(), "review.max_fix_rounds") {
				t.Fatalf("error = %v, want review.max_fix_rounds validation", err)
			}
		})
	}
}
