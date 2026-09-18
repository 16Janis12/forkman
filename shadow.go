package main

// Shadow mode: a nested clone that keeps its full history, but whose git dir is
// parked at <parent>/.git/forkman/<slug>.git instead of <prefix>/.git.
//
// Git records a directory containing a .git as a gitlink (mode 160000), so a
// nested clone cannot have its files tracked by the parent repo. Moving the git
// dir out of the worktree removes the gitlink without losing the history: the
// parent sees plain files and pushes them to its own remote, while forkman
// drives the shadow repo (via --git-dir/--work-tree) to merge upstream with a
// real merge base.
//
// Local edits land in the worktree through the parent repo, so each sync first
// commits that drift into the shadow repo — that is what gives the upstream
// merge something to merge against.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const localBranchMsg = "forkman: local changes"

// shadowGitDir returns the absolute path of the parked git dir.
func shadowGitDir(repo string, sf subFork) string {
	if sf.GitDir == "" {
		return filepath.Join(repo, ".git", "forkman", shadowSlug(sf.Prefix)+".git")
	}
	if filepath.IsAbs(sf.GitDir) {
		return sf.GitDir
	}
	return filepath.Join(repo, sf.GitDir)
}

func shadowSlug(prefix string) string {
	return strings.Trim(strings.NewReplacer("/", "-", " ", "-").Replace(prefix), "-")
}

// shadowArgs prefixes git args so they act on the shadow repo over the
// subdirectory worktree.
func shadowArgs(repo string, sf subFork, args []string) []string {
	return append([]string{
		"--git-dir=" + shadowGitDir(repo, sf),
		"--work-tree=" + filepath.Join(repo, sf.Prefix),
	}, args...)
}

// gitShadow runs a git command against the shadow repo (honors dryRun).
func gitShadow(repo string, sf subFork, args ...string) (string, error) {
	return git(repo, shadowArgs(repo, sf, args)...)
}

// gitShadowRaw runs a git command against the shadow repo, ignoring dryRun.
func gitShadowRaw(repo string, sf subFork, args ...string) (string, error) {
	return gitRaw(repo, shadowArgs(repo, sf, args)...)
}

// initShadow converts a nested clone at prefix into a shadow-mode sub-fork.
func initShadow(repo, prefix, upstream, remote, branch, tag, ignore string) error {
	full := filepath.Join(repo, prefix)
	nested := filepath.Join(full, ".git")

	// A .git *file* means a worktree/submodule pointing elsewhere; relocating
	// that safely is out of scope.
	if fi, err := os.Stat(nested); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s/.git is a file (linked worktree or submodule), not a git dir; "+
			"forkman cannot relocate it", prefix)
	}

	gitDirRel := filepath.Join(".git", "forkman", shadowSlug(prefix)+".git")
	gitDirAbs := filepath.Join(repo, gitDirRel)
	if mustExist(gitDirAbs) {
		return fmt.Errorf("%s already exists — %s seems to be registered already; "+
			"`forkman remove --dir %s` first", gitDirAbs, prefix, prefix)
	}

	if branch != "" && tag != "" {
		return fmt.Errorf("cannot specify both --branch and --tag")
	}

	if remote == "" || remote == "upstream" {
		remote = "origin" // a nested clone's own remote is the upstream
	}

	if dryRun {
		fmt.Printf("[dry-run] would move %s -> %s and register %s (shadow mode)\n",
			nested, gitDirAbs, prefix)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(gitDirAbs), 0o755); err != nil {
		return err
	}
	if err := os.Rename(nested, gitDirAbs); err != nil {
		return fmt.Errorf("moving %s aside: %w", nested, err)
	}
	sf := subFork{Prefix: prefix, Mode: modeShadow, Remote: remote, GitDir: gitDirRel}

	// The git dir is no longer inside its worktree, so point it back explicitly.
	if _, err := gitShadowRaw(repo, sf, "config", "core.bare", "false"); err != nil {
		return err
	}
	if _, err := gitShadowRaw(repo, sf, "config", "core.worktree", full); err != nil {
		return err
	}

	// Attach or adopt the upstream remote inside the shadow repo.
	if _, err := gitShadowRaw(repo, sf, "remote", "get-url", remote); err != nil {
		if upstream == "" {
			return fmt.Errorf("remote %q does not exist in %s; provide --upstream URL", remote, prefix)
		}
		if _, err := gitShadowRaw(repo, sf, "remote", "add", remote, upstream); err != nil {
			return err
		}
	} else if upstream != "" {
		if _, err := gitShadowRaw(repo, sf, "remote", "set-url", remote, upstream); err != nil {
			return err
		}
	}
	if upstream == "" {
		if u, err := gitShadowRaw(repo, sf, "remote", "get-url", remote); err == nil {
			upstream = u
		}
	}

	if tag != "" {
		if _, err := gitShadowRaw(repo, sf, "fetch", "--force", remote, "tag", tag); err != nil {
			return fmt.Errorf("fetch tag %s from %s: %w", tag, remote, err)
		}
		sf.Tag = tag
	} else {
		if branch == "" {
			if b, err := gitShadowRaw(repo, sf, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && b != "" {
				branch = b
			} else {
				return fmt.Errorf("%s is not on a branch; pass --branch NAME", prefix)
			}
		}
		sf.Branch = branch
	}
	sf.URL = upstream

	for key, val := range map[string]string{
		"prefix":   prefix,
		"mode":     modeShadow,
		"gitdir":   gitDirRel,
		"remote":   remote,
		"upstream": upstream,
		"branch":   branch,
		"tag":      tag,
		"ignore":   ignore,
	} {
		if val == "" {
			continue
		}
		if err := gitConfigSet(repo, subKey(prefix, key), val); err != nil {
			return err
		}
	}

	staged, err := adoptIntoParent(repo, prefix)
	if err != nil {
		return err
	}
	if err := registerFork(repo); err != nil {
		return err
	}

	fmt.Printf("registered %s (shadow mode, subdirectory of %s)\n", prefix, repo)
	fmt.Printf("  history:  %s (moved out of the worktree)\n", gitDirRel)
	if tag != "" {
		fmt.Printf("  upstream: %s (tag %s, %s)\n", remote, tag, upstream)
	} else {
		fmt.Printf("  upstream: %s/%s (%s)\n", remote, branch, upstream)
	}
	if staged {
		fmt.Printf("  %s is now tracked by the parent as plain files — commit and `git push` as usual.\n", prefix)
	}
	fmt.Printf("run `forkman sync` to merge upstream changes into %s.\n", prefix)
	return nil
}

// adoptIntoParent makes the parent repo track prefix as plain files, replacing
// a gitlink entry if one was recorded while the nested .git was in place.
// Reports whether anything was staged.
func adoptIntoParent(repo, prefix string) (bool, error) {
	// A gitlink shows up as a commit-type entry in the parent's index.
	if out, err := gitRaw(repo, "ls-files", "--stage", "--", prefix); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "160000 ") {
				if _, err := gitRaw(repo, "rm", "--cached", "-q", "--", prefix); err != nil {
					return false, fmt.Errorf("removing gitlink entry for %s: %w", prefix, err)
				}
				break
			}
		}
	}
	if _, err := gitRaw(repo, "add", "--", prefix); err != nil {
		return false, fmt.Errorf("staging %s in the parent repo: %w", prefix, err)
	}
	staged, err := gitRaw(repo, "diff", "--cached", "--name-only", "--", prefix)
	if err != nil || strings.TrimSpace(staged) == "" {
		return false, nil
	}
	_, err = gitRaw(repo, "commit", "-m", "forkman: track "+prefix+" as plain files")
	return true, err
}

// restoreIgnoredInShadow restores ignored files in the shadow worktree to their baseCommit state.
func restoreIgnoredInShadow(repo string, sf subFork, baseCommit string, patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}

	diffOut, _ := gitShadowRaw(repo, sf, "diff", "--name-only", baseCommit)
	cachedDiffOut, _ := gitShadowRaw(repo, sf, "diff", "--cached", "--name-only", baseCommit)

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

		if _, err := gitShadowRaw(repo, sf, "cat-file", "-e", baseCommit+":"+f); err == nil {
			if _, err := gitShadowRaw(repo, sf, "checkout", baseCommit, "--", f); err != nil {
				return fmt.Errorf("restoring %s in shadow repo: %w", f, err)
			}
			_, _ = gitShadowRaw(repo, sf, "add", "--", f)
		} else {
			_, _ = gitShadowRaw(repo, sf, "rm", "-f", "--cached", "--ignore-unmatch", "--", f)
			fullPath := filepath.Join(repo, sf.Prefix, filepath.FromSlash(f))
			_ = os.Remove(fullPath)
		}
	}

	for _, p := range patterns {
		_, _ = gitShadowRaw(repo, sf, "clean", "-fdq", "--", p)
	}

	return nil
}

// syncShadow merges upstream into a shadow-mode subdirectory, then commits the
// resulting file changes in the parent repo.
func syncShadow(repo string, sf subFork, opt syncOptions) syncResult {
	res := syncResult{Repo: repo + " [" + sf.Prefix + "]"}

	if !mustExist(shadowGitDir(repo, sf)) {
		res.Status, res.Detail = "error", "shadow git dir missing: "+shadowGitDir(repo, sf)
		return res
	}

	branch, err := gitShadowRaw(repo, sf, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch == "" {
		res.Status, res.Detail = "error", "shadow repo HEAD is detached (not on a branch)"
		return res
	}

	var upstreamRef string
	targetTag := opt.Tag
	if targetTag == "" {
		targetTag = sf.Tag
	}

	if targetTag != "" {
		if _, err := gitShadow(repo, sf, "fetch", "--force", sf.Remote, "tag", targetTag); err != nil {
			res.Status, res.Detail = "error", "fetch failed: "+err.Error()
			return res
		}
		upstreamRef = "tags/" + targetTag
	} else {
		if sf.Branch == "" {
			sf.Branch = branch
		}
		upstreamRef = sf.Remote + "/" + sf.Branch

		if _, err := gitShadow(repo, sf, "fetch", sf.Remote, sf.Branch); err != nil {
			res.Status, res.Detail = "error", "fetch failed: "+err.Error()
			return res
		}
	}

	patterns := loadIgnorePatterns(repo, filepath.Join(repo, sf.Prefix), subKey(sf.Prefix, "ignore"))

	if dryRun {
		res.Status = "dry-run"
		ignoreMsg := ""
		if len(patterns) > 0 {
			ignoreMsg = fmt.Sprintf(" (ignoring %d pattern(s))", len(patterns))
		}
		res.Detail = fmt.Sprintf("would %s %s into %s/ (shadow)%s", strategyVerb(opt), upstreamRef, sf.Prefix, ignoreMsg)
		return res
	}

	// Edits reach these files through the parent repo, so the shadow repo's
	// index is stale. Commit that drift first: it is the local side of the merge.
	if out, err := gitShadowRaw(repo, sf, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
		if _, err := gitShadowRaw(repo, sf, "add", "-A"); err != nil {
			res.Status, res.Detail = "error", "staging local changes failed: "+err.Error()
			return res
		}
		if _, err := gitShadowRaw(repo, sf, "commit", "-m", localBranchMsg); err != nil {
			res.Status, res.Detail = "error", "committing local changes failed: "+err.Error()
			return res
		}
	}

	ahead, behind, err := aheadBehindShadow(repo, sf, upstreamRef)
	if err == nil && behind == 0 {
		res.Status = "up-to-date"
		res.Detail = fmt.Sprintf("%s/ ahead %d of %s", sf.Prefix, ahead, upstreamRef)
		return res
	}

	var mergeErr error
	if opt.Rebase {
		_, mergeErr = gitShadowRaw(repo, sf, "rebase", upstreamRef)
	} else if len(patterns) == 0 {
		_, mergeErr = gitShadowRaw(repo, sf, "merge", "--no-edit", upstreamRef)
	} else {
		preMergeHead, err := gitShadowRaw(repo, sf, "rev-parse", "HEAD")
		if err != nil {
			res.Status, res.Detail = "error", "rev-parse shadow HEAD failed: "+err.Error()
			return res
		}

		_, mergeErr = gitShadowRaw(repo, sf, "merge", "--no-commit", "--no-ff", upstreamRef)
		if mergeErr != nil {
			out, _ := gitShadowRaw(repo, sf, "diff", "--name-only", "--diff-filter=U")
			conflicts := strings.Split(strings.TrimSpace(out), "\n")
			allConflictsIgnored := len(conflicts) > 0 && conflicts[0] != ""
			for _, c := range conflicts {
				if c != "" && !pathMatchesIgnore(c, patterns) {
					allConflictsIgnored = false
					break
				}
			}
			if allConflictsIgnored {
				for _, c := range conflicts {
					if c == "" {
						continue
					}
					if _, err := gitShadowRaw(repo, sf, "cat-file", "-e", preMergeHead+":"+c); err == nil {
						_, _ = gitShadowRaw(repo, sf, "checkout", preMergeHead, "--", c)
						_, _ = gitShadowRaw(repo, sf, "add", "--", c)
					} else {
						_, _ = gitShadowRaw(repo, sf, "rm", "-f", "--cached", "--ignore-unmatch", "--", c)
						_ = os.Remove(filepath.Join(repo, sf.Prefix, filepath.FromSlash(c)))
					}
				}
				mergeErr = nil
			}
		}

		if mergeErr == nil {
			if err := restoreIgnoredInShadow(repo, sf, preMergeHead, patterns); err != nil {
				_, _ = gitShadowRaw(repo, sf, "merge", "--abort")
				res.Status, res.Detail = "error", "restoring ignored paths in shadow failed: "+err.Error()
				return res
			}
			msg := fmt.Sprintf("forkman: sync %s from %s", sf.Prefix, upstreamRef)
			_, mergeErr = gitShadowRaw(repo, sf, "commit", "--allow-empty", "-m", msg)
		}
	}

	if mergeErr != nil {
		// Abort restores the worktree, so the parent's files are untouched too.
		if opt.Rebase {
			_, _ = gitShadowRaw(repo, sf, "rebase", "--abort")
		} else {
			_, _ = gitShadowRaw(repo, sf, "merge", "--abort")
		}
		parked := syncBranchPrefix + branch + "-" + time.Now().Format("20060102")
		if _, err := gitShadowRaw(repo, sf, "branch", "-f", parked, upstreamRef); err != nil {
			res.Status = "CONFLICT"
			res.Detail = "conflict, and failed to create park branch: " + err.Error()
			return res
		}
		res.Status = "CONFLICT"
		res.Detail = fmt.Sprintf("parked upstream in %q — resolve with: %s merge %s",
			parked, shadowCmd(repo, sf), parked)
		return res
	}

	// The merge rewrote files the parent tracks; record that as a parent commit.
	if _, err := gitRaw(repo, "add", "-A", "--", sf.Prefix); err != nil {
		res.Status, res.Detail = "error", "staging merged files in the parent failed: "+err.Error()
		return res
	}
	staged, _ := gitRaw(repo, "diff", "--cached", "--name-only", "--", sf.Prefix)
	if strings.TrimSpace(staged) == "" {
		res.Status = fmt.Sprintf("merged %d", behind)
		res.Detail = fmt.Sprintf("from %s (no file changes in %s/)", upstreamRef, sf.Prefix)
		return res
	}
	msg := fmt.Sprintf("forkman: sync %s from %s", sf.Prefix, upstreamRef)
	if _, err := gitRaw(repo, "commit", "-m", msg, "--", sf.Prefix); err != nil {
		res.Status, res.Detail = "error", "parent commit failed: "+err.Error()
		return res
	}

	res.Status = fmt.Sprintf("merged %d", behind)
	res.Detail = fmt.Sprintf("from %s into %s/ — `git push` to publish", upstreamRef, sf.Prefix)
	return res
}

// aheadBehindShadow reports divergence of the shadow repo's HEAD from upstream.
func aheadBehindShadow(repo string, sf subFork, upstreamRef string) (ahead, behind int, err error) {
	out, err := gitShadowRaw(repo, sf, "rev-list", "--left-right", "--count", upstreamRef+"...HEAD")
	if err != nil {
		return 0, 0, err
	}
	return parseAheadBehind(out)
}

// shadowCmd renders the git invocation a user needs to drive the shadow repo.
func shadowCmd(repo string, sf subFork) string {
	return fmt.Sprintf("git --git-dir=%s --work-tree=%s",
		shadowGitDir(repo, sf), filepath.Join(repo, sf.Prefix))
}

// removeShadow moves the git dir back into the subdirectory, restoring an
// ordinary nested clone. The parent keeps tracking the files it has committed.
func restoreShadow(repo string, sf subFork) error {
	gitDirAbs := shadowGitDir(repo, sf)
	if !mustExist(gitDirAbs) {
		return nil
	}
	dest := filepath.Join(repo, sf.Prefix, ".git")
	if mustExist(dest) {
		return fmt.Errorf("%s already exists; leaving %s in place", dest, gitDirAbs)
	}
	if err := os.Rename(gitDirAbs, dest); err != nil {
		return err
	}
	if _, err := gitRaw(repo, "--git-dir="+dest, "--work-tree="+filepath.Join(repo, sf.Prefix),
		"config", "--unset", "core.worktree"); err != nil {
		return err
	}
	return nil
}
