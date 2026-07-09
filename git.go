package main

import (
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
