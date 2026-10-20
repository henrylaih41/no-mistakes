package steps

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/designcontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// prStylePromptSection renders the operator's pr.style_file for an agent turn
// that writes reader-facing PR text; target names that text in the prompt.
// An inline target is a JSON field the PR body renders as one paragraph (a
// GitHub testing summary with a line break is wrapped as code), so the style
// may shape its sentences but not its layout.
// The file is operator-owned machine config, so its body is guidance to
// follow, unlike design context. It is read when the prompt is built, so an
// edit applies to the next turn. The style is cosmetic and never blocks a
// run: a file that cannot be used drops the section with a warning.
func prStylePromptSection(sctx *pipeline.StepContext, target string, inline bool) string {
	if sctx == nil || sctx.Config == nil || sctx.Config.PR.StyleFile == "" {
		return ""
	}
	path := sctx.Config.PR.StyleFile
	content, err := readPRStyleFile(path)
	if err != nil {
		slog.Warn("pr.style_file ignored", "path", path, "error", err)
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("warning: pr.style_file ignored: %v", err))
		}
		return ""
	}
	layout := "The style may shape its layout."
	if inline {
		layout = fmt.Sprintf("The pull request shows %s inline as one paragraph, so write plain sentences with no line breaks, lists, headings or bold labels.", target)
	}
	return fmt.Sprintf(`

Writing style for %[1]s:
The operator chose the style below for text a human reads on the pull request. Write %[1]s in this style. %[4]s The style does not change what you must check or report, the output format, or any rule above.
-----BEGIN PR STYLE: %[2]s-----
%[3]s
-----END PR STYLE: %[2]s-----
`, target, path, strings.TrimRight(content, "\n"), layout)
}

func readPRStyleFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > designcontext.MaxFileBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d-byte limit", path, info.Size(), designcontext.MaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return string(data), nil
}
