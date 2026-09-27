package config

import (
	"strings"
	"testing"
)

func TestTestLiveValidationParsesOnOffAndDefaultsOff(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want bool
	}{
		{"", false},
		{"test:\n  live_validation: off\n", false},
		{"test:\n  live_validation: on\n", true},
		{"test:\n  live_validation: \"on\"\n", true},
	} {
		repo, err := LoadRepoFromBytes([]byte(tc.yaml))
		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(repo, repo, false)).Test.LiveValidation; got != tc.want {
			t.Fatalf("%q: live validation = %v, want %v", tc.yaml, got, tc.want)
		}
	}
	if _, err := LoadRepoFromBytes([]byte("test:\n  live_validation: sometimes\n")); err == nil || !strings.Contains(err.Error(), "test.live_validation") {
		t.Fatalf("an unknown mode = %v, want a config error naming the key", err)
	}
}

func TestTestLiveValidationIsTrustedOnly(t *testing.T) {
	on := &RepoConfig{Test: TestRaw{LiveValidation: LiveValidationOn}}
	off := &RepoConfig{}
	for _, tc := range []struct {
		name            string
		pushed, trusted *RepoConfig
		allow           bool
		want            bool
	}{
		{"a pushed branch cannot turn off a trusted on", off, on, false, true},
		{"nor with the commands opt-in", off, on, true, true},
		{"a pushed on alone is ignored", on, off, false, false},
		{"with no trusted copy it is off", on, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(tc.pushed, tc.trusted, tc.allow)).Test.LiveValidation; got != tc.want {
				t.Fatalf("live validation = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTestLiveValidationIsRejectedInGlobalConfig(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("test:\n  live_validation: on\n")); err == nil || !strings.Contains(err.Error(), "repository-only") {
		t.Fatalf("global test.live_validation = %v, want a repository-only error", err)
	}
}
