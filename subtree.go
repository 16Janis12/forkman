package main

// Subdirectory ("vendored") forks: a copy of an upstream repo living inside a
// parent repo, with its own .git removed so the parent tracks the files.
//
// There is no shared history with upstream, so merge/rebase cannot be used.
// Instead we record the upstream commit the copy corresponds to (fork.sub.*.base)
// and, on each sync, replay `git diff <base>..<newTip>` onto the subdirectory
// with `git apply --directory=<prefix>`. A successful apply is committed in the
// parent repo and the base advances. A patch that does not apply cleanly is
// written out for manual resolution; the working tree is left untouched.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sync modes for a vendored subdirectory.
const (
	// modeReplay: the copy has no history of its own; upstream changes are
	// replayed as a patch. Used when the subdirectory has no .git.
	modeReplay = "replay"
	// modeShadow: the copy keeps its full history in a git dir parked outside
	// the worktree, so upstream can be merged normally. Used when the
	// subdirectory still has a .git.
	modeShadow = "shadow"
)

// subFork is one vendored upstream copy inside a parent repo.
type subFork struct {
	Prefix string // repo-relative directory, no trailing slash, e.g. "vendor/proj"
	Mode   string // modeReplay or modeShadow
	Remote string // remote name (in the parent repo for replay, in the shadow repo for shadow)
	URL    string // upstream fetch URL
	Branch string // upstream branch to track
	Tag    string // upstream tag to track
	Base   string // replay mode: upstream commit the current copy corresponds to
	GitDir string // shadow mode: repo-relative path of the parked git dir
	Ignore string // comma-separated ignore patterns
}

func (s subFork) ref() string {
	if s.Tag != "" {
		return "tags/" + s.Tag
	}
	return s.Remote + "/" + s.Branch
}

// configKey builds the git config key for one field of this sub-fork.
func subKey(prefix, field string) string {
	return "fork.sub." + prefix + "." + field
}

// subRemoteName derives a default remote name from a repo-relative prefix.
func subRemoteName(prefix string) string {
	slug := strings.NewReplacer("/", "-", " ", "-", ".", "-").Replace(prefix)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "sub"
	}
	return "upstream-" + slug
}

// relPrefix resolves a user-supplied directory to a clean repo-relative prefix.
func relPrefix(repo, dir string) (string, error) {
	abs := absClean(dir)
	rel, err := filepath.Rel(repo, abs)
	if err != nil {
		return "", fmt.Errorf("%s is not inside %s", dir, repo)
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" {
		return "", fmt.Errorf("--dir must name a subdirectory, not the repo root")
	}
	if strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside the repo %s", dir, repo)
	}
	return rel, nil
}

// loadSubForks reads every fork.sub.* entry from the parent repo's git config.
func loadSubForks(repo string) ([]subFork, error) {
	out, err := gitRaw(repo, "config", "--local", "--get-regexp", `^fork\.sub\.`)
	if err != nil {
		return nil, nil // no entries
	}
	byPrefix := map[string]*subFork{}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		rest := strings.TrimPrefix(key, "fork.sub.")
		dot := strings.LastIndex(rest, ".")
		if dot < 1 {
			continue
		}
		prefix, field := rest[:dot], rest[dot+1:]
		sf := byPrefix[prefix]
		if sf == nil {
			sf = &subFork{Prefix: prefix}
			byPrefix[prefix] = sf
		}
		switch field {
		case "remote":
			sf.Remote = val
		case "upstream":
			sf.URL = val
		case "branch":
			sf.Branch = val
		case "tag":
			sf.Tag = val
		case "base":
			sf.Base = val
		case "mode":
			sf.Mode = val
		case "gitdir":
			sf.GitDir = val
		case "ignore":
			sf.Ignore = val
		}
	}
	var res []subFork
	for _, sf := range byPrefix {
		if sf.Mode == "" {
			continue // incomplete entry
		}
		res = append(res, *sf)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Prefix < res[j].Prefix })
	return res, nil
}

// loadSubFork reads configuration for a single sub-fork by its prefix.
func loadSubFork(repo, prefix string) (subFork, error) {
	subs, err := loadSubForks(repo)
	if err != nil {
		return subFork{}, err
	}
	for _, sf := range subs {
		if sf.Prefix == prefix {
			return sf, nil
		}
	}
	return subFork{}, fmt.Errorf("no sub-fork registered at %s", prefix)
}

// initSub registers a vendored copy. If the copy still has a .git, it delegates
// to initShadow; otherwise it registers a replay-mode sub-fork.
func initSub(repo, dir, upstream, remote, branch, tag, base, ignore string) error {
	prefix, err := relPrefix(repo, dir)
	if err != nil {
		return err
	}
	full := filepath.Join(repo, prefix)
	if !mustExist(full) {
		return fmt.Errorf("directory %s does not exist", full)
	}

	// Mode dispatch: if prefix/.git is a directory, use shadow mode.
	if fi, err := os.Stat(filepath.Join(full, ".git")); err == nil && fi.IsDir() {
		if base != "" {
			return fmt.Errorf("--base is only for replay mode (when %s has no .git)", prefix)
		}
		return initShadow(repo, prefix, upstream, remote, branch, tag, ignore)
	}

	// Replay mode.
	return initReplay(repo, prefix, upstream, remote, branch, tag, base, ignore)
}

// initReplay registers a replay-mode sub-fork.
func initReplay(repo, prefix, upstream, remote, branch, tag, base, ignore string) error {
	if branch != "" && tag != "" {
		return fmt.Errorf("cannot specify both --branch and --tag")
	}

	if remote == "" || remote == "upstream" {
		remote = subRemoteName(prefix)
	}

	// Attach or adopt the upstream remote in the parent repo.
	if remoteExists(repo, remote) {
		if upstream != "" {
			if _, err := git(repo, "remote", "set-url", remote, upstream); err != nil {
				return err
			}
		}
	} else {
		if upstream == "" {
			return fmt.Errorf("remote %q does not exist; provide --upstream URL", remote)
		}
		if _, err := git(repo, "remote", "add", remote, upstream); err != nil {
			return err
		}
	}

	url := upstream
	if url == "" {
		if u, err := remoteURL(repo, remote); err == nil {
			url = u
		}
	}

	// Fetch upstream to resolve default branch / verify base.
	if !dryRun {
		if tag != "" {
			if _, err := gitRaw(repo, "fetch", "--force", remote, "tag", tag); err != nil {
				return fmt.Errorf("fetching upstream tag %q from %s: %w", tag, remote, err)
			}
		} else {
			if _, err := gitRaw(repo, "fetch", remote); err != nil {
				return fmt.Errorf("fetching upstream from %s: %w", remote, err)
			}
		}
	}

	if tag == "" && branch == "" {
		if b, err := defaultBranch(repo, remote); err == nil {
			branch = b
		} else {
			branch = "main"
		}
	}

	// Resolve base commit.
	baseSHA := base
	if baseSHA == "" {
		targetRef := remote + "/" + branch
		if tag != "" {
			targetRef = "tags/" + tag
		}
		tip, err := gitRaw(repo, "rev-parse", "--verify", targetRef+"^{commit}")
		if err != nil {
			return fmt.Errorf("cannot determine upstream tip (%s): provide --base <commit>", err)
		}
		baseSHA = tip
	} else {
		resolved, err := gitRaw(repo, "rev-parse", "--verify", baseSHA+"^{commit}")
		if err != nil {
			return fmt.Errorf("invalid base %q: %w", baseSHA, err)
		}
		baseSHA = resolved
	}

	if dryRun {
		fmt.Printf("[dry-run] would register %s/ (replay mode, base %s, remote %s -> %s)\n",
			prefix, short(baseSHA), remote, url)
		return nil
	}

	for _, kv := range [][2]string{
		{"mode", modeReplay},
		{"remote", remote},
		{"upstream", url},
		{"branch", branch},
		{"tag", tag},
		{"base", baseSHA},
		{"ignore", ignore},
	} {
		if kv[1] != "" {
			if err := gitConfigSet(repo, subKey(prefix, kv[0]), kv[1]); err != nil {
				return err
			}
		}
	}

	fmt.Printf("registered %s/ (replay mode)\n", prefix)
	if tag != "" {
		fmt.Printf("  upstream: %s (tag %s, %s)\n", remote, tag, url)
	} else {
		fmt.Printf("  upstream: %s/%s (%s)\n", remote, branch, url)
	}
	fmt.Printf("  base:     %s\n", short(baseSHA))
	if ignore != "" {
		fmt.Printf("  ignored:  %s\n", ignore)
	}
	fmt.Printf("run `forkman sync` to replay upstream changes onto %s.\n", prefix)
	return nil
}

// removeSub drops the config for one subdirectory fork. Returns whether it existed.
func removeSub(repo, dir string) (bool, error) {
	prefix, err := relPrefix(repo, dir)
	if err != nil {
		return false, err
	}
	sf, err := loadSubFork(repo, prefix)
	if err != nil {
		return false, nil
	}
	if _, err := git(repo, "config", "--local", "--remove-section", "fork.sub."+prefix); err != nil {
		return false, err
	}
	if sf.Mode == modeShadow && !dryRun {
		// Put the history back where it came from, leaving an ordinary nested
		// clone. The parent still tracks the files until you tell it otherwise.
		if err := restoreShadow(repo, sf); err != nil {
			fmt.Printf("unregistered %s, but restoring its .git failed: %v\n", prefix, err)
			return true, nil
		}
		fmt.Printf("unregistered %s (history moved back to %s/.git)\n", prefix, prefix)
		return true, nil
	}
	fmt.Printf("unregistered %s (remote %q left in place)\n", prefix, sf.Remote)
	return true, nil
}

// syncSub dispatches to the pipeline for this sub-fork's mode.
func syncSub(repo string, sf subFork, opt syncOptions) syncResult {
	if sf.Mode == modeShadow {
		return syncShadow(repo, sf, opt)
	}
	return syncReplay(repo, sf, opt)
}

// syncReplay replays upstream's diff onto a vendored copy that has no history.
// The result is named so the deferred stash-pop can amend it after `return res`.
func syncReplay(repo string, sf subFork, opt syncOptions) (res syncResult) {
	res = syncResult{Repo: repo + " [" + sf.Prefix + "]"}

	var upstreamRef string
	targetTag := opt.Tag

	prevTag := sf.Tag
	res.PrevTag = prevTag

	effectiveMode := opt.Mode
	if effectiveMode == "" && targetTag == "" {
		effectiveMode, _ = gitConfigGet(repo, subKey(sf.Prefix, "syncMode"))
	}

	if effectiveMode == modeSemanticTags {
		latestTag, err := findLatestSemanticTag(repo, sf.Remote, opt.TagPattern)
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
		if _, err := git(repo, "fetch", "--force", sf.Remote, "tag", targetTag); err != nil {
			res.Status, res.Detail = "error", "fetch failed: "+err.Error()
			return res
		}
		upstreamRef = "tags/" + targetTag
	} else {
		if sf.Branch == "" {
			b, err := defaultBranch(repo, sf.Remote)
			if err != nil {
				res.Status, res.Detail = "error", err.Error()
				return res
			}
			sf.Branch = b
		}
		upstreamRef = sf.Remote + "/" + sf.Branch

		if _, err := git(repo, "fetch", sf.Remote, sf.Branch); err != nil {
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
		res.Detail = fmt.Sprintf("would replay %s onto %s/%s%s", upstreamRef, sf.Prefix, ignoreMsg, upgradeMsg)
		return res
	}

	if sf.Base == "" {
		res.Status = "error"
		res.Detail = "no base commit recorded; re-run `forkman init --dir " + sf.Prefix + " --base <ref>`"
		return res
	}

	newTip, err := gitRaw(repo, "rev-parse", "--verify", upstreamRef+"^{commit}")
	if err != nil {
		res.Status, res.Detail = "error", err.Error()
		return res
	}
	if newTip == sf.Base {
		if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
			_ = gitConfigSet(repo, subKey(sf.Prefix, "tag"), targetTag)
		}
		res.Status = "up-to-date"
		res.UpgradeType = ""
		res.Detail = fmt.Sprintf("%s at %s", sf.Prefix, short(newTip))
		return res
	}

	diffArgs := []string{"diff", "--binary", "--no-color", sf.Base, newTip}
	if len(patterns) > 0 {
		diffArgs = append(diffArgs, "--", ".")
		diffArgs = append(diffArgs, buildPathspecExcludes(patterns)...)
	}
	patch, err := gitOut(repo, diffArgs...)
	if err != nil {
		res.Status, res.Detail = "error", "diff failed: "+err.Error()
		return res
	}
	count, _ := gitRaw(repo, "rev-list", "--count", sf.Base+".."+newTip)
	if strings.TrimSpace(patch) == "" {
		// Different commits, identical trees (or all changes ignored) — just advance the pin.
		_ = gitConfigSet(repo, subKey(sf.Prefix, "base"), newTip)
		if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
			_ = gitConfigSet(repo, subKey(sf.Prefix, "tag"), targetTag)
		}
		res.Status = "up-to-date"
		res.UpgradeType = ""
		res.Detail = fmt.Sprintf("%s commit(s) upstream, no file changes; base -> %s", count, short(newTip))
		return res
	}

	// Stash dirty work so the apply lands on a clean index.
	stashed := false
	if dirty, _ := isDirty(repo); dirty {
		if _, err := git(repo, "stash", "push", "-u", "-m", "forkman-autostash"); err != nil {
			res.Status, res.Detail = "error", "stash failed: "+err.Error()
			return res
		}
		stashed = true
	}
	defer func() {
		if stashed {
			if _, err := git(repo, "stash", "pop"); err != nil {
				res.Detail += " (WARNING: `git stash pop` failed — restore manually: " + err.Error() + ")"
			}
		}
	}()

	// Apply strictly first; fall back to a 3-way merge so local edits near an
	// upstream hunk do not fail the whole sync.
	dirFlag := "--directory=" + sf.Prefix + "/"
	if _, err := gitIn(repo, patch, "apply", dirFlag, "--index", "-"); err != nil {
		if _, err := gitIn(repo, patch, "apply", dirFlag, "--3way", "-"); err != nil {
			// Real conflict: undo the partial 3-way result and park the patch so
			// the working tree is left exactly as we found it.
			_, _ = gitRaw(repo, "reset", "--hard", "HEAD")
			_, _ = gitRaw(repo, "clean", "-fdq", "--", sf.Prefix)
			path, werr := parkPatch(repo, sf.Prefix, patch)
			res.Status = "CONFLICT"
			upgradeInfo := ""
			if res.UpgradeType != "" {
				upgradeInfo = fmt.Sprintf(" (%s upgrade)", res.UpgradeType)
			}
			if werr != nil {
				res.Detail = fmt.Sprintf("patch conflicts and could not be saved%s: %v", upgradeInfo, werr)
				return res
			}
			res.Detail = fmt.Sprintf("%s commit(s) conflict with local changes%s; patch saved to %s — "+
				"resolve with: git apply --3way --directory=%s/ %s", count, upgradeInfo, path, sf.Prefix, path)
			return res
		}
		if _, err := gitRaw(repo, "add", "-A", "--", sf.Prefix); err != nil {
			res.Status, res.Detail = "error", "staging 3-way result failed: "+err.Error()
			return res
		}
	}

	msg := fmt.Sprintf("forkman: sync %s from %s (%s..%s)", sf.Prefix, upstreamRef, short(sf.Base), short(newTip))
	if _, err := gitRaw(repo, "commit", "-m", msg); err != nil {
		res.Status, res.Detail = "error", "commit failed: "+err.Error()
		return res
	}
	if err := gitConfigSet(repo, subKey(sf.Prefix, "base"), newTip); err != nil {
		res.Status, res.Detail = "error", "committed but failed to record new base: "+err.Error()
		return res
	}
	if !dryRun && effectiveMode == modeSemanticTags && targetTag != "" {
		_ = gitConfigSet(repo, subKey(sf.Prefix, "tag"), targetTag)
	}

	res.Status = fmt.Sprintf("merged %s", strings.TrimSpace(count))
	if res.UpgradeType != "" {
		if prevTag != "" && prevTag != targetTag {
			res.Detail = fmt.Sprintf("%s -> %s in %s/ (%s upgrade: %s -> %s)", short(sf.Base), short(newTip), sf.Prefix, res.UpgradeType, prevTag, targetTag)
		} else {
			res.Detail = fmt.Sprintf("%s -> %s in %s/ (%s upgrade)", short(sf.Base), short(newTip), sf.Prefix, res.UpgradeType)
		}
	} else {
		res.Detail = fmt.Sprintf("%s -> %s in %s/", short(sf.Base), short(newTip), sf.Prefix)
	}
	return res
}

// parkPatch writes patch to .git/forkman/<prefix>-<date>.patch.
func parkPatch(repo, prefix, patch string) (string, error) {
	dir := filepath.Join(repo, ".git", "forkman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	slug := strings.Trim(strings.NewReplacer("/", "-", " ", "-").Replace(prefix), "-")
	name := fmt.Sprintf("%s-%s.patch", slug, time.Now().Format("20060102"))
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte(patch), 0o644); err != nil {
		return "", err
	}
	return target, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
