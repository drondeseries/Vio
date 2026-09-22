package scanner

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// scanRootIgnoreRules loads the rules above a scoped folder walk, stopping at
// its configured library root. Rules outside that root cannot affect a library.
// A failed ancestor read makes the scope incomplete, so callers must protect it
// from missing-file reconciliation.
func scanRootIgnoreRules(path string, libraryRoots []string) ([]ignoreRules, bool, error) {
	path = filepath.Clean(path)
	root := ""
	for _, candidate := range libraryRoots {
		candidate = filepath.Clean(candidate)
		if pathWithinAnyRoot(path, []string{candidate}) && len(candidate) > len(root) {
			root = candidate
		}
	}
	if root == "" || root == path {
		return nil, false, nil
	}
	var parents []string
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		parents = append(parents, dir)
		if dir == root {
			break
		}
	}
	var rules []ignoreRules
	for i := len(parents) - 1; i >= 0; i-- {
		dir := parents[i]
		if ignoreRulesMatch(rules, dir) {
			return rules, true, nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, false, fmt.Errorf("read ignore ancestor %s: %w", dir, err)
		}
		if dirHasIgnoreMarker(entries) {
			return rules, true, nil
		}
		rules = childIgnoreRules(rules, dir, dir, entries)
	}
	return rules, ignoreRulesMatch(rules, path), nil
}

// Filesystem ignore conventions honored during scans:
//
//   - .ignore / .nomedia: a directory containing either marker file is skipped
//     entirely, together with everything under it. No pattern semantics.
//   - .siloignore: per-directory glob file with Plex .plexignore semantics —
//     patterns are matched against paths relative to the directory holding the
//     file, apply to that directory and every descendant, nested files stack
//     with inherited ones, and a pattern matching a directory name prunes the
//     whole subtree.

const (
	ignoreMarkerIgnore  = ".ignore"
	ignoreMarkerNoMedia = ".nomedia"
	siloIgnoreFileName  = ".siloignore"
)

// ignoreRules is one parsed .siloignore: the logical path of the directory it
// lives in plus its glob patterns (filepath.Match semantics, so `*` matches
// within one path segment and `/` is literal).
type ignoreRules struct {
	basePath string
	patterns []string
}

// dirHasIgnoreMarker reports whether the entries contain an ignore marker
// file. Only regular files count: a directory or symlink named .ignore or
// .nomedia is not a marker.
func dirHasIgnoreMarker(entries []fs.DirEntry) bool {
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if entry.Name() == ignoreMarkerIgnore || entry.Name() == ignoreMarkerNoMedia {
			return true
		}
	}
	return false
}

// parseIgnorePatterns converts .siloignore file content into match patterns.
// Blank lines and `#` comments are dropped, the rest is kept verbatim.
func parseIgnorePatterns(content string) []string {
	var patterns []string
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// ignoreRulesMatch reports whether logicalPath matches any inherited rule.
// Patterns apply to the entry's own path relative to the rule's directory;
// matching directories are pruned by the caller, which is what excludes whole
// subtrees.
func ignoreRulesMatch(rules []ignoreRules, logicalPath string) bool {
	for _, rule := range rules {
		rel, err := filepath.Rel(rule.basePath, logicalPath)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for _, pattern := range rule.patterns {
			if ok, _ := filepath.Match(pattern, rel); ok {
				return true
			}
		}
	}
	return false
}

// childIgnoreRules returns the rule set children of a directory inherit: the
// inherited rules plus this directory's own .siloignore, if present. Only a
// regular file counts as the pattern file. A read failure leaves inherited
// rules intact and lets the walk continue.
func childIgnoreRules(inherited []ignoreRules, dirLogicalPath, dirPhysicalPath string, entries []fs.DirEntry) []ignoreRules {
	for _, entry := range entries {
		if !entry.Type().IsRegular() || entry.Name() != siloIgnoreFileName {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dirPhysicalPath, siloIgnoreFileName))
		if err != nil {
			return inherited
		}
		patterns := parseIgnorePatterns(string(content))
		if len(patterns) == 0 {
			return inherited
		}
		return append(inherited, ignoreRules{basePath: dirLogicalPath, patterns: patterns})
	}
	return inherited
}
