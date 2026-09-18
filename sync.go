package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const syncBranchPrefix = "fork-sync/"

// forkConfig is the per-repo upstream configuration stored in git config.
type forkConfig struct {
	Remote string // e.g. "upstream"
	URL    string // upstream fetch URL
	Branch string // upstream branch to track, e.g. "main"
	Tag    string // upstream tag to track, e.g. "v1.0.0"
}

// loadForkConfig reads fork.* keys from the repo's git config.
// Missing remote/branch are filled from sensible defaults where possible.
func loadForkConfig(repo string) (forkConfig, error) {
	var c forkConfig
	c.Remote, _ = gitConfigGet(repo, "fork.upstreamRemote")
	c.URL, _ = gitConfigGet(repo, "fork.upstream")
	c.Branch, _ = gitConfigGet(repo, "fork.upstreamBranch")
	c.Tag, _ = gitConfigGet(repo, "fork.upstreamTag")

	if c.Remote == "" {
		c.Remote = "upstream"
	}
	if c.URL == "" && !remoteExists(repo, c.Remote) {
		return c, fmt.Errorf("not initialized: run `forkman init` here first")
	}
	if c.Tag == "" && c.Branch == "" {
		if b, err := defaultBranch(repo, c.Remote); err == nil {
			c.Branch = b
		} else if b, err := currentBranch(repo); err == nil {
			c.Branch = b // last resort: track same-named branch
		} else {
			return c, fmt.Errorf("cannot determine upstream branch: %w", err)
		}
	}
	return c, nil
}

// cleanTag normalizes a tag string by stripping refs/tags/ and tags/ prefixes and trimming spaces.
func cleanTag(tag string) string {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "refs/tags/")
	tag = strings.TrimPrefix(tag, "tags/")
	return tag
}

// hasRepoFork reports whether the repo itself (not just subdirectories of it)
// is registered as a fork.
func hasRepoFork(repo string) bool {
	if v, _ := gitConfigGet(repo, "fork.upstream"); v != "" {
		return true
	}
	v, _ := gitConfigGet(repo, "fork.upstreamRemote")
	return v != ""
}

// syncResult captures the outcome of syncing one repo for the summary table.
type syncResult struct {
	Repo   string
	Status string // "up-to-date", "merged N", "CONFLICT", "error"
	Detail string
}

// syncOptions controls the sync pipeline.
type syncOptions struct {
	Rebase bool
	Tag    string // if set, sync to this specific tag instead of the tracked branch/tag
}

// syncOne runs the full sync pipeline for a single repo.
// The result is named so the deferred stash-pop below can append its warning
// to a result that has already been assigned by a `return res` statement.
func syncOne(repo string, opt syncOptions) (res syncResult) {
	res = syncResult{Repo: repo}

	cfg, err := loadForkConfig(repo)
	if err != nil {
		res.Status, res.Detail = "error", err.Error()
		return res
	}

	branch, err := currentBranch(repo)
	if err != nil {
		res.Status, res.Detail = "error", err.Error()
		return res
	}

	// 1. Stash dirty tree so we never clobber uncommitted work.
	stashed := false
	if dirty, _ := isDirty(repo); dirty {
		if _, err := git(repo, "stash", "push", "-u", "-m", "forkman-autostash"); err != nil {
			res.Status, res.Detail = "error", "stash failed: "+err.Error()
			return res
		}
		stashed = true
	}
	// Ensure we always try to restore the stash on the way out.
	defer func() {
		if stashed {
			if _, err := git(repo, "stash", "pop"); err != nil {
				res.Detail += " (WARNING: `git stash pop` failed — restore manually: " + err.Error() + ")"
			}
		}
	}()

	var upstreamRef string
	targetTag := opt.Tag
	if targetTag == "" {
		targetTag = cfg.Tag
	}

	if targetTag != "" {
		// 2. Fetch upstream tag.
		if _, err := git(repo, "fetch", "--force", cfg.Remote, "tag", targetTag); err != nil {
			res.Status, res.Detail = "error", "fetch failed: "+err.Error()
			return res
		}
		upstreamRef = "tags/" + targetTag
	} else {
		// 2. Fetch upstream branch.
		if _, err := git(repo, "fetch", cfg.Remote, cfg.Branch); err != nil {
			res.Status, res.Detail = "error", "fetch failed: "+err.Error()
			return res
		}
		upstreamRef = cfg.Remote + "/" + cfg.Branch
	}

	patterns := loadIgnorePatterns(repo, repo, "fork.ignore")

	if dryRun {
		res.Status = "dry-run"
		ignoreMsg := ""
		if len(patterns) > 0 {
			ignoreMsg = fmt.Sprintf(" (ignoring %d pattern(s))", len(patterns))
		}
		res.Detail = fmt.Sprintf("would %s %s into %s%s", strategyVerb(opt), upstreamRef, branch, ignoreMsg)
		return res
	}

	// 3. Already up to date?
	ahead, behind, err := aheadBehind(repo, upstreamRef, "HEAD")
	if err == nil && behind == 0 {
		res.Status = "up-to-date"
		res.Detail = fmt.Sprintf("ahead %d of %s", ahead, upstreamRef)
		return res
	}

	// 4. Attempt the integration.
	var mergeErr error
	if opt.Rebase {
		_, mergeErr = git(repo, "rebase", upstreamRef)
	} else if len(patterns) == 0 {
		_, mergeErr = git(repo, "merge", "--no-edit", upstreamRef)
	} else {
		preMergeHead, err := gitRaw(repo, "rev-parse", "HEAD")
		if err != nil {
			res.Status, res.Detail = "error", "rev-parse HEAD failed: "+err.Error()
			return res
		}

		_, mergeErr = gitRaw(repo, "merge", "--no-commit", "--no-ff", upstreamRef)
		if mergeErr != nil {
			conflicts := unmergedFiles(repo)
			allConflictsIgnored := len(conflicts) > 0
			for _, c := range conflicts {
				if !pathMatchesIgnore(c, patterns) {
					allConflictsIgnored = false
					break
				}
			}
			if allConflictsIgnored {
				for _, c := range conflicts {
					if _, err := gitRaw(repo, "cat-file", "-e", preMergeHead+":"+c); err == nil {
						_, _ = gitRaw(repo, "checkout", preMergeHead, "--", c)
						_, _ = gitRaw(repo, "add", "--", c)
					} else {
						_, _ = gitRaw(repo, "rm", "-f", "--cached", "--ignore-unmatch", "--", c)
						_ = os.Remove(filepath.Join(repo, filepath.FromSlash(c)))
					}
				}
				mergeErr = nil
			}
		}

		if mergeErr == nil {
			if err := restoreIgnoredInRepo(repo, preMergeHead, patterns); err != nil {
				_, _ = gitRaw(repo, "merge", "--abort")
				res.Status, res.Detail = "error", "restoring ignored paths failed: "+err.Error()
				return res
			}
			msg := fmt.Sprintf("forkman: merge %s into %s", upstreamRef, branch)
			_, mergeErr = gitRaw(repo, "commit", "--allow-empty", "-m", msg)
		}
	}

	if mergeErr == nil {
		res.Status = fmt.Sprintf("merged %d", behind)
		res.Detail = fmt.Sprintf("from %s", upstreamRef)
		return res
	}

	// 5. Conflict: abort, keep working branch clean, park upstream in a branch.
	if opt.Rebase {
		_, _ = git(repo, "rebase", "--abort")
	} else {
		_, _ = git(repo, "merge", "--abort")
	}

	parked := syncBranchPrefix + branch + "-" + time.Now().Format("20060102")
	if _, err := git(repo, "branch", "-f", parked, upstreamRef); err != nil {
		res.Status = "CONFLICT"
		res.Detail = "conflict, and failed to create park branch: " + err.Error()
		return res
	}
	res.Status = "CONFLICT"
	res.Detail = fmt.Sprintf("parked upstream in %q — resolve with: git merge %s", parked, parked)
	return res
}

func strategyVerb(opt syncOptions) string {
	if opt.Rebase {
		return "rebase onto"
	}
	return "merge"
}

// runSync syncs the given repos and prints a summary. Returns a non-zero-worthy
// bool (hadFailure) if any repo conflicted or errored.
func runSync(repos []string, opt syncOptions) bool {
	results := make([]syncResult, 0, len(repos))
	for _, repo := range repos {
		fmt.Printf("==> %s\n", repo)

		subs, _ := loadSubForks(repo)
		whole := hasRepoFork(repo)
		if !whole && len(subs) == 0 {
			r := syncResult{Repo: repo, Status: "error",
				Detail: "not initialized: run `forkman init` (or `forkman init --dir SUBDIR`) here first"}
			fmt.Printf("    %s: %s\n", r.Status, r.Detail)
			results = append(results, r)
			continue
		}

		if whole {
			r := syncOne(repo, opt)
			fmt.Printf("    %s: %s\n", r.Status, r.Detail)
			results = append(results, r)
		}
		for _, sf := range subs {
			fmt.Printf("  -> %s/\n", sf.Prefix)
			r := syncSub(repo, sf, opt)
			fmt.Printf("    %s: %s\n", r.Status, r.Detail)
			results = append(results, r)
		}
	}

	fmt.Println("\nSummary:")
	hadFailure := false
	for _, r := range results {
		mark := "ok"
		switch {
		case strings.HasPrefix(r.Status, "CONFLICT"), r.Status == "error":
			mark = "!!"
			hadFailure = true
		}
		fmt.Printf("  [%s] %-10s %s\n", mark, r.Status, r.Repo)
	}
	return hadFailure
}
