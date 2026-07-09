package main

import (
	"fmt"
	"strings"
	"time"
)

const syncBranchPrefix = "fork-sync/"

// forkConfig is the per-repo upstream configuration stored in git config.
type forkConfig struct {
	Remote string // e.g. "upstream"
	URL    string // upstream fetch URL
	Branch string // upstream branch to track, e.g. "main"
}

// loadForkConfig reads fork.* keys from the repo's git config.
// Missing remote/branch are filled from sensible defaults where possible.
func loadForkConfig(repo string) (forkConfig, error) {
	var c forkConfig
	c.Remote, _ = gitConfigGet(repo, "fork.upstreamRemote")
	c.URL, _ = gitConfigGet(repo, "fork.upstream")
	c.Branch, _ = gitConfigGet(repo, "fork.upstreamBranch")

	if c.Remote == "" {
		c.Remote = "upstream"
	}
	if c.URL == "" && !remoteExists(repo, c.Remote) {
		return c, fmt.Errorf("not initialized: run `forkman init` here first")
	}
	if c.Branch == "" {
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

// syncResult captures the outcome of syncing one repo for the summary table.
type syncResult struct {
	Repo   string
	Status string // "up-to-date", "merged N", "CONFLICT", "error"
	Detail string
}

// syncOptions controls the sync pipeline.
type syncOptions struct {
	Rebase bool
}

// syncOne runs the full sync pipeline for a single repo.
func syncOne(repo string, opt syncOptions) syncResult {
	res := syncResult{Repo: repo}

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

	// 2. Fetch upstream.
	if _, err := git(repo, "fetch", cfg.Remote, cfg.Branch); err != nil {
		res.Status, res.Detail = "error", "fetch failed: "+err.Error()
		return res
	}
	upstreamRef := cfg.Remote + "/" + cfg.Branch

	if dryRun {
		res.Status = "dry-run"
		res.Detail = fmt.Sprintf("would %s %s into %s", strategyVerb(opt), upstreamRef, branch)
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
	} else {
		_, mergeErr = git(repo, "merge", "--no-edit", upstreamRef)
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
		r := syncOne(repo, opt)
		fmt.Printf("    %s: %s\n", r.Status, r.Detail)
		results = append(results, r)
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
