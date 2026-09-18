package main

// Vendored subdirectory forks: shadow mode.
//
// When a vendored copy still has its own .git directory, Git treats it as a
// submodule gitlink (mode 160000) and refuses to track the plain files inside
// it. That defeats the point of vendoring.
//
// Instead of deleting the .git (which throws away the entire commit history and
// forces diff-replay syncing), shadow mode *parks* the .git directory outside
// the worktree:
//
//   vendor/proj/.git  ->  .git/forkman/vendor-proj.git
//
// With `core.worktree` pointed back at vendor/proj, the parked repo keeps its
// full history and upstream remotes, while the parent repo tracks plain files.
//
// Syncing in shadow mode:
//   1. Local edits to vendor/proj/ are committed into the shadow repo's branch.
//   2. `git merge upstream/<branch>` runs inside the shadow repo with a real
//      common ancestor.
//   3. The merged files are staged and committed in the parent repo.
//
// The result: upstream history is preserved, merges have real merge bases, and
// the parent repo holds the one copy that is published.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const localBranchMsg = "forkman: local changes"

// shadowGitDir returns the repo-relative path where a sub-fork's git dir is parked.
func shadowGitDir(repo string, sf subFork) string {
	if sf.GitDir != "" {
		return sf.GitDir
	}
	slug := strings.Trim(strings.NewReplacer("/", "-", " ", "-").Replace(sf.Prefix), "-")
	return filepath.Join(".git", "forkman", slug+".git")
}

// gitShadow runs a git command inside the parked shadow repo.
func gitShadow(repo string, sf subFork, args ...string) (string, error) {
	gitDir := filepath.Join(repo, shadowGitDir(repo, sf))
	workTree := filepath.Join(repo, sf.Prefix)
	return git(repo, append([]string{"--git-dir=" + gitDir, "--work-tree=" + workTree}, args...)...)
}

// gitShadowRaw is the raw-output counterpart to gitShadow.
func gitShadowRaw(repo string, sf subFork, args ...string) (string, error) {
	gitDir := filepath.Join(repo, shadowGitDir(repo, sf))
	workTree := filepath.Join(repo, sf.Prefix)
	return gitRaw(repo, append([]string{"--git-dir=" + gitDir, "--work-tree=" + workTree}, args...)...)
}

// shadowCmd returns the git flags a user needs to run manual commands in the shadow repo.
func shadowCmd(repo string, sf subFork) string {
	return fmt.Sprintf("git --git-dir=%s --work-tree=%s",
		filepath.Join(repo, shadowGitDir(repo, sf)),
		filepath.Join(repo, sf.Prefix))
}

// initShadow converts a nested clone (which still has .git) into a shadow sub-fork.
func initShadow(repo, prefix, upstream, remote, branch, tag, ignore string) error {
	full := filepath.Join(repo, prefix)
	nested := filepath.Join(full, ".git")

	slug := strings.Trim(strings.NewReplacer("/", "-", " ", "-").Replace(prefix), "-")
	gitDirRel := filepath.Join(".git", "forkman", slug+".git")
	gitDirAbs := filepath.Join(repo, gitDirRel)

	if mustExist(gitDirAbs) {
		return fmt.Errorf("shadow git dir %s already exists; sub-fork already initialized?", gitDirAbs)
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

	url := upstream
	if url == "" {
		if u, err := gitShadowRaw(repo, sf, "remote", "get-url", remote); err == nil {
			url = u
		}
	}

	// Default branch: query the shadow repo's own upstream.
	if branch == "" && tag == "" {
		if b, err := defaultBranch(shadowGitDir(repo, sf), remote); err == nil {
			branch = b
		} else if b, err := gitShadowRaw(repo, sf, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			branch = b
		} else {
			branch = "main"
		}
	}

	// Record configuration in the parent repo's git config.
	for _, kv := range [][2]string{
		{"mode", modeShadow},
		{"remote", remote},
		{"upstream", url},
		{"branch", branch},
		{"tag", tag},
		{"gitdir", gitDirRel},
		{"ignore", ignore},
	} {
		if kv[1] != "" {
			if err := gitConfigSet(repo, subKey(prefix, kv[0]), kv[1]); err != nil {
				return err
			}
		}
	}

	// Ensure the parent repo tracks prefix as plain files, not as a gitlink.
	staged, err := adoptIntoParent(repo, prefix)
	if err != nil {
		return err
	}

	fmt.Printf("registered %s/ (shadow mode)\n", prefix)
	if tag != "" {
		fmt.Printf("  upstream: %s (tag %s, %s)\n", remote, tag, url)
	} else {
		fmt.Printf("  upstream: %s/%s (%s)\n", remote, branch, url)
	}
	fmt.Printf("  history:  %s\n", gitDirAbs)
	if ignore != "" {
		fmt.Printf("  ignored:  %s\n", ignore)
	}
	if staged {
		fmt.Printf("  committed %s as plain files in parent repo\n", prefix)
	} else {
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
		for _, line := range strings.Split(out, "\n") {
			f := strings.TrimSpace(line)
			if f != "" {
				fileSet[f] = true
			}
		}
	}

	fullPrefix := filepath.Join(repo, sf.Prefix)
	for file := range fileSet {
		if pathMatchesIgnore(file, patterns) {
			if _, err := gitShadowRaw(repo, sf, "cat-file", "-e", baseCommit+":"+file); err == nil {
				_, _ = gitShadowRaw(repo, sf, "checkout", baseCommit, "--", file)
				_, _ = gitShadowRaw(repo, sf, "add", "--", file)
			} else {
				_, _ = gitShadowRaw(repo, sf, "rm", "-f", "--cached", "--ignore-unmatch", "--", file)
				_ = os.Remove(filepath.Join(fullPrefix, filepath.FromSlash(file)))
			}
		}
	}
	return nil
}

// restoreShadow moves the parked git dir back into prefix/.git.
func restoreShadow(repo string, sf subFork) error {
	gitDirAbs := filepath.Join(repo, shadowGitDir(repo, sf))
	nested := filepath.Join(repo, sf.Prefix, ".git")
	if !mustExist(gitDirAbs) {
		return nil
	}
	// Clear the worktree config before putting it back.
	_, _ = gitRaw(repo, "--git-dir="+gitDirAbs, "config", "--unset", "core.worktree")
	return os.Rename(gitDirAbs, nested)
}

// syncShadow runs the sync pipeline inside a shadow sub-fork.
func syncShadow(repo string, sf subFork, opt syncOptions) (res syncResult) {
	res = syncResult{Repo: repo + " [" + sf.Prefix + "]"}

	if !mustExist(filepath.Join(repo, shadowGitDir(repo, sf))) {
		res.Status, res.Detail = "error", "shadow git dir missing; run `forkman init --dir "+sf.Prefix+"` again"
		return res
	}

	branch, err := gitShadowRaw(repo, sf, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch == "" {
		res.Status, res.Detail = "error", "shadow repo HEAD is detached (not on a branch)"
		return res
	}

	var upstreamRef string
	targetTag := opt.Tag

	prevTag := sf.Tag
	res.PrevTag = prevTag

	effectiveMode := opt.Mode
	if effectiveMode == "" && targetTag == "" {
		effectiveMode, _ = gitConfigGet(repo, subKey(sf.Prefix, "syncMode"))
	}

	if effectiveMode == modeSemanticTags {
		latestTag, err := findLatestSemanticTagShadow(repo, sf, opt.TagPattern)
		if err != nil {
			res.Status, res.Detail = "error", err.Error()
			return res
		}
		targetTag = latestTag
		res.Tag = latestTag
	} else if effectiveMode == modeLatestRev {
		targetTag = ""
	} else if targetTag == "" {
		targetTag = sf.Tag
	}

	if targetTag != "" {
		res.Tag = targetTag
		if prevTag != targetTag {
			res.UpgradeType = semverUpgradeType(prevTag, targetTag)
		}
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
		upgradeMsg := ""
		if res.UpgradeType != "" {
			upgradeMsg = fmt.Sprintf(" [%s upgrade]", res.UpgradeType)
		}
		res.Detail = fmt.Sprintf("would %s %s into %s/ (shadow)%s%s", strategyVerb(opt), upstreamRef, sf.Prefix, ignoreMsg, upgradeMsg)
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
		if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
			_ = gitConfigSet(repo, subKey(sf.Prefix, "tag"), targetTag)
		}
		res.Status = "up-to-date"
		res.UpgradeType = ""
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
			allConflictsIgnored := len(conflicts) > 0
			for _, c := range conflicts {
				if c != "" && !pathMatchesIgnore(c, patterns) {
					allConflictsIgnored = false
					break
				}
			}
			if allConflictsIgnored {
				fullPrefix := filepath.Join(repo, sf.Prefix)
				for _, c := range conflicts {
					if c == "" {
						continue
					}
					if _, err := gitShadowRaw(repo, sf, "cat-file", "-e", preMergeHead+":"+c); err == nil {
						_, _ = gitShadowRaw(repo, sf, "checkout", preMergeHead, "--", c)
						_, _ = gitShadowRaw(repo, sf, "add", "--", c)
					} else {
						_, _ = gitShadowRaw(repo, sf, "rm", "-f", "--cached", "--ignore-unmatch", "--", c)
						_ = os.Remove(filepath.Join(fullPrefix, filepath.FromSlash(c)))
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
		upgradeInfo := ""
		if res.UpgradeType != "" {
			upgradeInfo = fmt.Sprintf(" (%s upgrade)", res.UpgradeType)
		}
		res.Detail = fmt.Sprintf("parked upstream in %q%s — resolve with: %s merge %s",
			parked, upgradeInfo, shadowCmd(repo, sf), parked)
		return res
	}

	// The merge rewrote files the parent tracks; record that as a parent commit.
	if _, err := gitRaw(repo, "add", "-A", "--", sf.Prefix); err != nil {
		res.Status, res.Detail = "error", "staging merged files in the parent failed: "+err.Error()
		return res
	}
	staged, _ := gitRaw(repo, "diff", "--cached", "--name-only", "--", sf.Prefix)
	if strings.TrimSpace(staged) == "" {
		if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
			_ = gitConfigSet(repo, subKey(sf.Prefix, "tag"), targetTag)
		}
		res.Status = fmt.Sprintf("merged %d", behind)
		res.Detail = fmt.Sprintf("from %s (no file changes in %s/)", upstreamRef, sf.Prefix)
		return res
	}
	msg := fmt.Sprintf("forkman: sync %s from %s", sf.Prefix, upstreamRef)
	if _, err := gitRaw(repo, "commit", "-m", msg, "--", sf.Prefix); err != nil {
		res.Status, res.Detail = "error", "parent commit failed: "+err.Error()
		return res
	}

	if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
		_ = gitConfigSet(repo, subKey(sf.Prefix, "tag"), targetTag)
	}

	res.Status = fmt.Sprintf("merged %d", behind)
	if res.UpgradeType != "" {
		if prevTag != "" && prevTag != targetTag {
			res.Detail = fmt.Sprintf("from %s into %s/ (%s upgrade: %s -> %s) — `git push` to publish", upstreamRef, sf.Prefix, res.UpgradeType, prevTag, targetTag)
		} else {
			res.Detail = fmt.Sprintf("from %s into %s/ (%s upgrade) — `git push` to publish", upstreamRef, sf.Prefix, res.UpgradeType)
		}
	} else {
		res.Detail = fmt.Sprintf("from %s into %s/ — `git push` to publish", upstreamRef, sf.Prefix)
	}
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
