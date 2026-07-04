package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestReviewerSourceTag(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"", ""},
		{types.FindingSourceAgent, ""},
		{types.FindingSourceUser, ""},
		{"codex", "[codex]"},
		{"claude", "[claude]"},
	}
	for _, tc := range cases {
		if got := reviewerSourceTag(tc.source); got != tc.want {
			t.Errorf("reviewerSourceTag(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

func TestIsUserSource(t *testing.T) {
	if !isUserSource(types.FindingSourceUser) {
		t.Error("expected the user sentinel to be a user source")
	}
	for _, s := range []string{"", types.FindingSourceAgent, "codex", "claude"} {
		if isUserSource(s) {
			t.Errorf("expected %q not to be treated as a user source", s)
		}
	}
}

func TestRenderFindings_ReviewerSourceTags(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := `{"findings":[
		{"id":"review-codex-1","severity":"warning","file":"a.go","line":1,"description":"codex issue","source":"codex"},
		{"id":"review-claude-1","severity":"error","file":"b.go","line":2,"description":"claude issue","source":"claude"},
		{"id":"user-1","severity":"info","description":"user idea","source":"user"},
		{"id":"agent-1","severity":"info","description":"plain agent finding","source":"agent"}
	],"summary":"4 issues"}`

	plain := stripANSI(renderFindings(raw, 80))

	if !strings.Contains(plain, "[codex]") {
		t.Error("expected a [codex] reviewer tag in the rendered findings")
	}
	if !strings.Contains(plain, "[claude]") {
		t.Error("expected a [claude] reviewer tag in the rendered findings")
	}
	if !strings.Contains(plain, "[user]") {
		t.Error("expected the [user] tag for user-authored findings")
	}
	// The agent sentinel and empty sources must not get an attribution tag.
	if strings.Contains(plain, "[agent]") {
		t.Error("did not expect an [agent] attribution tag")
	}
}

func TestRenderFindings_ReviewerSourceTagFitsBoxContentWidth(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	longRef := "internal/tui/" + strings.Repeat("a", 29) + ".go"
	raw := `{"findings":[{"id":"f1","severity":"warning","file":"` + longRef + `","line":7,"description":"narrow finding","source":"acp:gemini-cli"}]}`
	selected := map[string]bool{"f1": true}

	const narrowBoxWidth = 60
	narrowContentWidth := narrowBoxWidth - 4
	box := stripANSI(renderFindingsBoxForHeight(raw, narrowBoxWidth, 0, selected, 0))
	for _, line := range strings.Split(box, "\n") {
		if !strings.HasPrefix(line, "│ ") || !strings.HasSuffix(line, " │") {
			continue
		}
		content := strings.TrimSuffix(strings.TrimPrefix(line, "│ "), " │")
		content = strings.TrimRight(content, " ")
		if w := lipgloss.Width(content); w > narrowContentWidth {
			t.Fatalf("finding box content line width = %d, want <= %d:\n%s", w, narrowContentWidth, line)
		}
	}

	const wideBoxWidth = 80
	wideContentWidth := wideBoxWidth - 4
	wideContent, _ := renderFindingsWithSelection(raw, wideContentWidth, 0, selected, 0)
	if !strings.Contains(stripANSI(wideContent), longRef+":7") {
		t.Fatalf("expected 80-column box content to keep file ref unchanged, got:\n%s", stripANSI(wideContent))
	}
}
