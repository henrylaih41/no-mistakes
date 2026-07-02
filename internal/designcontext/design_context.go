package designcontext

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	MaxFileBytes  = 64 * 1024
	MaxTotalBytes = 256 * 1024

	pushOptionPrefix = "no-mistakes.design-context="
)

// ResolveCLIPaths expands and absolutizes explicit --design-context paths from
// the caller's working directory. These are explicit local user input, so they
// may point outside the repository.
func ResolveCLIPaths(cwd string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			return nil, fmt.Errorf("empty design context path")
		}
		expanded, err := expandHome(path)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(cwd, expanded)
		}
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return nil, fmt.Errorf("resolve design context path %q: %w", raw, err)
		}
		out = append(out, filepath.Clean(abs))
	}
	return out, nil
}

// FormatPushOption encodes explicit CLI context file paths for the gate
// post-receive hook. It carries paths, not file contents; the daemon
// materializes the files once at run start.
func FormatPushOption(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	data, _ := json.Marshal(paths)
	return pushOptionPrefix + base64.StdEncoding.EncodeToString(data)
}

// ParsePushOptions extracts explicit CLI design-context paths from forwarded
// git push options. The last occurrence wins.
func ParsePushOptions(options []string) ([]string, error) {
	var paths []string
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, pushOptionPrefix)
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode design context push option: %w", err)
		}
		var decoded []string
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, fmt.Errorf("parse design context push option: %w", err)
		}
		paths = decoded
	}
	return paths, nil
}

// Materialize reads explicit CLI paths and repo-config selectors once,
// returning the immutable design context for a run.
func Materialize(workDir string, cliPaths, repoSelectors []string) (types.DesignContext, error) {
	var refs []fileRef
	for _, path := range cliPaths {
		ref, err := explicitFileRef(path)
		if err != nil {
			return types.DesignContext{}, err
		}
		refs = append(refs, ref)
	}
	repoRefs, err := repoFileRefs(workDir, repoSelectors)
	if err != nil {
		return types.DesignContext{}, err
	}
	refs = append(refs, repoRefs...)

	seen := map[string]bool{}
	files := make([]types.DesignContextFile, 0, len(refs))
	total := 0
	for _, ref := range refs {
		if seen[ref.canonical] {
			continue
		}
		seen[ref.canonical] = true
		file, err := readTextFile(ref.source, ref.canonical, total)
		if err != nil {
			return types.DesignContext{}, err
		}
		total += accountedSourceBytes(file.OriginalBytes, MaxTotalBytes-total)
		files = append(files, file)
	}
	return types.DesignContext{Files: files}, nil
}

type fileRef struct {
	source    string
	canonical string
}

func explicitFileRef(path string) (fileRef, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return fileRef{}, fmt.Errorf("empty design context path")
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return fileRef{}, fmt.Errorf("resolve design context path %q: %w", path, err)
	}
	canonical, err := canonicalRegularFile(abs)
	if err != nil {
		return fileRef{}, fmt.Errorf("design context %q: %w", path, err)
	}
	return fileRef{source: filepath.Clean(abs), canonical: canonical}, nil
}

func repoFileRefs(workDir string, selectors []string) ([]fileRef, error) {
	if len(selectors) == 0 {
		return nil, nil
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree root: %w", err)
	}
	root = filepath.Clean(root)
	var refs []fileRef
	for _, selector := range selectors {
		raw := strings.TrimSpace(selector)
		if err := validateRepoSelector(raw); err != nil {
			return nil, err
		}
		pattern := filepath.Join(root, filepath.FromSlash(raw))
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid design_context.files glob %q: %w", selector, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("design_context.files %q matched no files", selector)
		}
		sort.Strings(matches)
		for _, match := range matches {
			canonical, err := canonicalRegularFile(match)
			if err != nil {
				return nil, fmt.Errorf("design_context.files %q: %w", selector, err)
			}
			if !insideDir(root, canonical) {
				return nil, fmt.Errorf("design_context.files %q resolves outside the worktree: %s", selector, match)
			}
			rel, err := filepath.Rel(root, canonical)
			if err != nil {
				return nil, fmt.Errorf("resolve design context relative path: %w", err)
			}
			refs = append(refs, fileRef{source: filepath.ToSlash(rel), canonical: canonical})
		}
	}
	return refs, nil
}

func validateRepoSelector(selector string) error {
	if selector == "" {
		return fmt.Errorf("empty design_context.files entry")
	}
	if strings.HasPrefix(selector, "~") || strings.HasPrefix(selector, "/") || strings.HasPrefix(selector, `\`) || filepath.IsAbs(selector) || filepath.VolumeName(selector) != "" || strings.Contains(selector, ":") {
		return fmt.Errorf("design_context.files %q must be repository-relative", selector)
	}
	if strings.Contains(selector, `\`) {
		return fmt.Errorf("design_context.files %q must use forward slashes", selector)
	}
	for _, part := range strings.FieldsFunc(selector, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("design_context.files %q must stay inside the repository", selector)
		}
	}
	return nil
}

func canonicalRegularFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		path = resolved
		info, err = os.Stat(path)
		if err != nil {
			return "", err
		}
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func readTextFile(source, path string, totalBefore int) (types.DesignContextFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return types.DesignContextFile{}, fmt.Errorf("read design context %q: %w", source, err)
	}
	if !utf8.Valid(data) {
		return types.DesignContextFile{}, fmt.Errorf("design context %q is not valid UTF-8", source)
	}
	text := string(data)
	limit := MaxFileBytes
	if remaining := MaxTotalBytes - totalBefore; remaining < limit {
		limit = remaining
	}
	file := types.DesignContextFile{
		Source:        source,
		OriginalBytes: int64(len(data)),
	}
	if limit <= 0 {
		file.Content = fmt.Sprintf("[no-mistakes: design context omitted because the total cap of %d bytes was reached before this file]", MaxTotalBytes)
		file.Truncated = true
		return file, nil
	}
	if len(text) > limit {
		file.Content = safePrefix(text, limit) + fmt.Sprintf("\n\n[no-mistakes: design context truncated at %d bytes; original file was %d bytes]", limit, len(data))
		file.Truncated = true
		return file, nil
	}
	file.Content = text
	return file, nil
}

func accountedSourceBytes(original int64, remaining int) int {
	if original <= 0 || remaining <= 0 {
		return 0
	}
	n := int64(MaxFileBytes)
	if int64(remaining) < n {
		n = int64(remaining)
	}
	if original < n {
		n = original
	}
	return int(n)
}

func safePrefix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.ValidString(s[:limit]) {
		limit--
	}
	return s[:limit]
}

func insideDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}
