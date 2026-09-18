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
	subs := make([]subFork, 0, len(byPrefix))
	for _, sf := range byPrefix {
		if sf.Mode == "" {
			sf.Mode = modeReplay // entries written before shadow mode existed
		}
		if sf.Remote == "" {
			if sf.Mode == modeShadow {
				sf.Remote = "origin"
			} else {
				sf.Remote = subRemoteName(sf.Prefix)
			}
		}
		subs = append(subs, *sf)
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Prefix < subs[j].Prefix })
	return subs, nil
}

// loadSubFork returns the entry for one prefix.
func loadSubFork(repo, prefix string) (subFork, error) {
	subs, err := loadSubForks(repo)
	if err != nil {
		return subFork{}, err
	}
	for _, s := range subs {
		if s.Prefix == prefix {
			return s, nil
		}
	}
	return subFork{}, fmt.Errorf("%s is not registered as a subdirectory fork in %s", prefix, repo)
}

// initSub registers a vendored subdirectory fork inside repo.
func initSub(repo, dir, upstream, remote, branch, tag, base, ignore string) error {
	prefix, err := relPrefix(repo, dir)
	if err != nil {
		return err
	}
	full := filepath.Join(repo, prefix)
	if !mustExist(full) {
		return fmt.Errorf("directory %s does not exist", full)
	}

	if branch != "" && tag != "" {
		return fmt.Errorf("cannot specify both --branch and --tag")
	}

	// A subdirectory that still has its own history gets shadow mode: the git
	// dir moves out of the worktree so the parent tracks plain files rather
	// than a gitlink, and upstream can still be merged with real history.
	if mustExist(filepath.Join(full, ".git")) {
		if base != "" {
			return fmt.Errorf("--base applies only to subdirectories without a .git "+
				"(%s has its own history, so its merge base is tracked by git)", prefix)
		}
		return initShadow(repo, prefix, upstream, remote, branch, tag, ignore)
	}

	if remote == "" || remote == "upstream" {
		remote = subRemoteName(prefix)
	}

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
	if upstream == "" {
		if u, err := remoteURL(repo, remote); err == nil {
			upstream = u
		}
	}

	if dryRun {
		fmt.Printf("[dry-run] would register %s in %s (upstream %s -> %s)\n", prefix, repo, remote, upstream)
		return nil
	}

	// Fetch so we can resolve the branch/tag and pin a base commit.
	if tag != "" {
		if _, err := gitRaw(repo, "fetch", "--force", remote, "tag", tag); err != nil {
			return fmt.Errorf("fetch tag %s from %s: %w", tag, remote, err)
		}
		if base == "" {
			base = "tags/" + tag
		}
	} else {
		if branch == "" {
			if _, err := gitRaw(repo, "fetch", remote); err != nil {
				return fmt.Errorf("fetch %s: %w", remote, err)
			}
			b, err := defaultBranch(repo, remote)
			if err != nil {
				return err
			}
			branch = b
		}
		if _, err := gitRaw(repo, "fetch", remote, branch); err != nil {
			return fmt.Errorf("fetch %s %s: %w", remote, branch, err)
		}
		if base == "" {
			base = remote + "/" + branch // default: the copy matches upstream tip today
		}
	}

	baseSHA, err := gitRaw(repo, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return fmt.Errorf("cannot resolve --base %q: %w", base, err)
	}

	for key, val := range map[string]string{
		"prefix":   prefix,
		"mode":     modeReplay,
		"remote":   remote,
		"upstream": upstream,
		"branch":   branch,
		"tag":      tag,
		"base":     baseSHA,
		"ignore":   ignore,
	} {
		if val == "" {
			continue
		}
		if err := gitConfigSet(repo, subKey(prefix, key), val); err != nil {
			return err
		}
	}
	if err := registerFork(repo); err != nil {
		return err
	}

	fmt.Printf("registered %s (subdirectory of %s)\n", prefix, repo)
	if tag != "" {
		fmt.Printf("  upstream: %s (tag %s, %s)\n", remote, tag, upstream)
	} else {
		fmt.Printf("  upstream: %s/%s (%s)\n", remote, branch, upstream)
	}
	fmt.Printf("  base:     %s\n", short(baseSHA))
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
	if targetTag == "" {
		targetTag = sf.Tag
	}

	if targetTag != "" {
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
		res.Detail = fmt.Sprintf("would replay %s onto %s/%s", upstreamRef, sf.Prefix, ignoreMsg)
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
		res.Status = "up-to-date"
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
		res.Status = "up-to-date"
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
			if werr != nil {
				res.Detail = "patch conflicts and could not be saved: " + werr.Error()
				return res
			}
			res.Detail = fmt.Sprintf("%s commit(s) conflict with local changes; patch saved to %s — "+
				"resolve with: git apply --3way --directory=%s/ %s", count, path, sf.Prefix, path)
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

	res.Status = fmt.Sprintf("merged %s", strings.TrimSpace(count))
	res.Detail = fmt.Sprintf("%s -> %s in %s/", short(sf.Base), short(newTip), sf.Prefix)
	return res
}

// parkPatch writes an unappliable patch under .git/forkman/ and returns its path.
func parkPatch(repo, prefix, patch string) (string, error) {
	dir := filepath.Join(repo, ".git", "forkman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := strings.ReplaceAll(prefix, "/", "-") + "-" + time.Now().Format("20060102-150405") + ".patch"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(patch), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
