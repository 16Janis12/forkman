package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// dryRun, when true, causes mutating git commands to be printed instead of run.
var dryRun bool

// mutatingArgs lists the first git subcommand words that change repo state.
// Used to gate execution under --dry-run.
var mutatingArgs = map[string]bool{
	"fetch": true, "merge": true, "rebase": true, "stash": true,
	"branch": true, "remote": true, "config": true, "checkout": true,
	"apply": true, "commit": true,
}

// git runs `git -C repoDir args...`, streaming nothing, returning combined output.
// Under dryRun, mutating commands are printed and skipped (returning empty output).
func git(repoDir string, args ...string) (string, error) {
	if dryRun && len(args) > 0 && mutatingArgs[args[0]] {
		// config reads are non-mutating; only --add/--unset etc. mutate, but
		// gitConfig() handles reads separately, so any config call here is a write.
		fmt.Printf("    [dry-run] git -C %s %s\n", repoDir, strings.Join(args, " "))
		return "", nil
	}
	return gitRaw(repoDir, args...)
}

// gitRaw always executes, ignoring dryRun. Use for read-only queries.
func gitRaw(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// gitOut executes git and returns stdout only (stderr is folded into the error).
// Use where the output is data, e.g. `git diff`.
func gitOut(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// gitIn executes git with stdin fed from in, returning combined output.
// Honors dryRun for mutating subcommands.
func gitIn(repoDir, in string, args ...string) (string, error) {
	if dryRun && len(args) > 0 && mutatingArgs[args[0]] {
		fmt.Printf("    [dry-run] git -C %s %s\n", repoDir, strings.Join(args, " "))
		return "", nil
	}
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// repoRoot returns the top-level directory of the git repo containing dir.
func repoRoot(dir string) (string, error) {
	out, err := gitRaw(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	return out, nil
}

// isGitRepo reports whether dir is within a git work tree.
func isGitRepo(dir string) bool {
	_, err := gitRaw(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// currentBranch returns the checked-out branch name, or error if detached.
func currentBranch(repo string) (string, error) {
	out, err := gitRaw(repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || out == "" {
		return "", errors.New("HEAD is detached (not on a branch)")
	}
	return out, nil
}

// isDirty reports whether the working tree or index has uncommitted changes.
func isDirty(repo string) (bool, error) {
	out, err := gitRaw(repo, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// remoteExists reports whether a remote named name is configured.
func remoteExists(repo, name string) bool {
	out, err := gitRaw(repo, "remote")
	if err != nil {
		return false
	}
	for _, r := range strings.Fields(out) {
		if r == name {
			return true
		}
	}
	return false
}

// remoteURL returns the fetch URL of the named remote.
func remoteURL(repo, name string) (string, error) {
	return gitRaw(repo, "remote", "get-url", name)
}

// gitConfigGet reads a git config value; returns ("", nil) if unset.
func gitConfigGet(repo, key string) (string, error) {
	out, err := gitRaw(repo, "config", "--local", "--get", key)
	if err != nil {
		// exit code 1 = key missing; distinguish from real errors is noisy, treat as unset.
		return "", nil
	}
	return out, nil
}

// gitConfigSet writes a local git config value (honors dryRun).
func gitConfigSet(repo, key, val string) error {
	_, err := git(repo, "config", "--local", key, val)
	return err
}

// defaultBranch returns the upstream remote's default branch (via its HEAD ref),
// e.g. "main". Falls back to error if it cannot be determined.
func defaultBranch(repo, remote string) (string, error) {
	out, err := gitRaw(repo, "symbolic-ref", "--quiet", "--short",
		"refs/remotes/"+remote+"/HEAD")
	if err == nil && out != "" {
		return strings.TrimPrefix(out, remote+"/"), nil
	}
	// Fallback: ask the remote directly.
	out, err = gitRaw(repo, "ls-remote", "--symref", remote, "HEAD")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "ref:") {
				f := strings.Fields(line) // ref: refs/heads/main HEAD
				if len(f) >= 2 {
					return strings.TrimPrefix(f[1], "refs/heads/"), nil
				}
			}
		}
	}
	return "", fmt.Errorf("could not determine default branch of %q", remote)
}

// aheadBehind returns how many commits ref b is ahead and behind ref a
// (i.e. commits in b..a and a..b). Used to summarize divergence.
func aheadBehind(repo, base, tip string) (ahead, behind int, err error) {
	out, err := gitRaw(repo, "rev-list", "--left-right", "--count", base+"..."+tip)
	if err != nil {
		return 0, 0, err
	}
	return parseAheadBehind(out)
}

// parseAheadBehind parses `rev-list --left-right --count base...tip` output.
func parseAheadBehind(out string) (ahead, behind int, err error) {
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output: %q", out)
	}
	behind, _ = strconv.Atoi(f[0]) // left = in base not tip
	ahead, _ = strconv.Atoi(f[1])  // right = in tip not base
	return ahead, behind, nil
}

// syncBranches lists local branches matching the fork-sync/ prefix.
func syncBranches(repo string) []string {
	out, err := gitRaw(repo, "branch", "--list", syncBranchPrefix+"*", "--format=%(refname:short)")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// absClean returns the absolute, symlink-free path for dir.
func absClean(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// mustExist reports whether path exists on disk.
func mustExist(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// loadIgnorePatterns loads ignore patterns for a repo or directory.
// It checks both the .forkignore file in workDir and the git config key in repoDir.
func loadIgnorePatterns(repoDir, workDir, configKey string) []string {
	var patterns []string
	seen := make(map[string]bool)

	addPattern := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			return
		}
		p = filepath.ToSlash(p)
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimSuffix(p, "/")
		if p != "" && !seen[p] {
			seen[p] = true
			patterns = append(patterns, p)
		}
	}

	// 1. Read .forkignore from workDir if present
	forkignorePath := filepath.Join(workDir, ".forkignore")
	if data, err := os.ReadFile(forkignorePath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			addPattern(line)
		}
	}

	// 2. Read git config key if provided
	if configKey != "" {
		if val, err := gitRaw(repoDir, "config", "--local", "--get-all", configKey); err == nil && val != "" {
			for _, line := range strings.Split(val, "\n") {
				for _, part := range strings.Split(line, ",") {
					addPattern(part)
				}
			}
		}
	}

	return patterns
}

// pathMatchesIgnore reports whether a slash-separated relative path matches any ignore pattern.
func pathMatchesIgnore(relPath string, patterns []string) bool {
	relPath = filepath.ToSlash(filepath.Clean(relPath))
	relPath = strings.TrimPrefix(relPath, "./")
	relPath = strings.TrimPrefix(relPath, "/")

	for _, pattern := range patterns {
		pattern = filepath.ToSlash(filepath.Clean(pattern))
		pattern = strings.TrimPrefix(pattern, "./")
		pattern = strings.TrimSuffix(pattern, "/")
		pattern = strings.TrimPrefix(pattern, "/")

		if relPath == pattern {
			return true
		}
		if strings.HasPrefix(relPath, pattern+"/") {
			return true
		}
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}
		if !strings.Contains(pattern, "/") {
			if matched, _ := filepath.Match(pattern, filepath.Base(relPath)); matched {
				return true
			}
		}
	}
	return false
}

// buildPathspecExcludes returns pathspec arguments for git commands to exclude patterns.
func buildPathspecExcludes(patterns []string) []string {
	var args []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p != "" {
			args = append(args, ":(exclude)"+p)
		}
	}
	return args
}

// unmergedFiles returns the list of files currently in merge conflict (unmerged status).
func unmergedFiles(repoDir string) []string {
	out, err := gitRaw(repoDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	var res []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			res = append(res, line)
		}
	}
	return res
}

// restoreIgnoredInRepo restores all ignored files/directories to their state in baseCommit,
// removes any new files created by upstream in ignored paths, and stages the result.
func restoreIgnoredInRepo(repoDir, baseCommit string, patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}

	diffOut, _ := gitRaw(repoDir, "diff", "--name-only", baseCommit)
	cachedDiffOut, _ := gitRaw(repoDir, "diff", "--cached", "--name-only", baseCommit)

	fileSet := make(map[string]bool)
	for _, out := range []string{diffOut, cachedDiffOut} {
		for _, f := range strings.Split(out, "\n") {
			f = strings.TrimSpace(f)
			if f != "" {
				fileSet[f] = true
			}
		}
	}

	for f := range fileSet {
		if !pathMatchesIgnore(f, patterns) {
			continue
		}

		if _, err := gitRaw(repoDir, "cat-file", "-e", baseCommit+":"+f); err == nil {
			if _, err := gitRaw(repoDir, "checkout", baseCommit, "--", f); err != nil {
				return fmt.Errorf("restoring %s from %s: %w", f, short(baseCommit), err)
			}
			_, _ = gitRaw(repoDir, "add", "--", f)
		} else {
			_, _ = gitRaw(repoDir, "rm", "-f", "--cached", "--ignore-unmatch", "--", f)
			fullPath := filepath.Join(repoDir, filepath.FromSlash(f))
			_ = os.Remove(fullPath)
		}
	}

	for _, p := range patterns {
		_, _ = gitRaw(repoDir, "clean", "-fdq", "--", p)
	}

	return nil
}
