package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestParseSkipPushOptions(t *testing.T) {
	got, err := parseSkipPushOptions([]string{
		"ci.skip",
		"no-mistakes.skip=test,lint",
	})
	if err != nil {
		t.Fatalf("parseSkipPushOptions() error = %v", err)
	}
	want := []types.StepName{types.StepTest, types.StepLint}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestParseSkipPushOptionsRejectsUnknownStep(t *testing.T) {
	_, err := parseSkipPushOptions([]string{"no-mistakes.skip=test,deploy"})
	if err == nil {
		t.Fatal("expected unknown step to fail")
	}
}

func TestNormalizeNotifyGatePathResolvesLegacyDotGate(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "repo123.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(bare); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	t.Setenv("PWD", ".")

	got, err := normalizeNotifyGatePath(".")
	if err != nil {
		t.Fatalf("normalizeNotifyGatePath: %v", err)
	}
	if got == "." || !filepath.IsAbs(got) {
		t.Fatalf("normalizeNotifyGatePath(.) = %q, want absolute path", got)
	}
	want, err := filepath.EvalSymlinks(bare)
	if err != nil {
		want = bare
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		gotResolved = got
	}
	if gotResolved != want {
		t.Fatalf("normalizeNotifyGatePath(.) = %q (resolved %q), want %q", got, gotResolved, want)
	}
}

func TestFormatSkipPushOptions(t *testing.T) {
	got := formatSkipPushOptions([]types.StepName{types.StepTest, types.StepLint})
	want := []string{"no-mistakes.skip=test,lint"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestIntentPushOptionRoundTrip(t *testing.T) {
	// Multi-line, comma- and colon-bearing intent must survive the
	// line-oriented push-option transport intact.
	intent := "add retry to the uploader\n\nwhy: flaky network, commas, colons: ok"
	opt := formatIntentPushOption(intent)
	if opt == "" {
		t.Fatal("formatIntentPushOption returned empty for a non-empty intent")
	}
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", opt})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != intent {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, intent)
	}
}

func TestFormatIntentPushOptionEmpty(t *testing.T) {
	if got := formatIntentPushOption("   "); got != "" {
		t.Fatalf("formatIntentPushOption(blank) = %q, want empty", got)
	}
}

func TestParseIntentPushOptionsNone(t *testing.T) {
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", "ci.skip"})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != "" {
		t.Fatalf("parseIntentPushOptions(no intent) = %q, want empty", got)
	}
}

func TestParseRoutePushOptionPresent(t *testing.T) {
	got, err := parseRoutePushOption([]string{"ci.skip", "no-mistakes.route=parent"})
	if err != nil {
		t.Fatalf("parseRoutePushOption() error = %v", err)
	}
	if got != "parent" {
		t.Fatalf("parseRoutePushOption() = %q, want %q", got, "parent")
	}
}

func TestParseRoutePushOptionAbsent(t *testing.T) {
	got, err := parseRoutePushOption([]string{"no-mistakes.skip=test", "ci.skip"})
	if err != nil {
		t.Fatalf("parseRoutePushOption() error = %v", err)
	}
	if got != "" {
		t.Fatalf("parseRoutePushOption(no route) = %q, want empty", got)
	}
}

func TestParseRoutePushOptionMultipleLastWins(t *testing.T) {
	got, err := parseRoutePushOption([]string{"no-mistakes.route=fork", "no-mistakes.route=parent"})
	if err != nil {
		t.Fatalf("parseRoutePushOption() error = %v", err)
	}
	if got != "parent" {
		t.Fatalf("parseRoutePushOption(multiple) = %q, want last %q", got, "parent")
	}
}

func TestParseRoutePushOptionEmptyValueRejected(t *testing.T) {
	if _, err := parseRoutePushOption([]string{"no-mistakes.route="}); err == nil {
		t.Fatal("expected empty route name to fail")
	}
	if _, err := parseRoutePushOption([]string{"no-mistakes.route=   "}); err == nil {
		t.Fatal("expected blank route name to fail")
	}
}

func TestParseRoutePushOptionGarbageRejected(t *testing.T) {
	for _, bad := range []string{
		"no-mistakes.route=has space",
		"no-mistakes.route=../escape",
		"no-mistakes.route=-leadingdash",
	} {
		if _, err := parseRoutePushOption([]string{bad}); err == nil {
			t.Fatalf("expected garbage route %q to fail", bad)
		}
	}
}
