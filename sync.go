package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// syncBranchPrefix is the prefix used for conflict branches parked on merge failure.
const syncBranchPrefix = "fork-sync/"

// forkConfig holds settings read from the current repo's git config.
type forkConfig struct {
	Remote string
	URL    string
	Branch string
	Tag    string
	Mode   string // "latest-rev" or "semantic-tags"
}

// loadForkConfig reads fork.* keys from the repo's git config.
// Missing remote/branch are filled from sensible defaults where possible.
func loadForkConfig(repo string) (forkConfig, error) {
	var c forkConfig
	c.Remote, _ = gitConfigGet(repo, "fork.upstreamRemote")
	c.URL, _ = gitConfigGet(repo, "fork.upstream")
	c.Branch, _ = gitConfigGet(repo, "fork.upstreamBranch")
	c.Tag, _ = gitConfigGet(repo, "fork.upstreamTag")
	c.Mode, _ = gitConfigGet(repo, "fork.syncMode")

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
	Repo        string
	Status      string // "up-to-date", "merged N", "CONFLICT", "error"
	Detail      string
	Tag         string // resolved tag if applicable
	PrevTag     string // tag before sync if applicable
	UpgradeType string // "MAJOR", "MINOR", "PATCH", or ""
}

// syncOptions controls the sync pipeline.
type syncOptions struct {
	Rebase     bool
	Tag        string // if set, sync to this specific tag instead of the tracked branch/tag
	Mode       string // "latest-rev" or "semantic-tags"
	TagPattern string // regex pattern for semantic tags
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

	prevTag := cfg.Tag
	if prevTag == "" {
		if out, err := gitRaw(repo, "describe", "--tags", "--abbrev=0", "HEAD"); err == nil {
			prevTag = cleanTag(strings.TrimSpace(out))
		}
	}
	res.PrevTag = prevTag

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

	effectiveMode := opt.Mode
	if effectiveMode == "" && targetTag == "" {
		effectiveMode = cfg.Mode
	}

	if effectiveMode == modeSemanticTags {
		latestTag, err := findLatestSemanticTag(repo, cfg.Remote, opt.TagPattern)
		if err != nil {
			res.Status, res.Detail = "error", err.Error()
			return res
		}
		targetTag = latestTag
		res.Tag = latestTag
	} else if effectiveMode == modeLatestRev {
		targetTag = ""
		if cfg.Branch == "" {
			if b, err := defaultBranch(repo, cfg.Remote); err == nil {
				cfg.Branch = b
			} else if b, err := currentBranch(repo); err == nil {
				cfg.Branch = b
			}
		}
	} else if targetTag == "" {
		targetTag = cfg.Tag
	}

	if targetTag != "" {
		res.Tag = targetTag
		if prevTag != targetTag {
			res.UpgradeType = semverUpgradeType(prevTag, targetTag)
		}
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
		upgradeMsg := ""
		if res.UpgradeType != "" {
			upgradeMsg = fmt.Sprintf(" [%s upgrade]", res.UpgradeType)
		}
		res.Detail = fmt.Sprintf("would %s %s into %s%s%s", strategyVerb(opt), upstreamRef, branch, ignoreMsg, upgradeMsg)
		return res
	}

	// 3. Already up to date?
	ahead, behind, err := aheadBehind(repo, upstreamRef, "HEAD")
	if err == nil && behind == 0 {
		if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
			_ = gitConfigSet(repo, "fork.upstreamTag", targetTag)
		}
		res.Status = "up-to-date"
		res.UpgradeType = ""
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
		if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
			_ = gitConfigSet(repo, "fork.upstreamTag", targetTag)
		}
		res.Status = fmt.Sprintf("merged %d", behind)
		if res.UpgradeType != "" {
			if prevTag != "" && prevTag != targetTag {
				res.Detail = fmt.Sprintf("from %s (%s upgrade: %s -> %s)", upstreamRef, res.UpgradeType, prevTag, targetTag)
			} else {
				res.Detail = fmt.Sprintf("from %s (%s upgrade)", upstreamRef, res.UpgradeType)
			}
		} else {
			res.Detail = fmt.Sprintf("from %s", upstreamRef)
		}
		return res
	}

	// 5. Conflict: abort, keep working branch clean, park upstream in a branch.
	if opt.Rebase {
		_, _ = git(repo, "rebase", "--abort")
	} else {
		_, _ = git(repo, "merge", "--abort")
	}

	parkBranch := fmt.Sprintf("%s%s-%s", syncBranchPrefix, branch, time.Now().Format("20060102"))
	targetCommit := upstreamRef
	if opt.Rebase {
		targetCommit = upstreamRef
	}
	if _, err := git(repo, "branch", "-f", parkBranch, targetCommit); err != nil {
		res.Status = "CONFLICT"
		res.Detail = fmt.Sprintf("merge conflict (failed to create branch %s: %v)", parkBranch, err)
		return res
	}

	res.Status = "CONFLICT"
	upgradeInfo := ""
	if res.UpgradeType != "" {
		upgradeInfo = fmt.Sprintf(" (%s upgrade)", res.UpgradeType)
	}
	res.Detail = fmt.Sprintf("parked upstream in %q%s — resolve with: git merge %s", parkBranch, upgradeInfo, parkBranch)
	return res
}

func strategyVerb(opt syncOptions) string {
	if opt.Rebase {
		return "rebase"
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
		case strings.HasPrefix(r.Status, "CONFLICT"):
			mark = "!!"
			hadFailure = true
			if os.Getenv("GITHUB_ACTIONS") == "true" {
				fmt.Printf("::warning::%s: %s (%s)\n", r.Repo, r.Status, r.Detail)
			}
		case r.Status == "error":
			mark = "!!"
			hadFailure = true
			if os.Getenv("GITHUB_ACTIONS") == "true" {
				fmt.Printf("::error::%s: %s (%s)\n", r.Repo, r.Status, r.Detail)
			}
		default:
			if r.UpgradeType != "" && os.Getenv("GITHUB_ACTIONS") == "true" {
				fmt.Printf("::notice::%s: Upstream upgraded (%s version bump: %s)\n", r.Repo, r.UpgradeType, r.Tag)
			}
		}
		upgradeSuffix := ""
		if r.UpgradeType != "" {
			upgradeSuffix = fmt.Sprintf(" (%s upgrade)", r.UpgradeType)
		}
		fmt.Printf("  [%s] %-10s %s%s\n", mark, r.Status, r.Repo, upgradeSuffix)
	}

	writeGitHubOutput(results)
	writeGitHubSummary(results, opt)

	return hadFailure
}

func writeGitHubOutput(results []syncResult) {
	outputFile := os.Getenv("GITHUB_OUTPUT")
	if outputFile == "" {
		return
	}
	f, err := os.OpenFile(outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	if len(results) > 0 {
		r := results[0]
		fmt.Fprintf(f, "status=%s\n", r.Status)
		if r.Tag != "" {
			fmt.Fprintf(f, "tag=%s\n", r.Tag)
		}
		fmt.Fprintf(f, "upgrade_type=%s\n", r.UpgradeType)
		fmt.Fprintf(f, "upgrade-type=%s\n", r.UpgradeType)
		synced := strings.HasPrefix(r.Status, "merged")
		fmt.Fprintf(f, "synced=%t\n", synced)
		if strings.HasPrefix(r.Status, "CONFLICT") {
			if idx := strings.Index(r.Detail, syncBranchPrefix); idx != -1 {
				parked := r.Detail[idx:]
				if endIdx := strings.IndexAny(parked, " \t\n\""); endIdx != -1 {
					parked = parked[:endIdx]
				}
				fmt.Fprintf(f, "parked_branch=%s\n", parked)
				fmt.Fprintf(f, "parked-branch=%s\n", parked)
			}
		}
	}
}

func writeGitHubSummary(results []syncResult, opt syncOptions) {
	summaryFile := os.Getenv("GITHUB_STEP_SUMMARY")
	if summaryFile == "" {
		return
	}
	f, err := os.OpenFile(summaryFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	var sb strings.Builder
	sb.WriteString("### Forkman Sync Summary\n\n")
	if opt.Mode != "" {
		sb.WriteString(fmt.Sprintf("- **Mode:** `%s`\n", opt.Mode))
	}
	if opt.Tag != "" {
		sb.WriteString(fmt.Sprintf("- **Tag:** `%s`\n", opt.Tag))
	}
	if len(results) > 0 && results[0].UpgradeType != "" {
		sb.WriteString(fmt.Sprintf("- **Upgrade Type:** **`%s`**\n", results[0].UpgradeType))
	}
	sb.WriteString("\n| Repository | Status | Upgrade | Detail |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- |\n")
	for _, r := range results {
		icon := "✅"
		if strings.HasPrefix(r.Status, "CONFLICT") {
			icon = "⚠️"
		} else if r.Status == "error" {
			icon = "❌"
		}
		upgradeBadge := "-"
		if r.UpgradeType != "" {
			upgradeBadge = fmt.Sprintf("**%s**", r.UpgradeType)
		}
		sb.WriteString(fmt.Sprintf("| `%s` | %s %s | %s | %s |\n", r.Repo, icon, r.Status, upgradeBadge, r.Detail))
	}
	sb.WriteString("\n")
	_, _ = f.WriteString(sb.String())
}
