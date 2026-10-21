package config

import (
	"strings"
	"testing"
)

func TestTestAgentTurnParsesAutoOffAndDefaultsAuto(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want bool
	}{
		{"", false},
		{"test:\n  agent_turn: auto\n", false},
		{"test:\n  agent_turn: off\n", true},
		{"test:\n  agent_turn: \"off\"\n", true},
	} {
		repo, err := LoadRepoFromBytes([]byte(tc.yaml))
		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(repo, repo, false)).Test.AgentTurnOff; got != tc.want {
			t.Fatalf("%q: agent turn off = %v, want %v", tc.yaml, got, tc.want)
		}
	}
	if _, err := LoadRepoFromBytes([]byte("test:\n  agent_turn: maybe\n")); err == nil || !strings.Contains(err.Error(), "test.agent_turn") {
		t.Fatalf("an unknown mode = %v, want a config error naming the key", err)
	}
	if _, err := LoadRepoFromBytes([]byte("test:\n  agent_turn: off\n  live_validation: on\n")); err == nil || !strings.Contains(err.Error(), "test.agent_turn") {
		t.Fatalf("agent_turn off with live_validation on = %v, want a config error naming the key", err)
	}
	if _, err := LoadRepoFromBytes([]byte("test:\n  agent_turn: auto\n  live_validation: on\n")); err != nil {
		t.Fatalf("agent_turn auto with live_validation on = %v, want it accepted", err)
	}
}

func TestTestAgentTurnIsTrustedOnly(t *testing.T) {
	off := &RepoConfig{Test: TestRaw{AgentTurn: AgentTurnOff}}
	unset := &RepoConfig{}
	for _, tc := range []struct {
		name            string
		pushed, trusted *RepoConfig
		allow           bool
		want            bool
	}{
		{"a pushed branch cannot turn off a trusted off", unset, off, false, true},
		{"nor with the commands opt-in", unset, off, true, true},
		{"a pushed off alone is ignored", off, unset, false, false},
		{"a pushed off alone is ignored with the commands opt-in", off, unset, true, false},
		{"with no trusted copy it is auto", off, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(tc.pushed, tc.trusted, tc.allow)).Test.AgentTurnOff; got != tc.want {
				t.Fatalf("agent turn off = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTestAgentTurnIsRejectedInGlobalConfig(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("test:\n  agent_turn: off\n")); err == nil || !strings.Contains(err.Error(), "repository-only") {
		t.Fatalf("global test.agent_turn = %v, want a repository-only error", err)
	}
}
