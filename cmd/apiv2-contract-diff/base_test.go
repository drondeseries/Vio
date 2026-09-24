package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("repo root: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestContractBaseSelectsMergeParent is the regression test for the
// merge-commit bypass: `git merge-base BASE_REF HEAD` resolves to HEAD itself
// once the base ref contains the merge (the merged-PR shape), so the checker
// compared the document with itself and reported nothing — which is how
// af22a65e's contract breaks slipped through. The base resolver must diff a
// merge head against the explicit base ref, falling back to the merge's first
// parent when BASE_REF is HEAD, while leaving a plain commit on the merge-base
// rule.
func TestContractBaseSelectsMergeParent(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "apiv2-contract-base.sh")
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	resolve := func(baseRef, revision string) string {
		t.Helper()
		cmd := exec.Command("bash", script, baseRef, revision)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("resolve %s %s: %v\n%s", baseRef, revision, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "feature", base)
	git("commit", "-q", "--allow-empty", "-m", "feature work")
	feature := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-ff", "feature", "-m", "merge feature")
	merge := git("rev-parse", "HEAD")

	// Precondition: the merged PR has landed, so merge-base collapses to the
	// merge itself — exactly the af22a65e shape.
	if got := git("merge-base", "main", merge); got != merge {
		t.Fatalf("precondition: merge-base main merge = %s, want the merge itself %s", got, merge)
	}

	// Collapse: BASE_REF is the merge itself, so the resolver must use the
	// merge's first parent (the pre-merge base) instead of short-circuiting.
	if got := resolve("main", merge); got != base {
		t.Fatalf("collapse base = %s, want first parent %s (merge-base would return %s)", got, base, merge)
	}

	// A merge head with a distinct base ref resolves to that ref, not to the
	// merge-base inside the merged work.
	git("branch", "-q", "pr-base", base)
	if got := resolve("pr-base", merge); got != base {
		t.Fatalf("merge base against pr-base = %s, want %s", got, base)
	}

	// A plain commit keeps the merge-base rule: on a feature branch that is
	// already contained in the base, merge-base is the tip.
	if got := resolve("main", feature); got != feature {
		t.Fatalf("plain head base = %s, want merge-base %s", got, feature)
	}
}
