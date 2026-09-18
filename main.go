// Command forkman manages private forks: it registers each fork with its
// upstream (original) remote and syncs upstream changes in, parking conflicts
// in a dedicated branch so the working branch stays clean.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

const usage = `forkman — manage your private forks

Usage:
  forkman init   [--upstream URL] [--remote NAME] [--branch NAME] [--tag TAG] [--ignore PATTERNS]
                 [--dir SUBDIR [--base REF]]
  forkman sync   [--all] [--rebase] [--tag TAG] [--dry-run] [PATH...]
  forkman list
  forkman status [PATH]
  forkman ignore [--dir SUBDIR] [add|remove] [PATTERN]
  forkman remove [PATH] [--dir SUBDIR]

Commands:
  init    Register the current repo and attach its upstream (original) remote.
          With --tag, track a specific upstream tag instead of a branch.
          With --dir, register a vendored subdirectory instead — a copy of an
          upstream repo living inside this one. Two modes, picked automatically:
            shadow  the copy still has its .git. It is moved to
                    .git/forkman/<name>.git so the parent tracks plain files
                    instead of a gitlink, and upstream keeps a real merge base.
            replay  the copy's .git was deleted. Upstream changes are replayed
                    as a patch, from the commit recorded by --base.
          With --ignore, specify folders or files to keep local and ignore from sync.
  sync    Fetch upstream and merge it in. On conflict, park upstream in a
          branch (fork-sync/<branch>-<date>) and leave your branch untouched.
          With --tag, sync to a specific upstream tag instead of the tracked branch.
          Vendored subdirectories sync per their mode; either way the result is
          committed to the parent, so "git push" there publishes everything.
  list    Show all registered forks.
  status  Show divergence from upstream for one repo.
  ignore  List, add, or remove ignored patterns (deviating folders/files).
  remove  Unregister a fork (does not modify the repo).

Global flags:
  --dry-run  Print git commands without mutating anything.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "sync":
		err = cmdSync(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "ignore":
		err = cmdIgnore(os.Args[2:])
	case "remove", "rm":
		err = cmdRemove(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	upstream := fs.String("upstream", "", "URL of the upstream (original) repository")
	remote := fs.String("remote", "upstream", "name for the upstream remote")
	branch := fs.String("branch", "", "upstream branch to track (default: its HEAD)")
	tag := fs.String("tag", "", "upstream tag to track")
	dir := fs.String("dir", "", "register a vendored subdirectory instead of the whole repo")
	base := fs.String("base", "", "with --dir: upstream commit the copy matches (default: current tip)")
	ignore := fs.String("ignore", "", "comma-separated folder/file patterns to ignore from upstream sync")
	fs.BoolVar(&dryRun, "dry-run", false, "print git commands without mutating")
	fs.Parse(args)

	cleanTagVal := cleanTag(*tag)
	if *branch != "" && cleanTagVal != "" {
		return fmt.Errorf("cannot specify both --branch and --tag")
	}

	repo, err := repoRoot(".")
	if err != nil {
		return err
	}
	repo = absClean(repo)

	if *dir != "" {
		return initSub(repo, *dir, *upstream, *remote, *branch, cleanTagVal, *base, *ignore)
	}
	if *base != "" {
		return fmt.Errorf("--base only applies together with --dir")
	}

	// Attach or adopt the upstream remote.
	if remoteExists(repo, *remote) {
		if *upstream != "" {
			if _, err := git(repo, "remote", "set-url", *remote, *upstream); err != nil {
				return err
			}
		}
	} else {
		if *upstream == "" {
			return fmt.Errorf("remote %q does not exist; provide --upstream URL", *remote)
		}
		if _, err := git(repo, "remote", "add", *remote, *upstream); err != nil {
			return err
		}
	}

	// Resolve the effective URL to record.
	url := *upstream
	if url == "" {
		if u, err := remoteURL(repo, *remote); err == nil {
			url = u
		}
	}

	if err := gitConfigSet(repo, "fork.upstreamRemote", *remote); err != nil {
		return err
	}
	if url != "" {
		if err := gitConfigSet(repo, "fork.upstream", url); err != nil {
			return err
		}
	}
	if cleanTagVal != "" {
		if err := gitConfigSet(repo, "fork.upstreamTag", cleanTagVal); err != nil {
			return err
		}
		_, _ = gitRaw(repo, "config", "--local", "--unset", "fork.upstreamBranch")
	} else if *branch != "" {
		if err := gitConfigSet(repo, "fork.upstreamBranch", *branch); err != nil {
			return err
		}
		_, _ = gitRaw(repo, "config", "--local", "--unset", "fork.upstreamTag")
	}
	if *ignore != "" {
		if err := gitConfigSet(repo, "fork.ignore", *ignore); err != nil {
			return err
		}
	}

	if dryRun {
		fmt.Printf("[dry-run] would register %s (upstream %s -> %s)\n", repo, *remote, url)
		return nil
	}
	if err := registerFork(repo); err != nil {
		return err
	}
	fmt.Printf("registered %s\n", repo)
	if cleanTagVal != "" {
		fmt.Printf("  upstream: %s (tag %s, %s)\n", *remote, cleanTagVal, url)
	} else if *branch != "" {
		fmt.Printf("  upstream: %s/%s (%s)\n", *remote, *branch, url)
	} else {
		fmt.Printf("  upstream: %s (%s)\n", *remote, url)
	}
	if *ignore != "" {
		fmt.Printf("  ignored:  %s\n", *ignore)
	}
	fmt.Printf("run `forkman sync` to pull upstream changes in.\n")
	return nil
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	all := fs.Bool("all", false, "sync every registered fork")
	rebase := fs.Bool("rebase", false, "rebase onto upstream instead of merging")
	tag := fs.String("tag", "", "sync to a specific upstream tag")
	fs.BoolVar(&dryRun, "dry-run", false, "print git commands without mutating")
	fs.Parse(args)

	var repos []string
	switch {
	case *all:
		r, err := loadRegistry()
		if err != nil {
			return err
		}
		if len(r) == 0 {
			return fmt.Errorf("no forks registered; run `forkman init` in a fork first")
		}
		repos = r
	case fs.NArg() > 0:
		for _, p := range fs.Args() {
			root, err := repoRoot(p)
			if err != nil {
				return err
			}
			repos = append(repos, absClean(root))
		}
	default:
		root, err := repoRoot(".")
		if err != nil {
			return err
		}
		repos = []string{absClean(root)}
	}

	if runSync(repos, syncOptions{Rebase: *rebase, Tag: cleanTag(*tag)}) {
		os.Exit(1)
	}
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	fs.Parse(args)

	repos, err := loadRegistry()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Println("no forks registered.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tUPSTREAM\tBRANCH\tSTATE")
	for _, repo := range repos {
		state := "?"
		if dirty, err := isDirty(repo); err == nil {
			if dirty {
				state = "dirty"
			} else {
				state = "clean"
			}
		}
		if hasRepoFork(repo) {
			up, br := "?", "?"
			if cfg, err := loadForkConfig(repo); err == nil {
				up = cfg.URL
				if cfg.Tag != "" {
					br = "tag:" + cfg.Tag
				} else {
					br = cfg.Branch
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", repo, up, br, state)
		}
		subs, _ := loadSubForks(repo)
		for _, sf := range subs {
			target := sf.Branch
			if sf.Tag != "" {
				target = "tag:" + sf.Tag
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				filepath.Join(repo, sf.Prefix)+"/ ("+sf.Mode+")", sf.URL, target, state)
		}
	}
	return w.Flush()
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Parse(args)

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	repo, err := repoRoot(target)
	if err != nil {
		return err
	}
	repo = absClean(repo)

	branch, _ := currentBranch(repo)
	fmt.Printf("repo:     %s\n", repo)
	fmt.Printf("branch:   %s\n", branch)

	if hasRepoFork(repo) {
		cfg, err := loadForkConfig(repo)
		if err != nil {
			return err
		}
		var upstreamRef string
		if cfg.Tag != "" {
			upstreamRef = "tags/" + cfg.Tag
			fmt.Printf("upstream: %s (%s)\n", upstreamRef, cfg.URL)
			if !dryRun {
				_, _ = gitRaw(repo, "fetch", "--force", cfg.Remote, "tag", cfg.Tag)
			}
		} else {
			upstreamRef = cfg.Remote + "/" + cfg.Branch
			fmt.Printf("upstream: %s (%s)\n", upstreamRef, cfg.URL)
			if !dryRun {
				_, _ = gitRaw(repo, "fetch", cfg.Remote, cfg.Branch)
			}
		}
		if ahead, behind, err := aheadBehind(repo, upstreamRef, "HEAD"); err == nil {
			fmt.Printf("divergence: %d ahead, %d behind %s\n", ahead, behind, upstreamRef)
		}
		patterns := loadIgnorePatterns(repo, repo, "fork.ignore")
		if len(patterns) > 0 {
			fmt.Printf("ignored:  %s\n", strings.Join(patterns, ", "))
		}
	}

	subs, _ := loadSubForks(repo)
	for _, sf := range subs {
		fmt.Printf("vendored: %s/ (%s mode)\n", sf.Prefix, sf.Mode)
		fmt.Printf("  upstream: %s (%s)\n", sf.ref(), sf.URL)
		subPatterns := loadIgnorePatterns(repo, filepath.Join(repo, sf.Prefix), subKey(sf.Prefix, "ignore"))
		if len(subPatterns) > 0 {
			fmt.Printf("  ignored:  %s\n", strings.Join(subPatterns, ", "))
		}

		if sf.Mode == modeShadow {
			fmt.Printf("  history:  %s\n", sf.GitDir)
			if !dryRun {
				if sf.Tag != "" {
					_, _ = gitShadowRaw(repo, sf, "fetch", "--force", sf.Remote, "tag", sf.Tag)
				} else {
					_, _ = gitShadowRaw(repo, sf, "fetch", sf.Remote, sf.Branch)
				}
			}
			ahead, behind, err := aheadBehindShadow(repo, sf, sf.ref())
			if err != nil {
				fmt.Printf("  divergence: unknown (%v)\n", err)
				continue
			}
			fmt.Printf("  divergence: %d ahead, %d behind %s\n", ahead, behind, sf.ref())
			// Edits arrive via the parent repo, so they are not in the shadow
			// repo's history until the next sync commits them.
			if out, err := gitShadowRaw(repo, sf, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
				fmt.Printf("  local:    uncommitted changes, will be committed on next sync\n")
			}
			continue
		}

		if !dryRun {
			if sf.Tag != "" {
				_, _ = gitRaw(repo, "fetch", "--force", sf.Remote, "tag", sf.Tag)
			} else {
				_, _ = gitRaw(repo, "fetch", sf.Remote, sf.Branch)
			}
		}
		tip, err := gitRaw(repo, "rev-parse", "--verify", sf.ref()+"^{commit}")
		if err != nil {
			fmt.Printf("  base:     %s (upstream unreachable)\n", short(sf.Base))
			continue
		}
		n, _ := gitRaw(repo, "rev-list", "--count", sf.Base+".."+tip)
		fmt.Printf("  base:     %s -> tip %s (%s commit(s) pending)\n", short(sf.Base), short(tip), n)
	}
	if !hasRepoFork(repo) && len(subs) == 0 {
		return fmt.Errorf("not initialized: run `forkman init` here first")
	}

	if dirty, _ := isDirty(repo); dirty {
		fmt.Println("worktree: dirty")
	} else {
		fmt.Println("worktree: clean")
	}
	if sb := syncBranches(repo); len(sb) > 0 {
		fmt.Println("parked conflict branches:")
		for _, b := range sb {
			fmt.Printf("  %s\n", b)
		}
	}
	return nil
}

func cmdIgnore(args []string) error {
	fs := flag.NewFlagSet("ignore", flag.ExitOnError)
	dir := fs.String("dir", "", "sub-fork directory")
	fs.Parse(args)

	repo, err := repoRoot(".")
	if err != nil {
		return err
	}
	repo = absClean(repo)

	workDir := repo
	configKey := "fork.ignore"
	if *dir != "" {
		prefix, err := relPrefix(repo, *dir)
		if err != nil {
			return err
		}
		workDir = filepath.Join(repo, prefix)
		configKey = subKey(prefix, "ignore")
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		patterns := loadIgnorePatterns(repo, workDir, configKey)
		if len(patterns) == 0 {
			fmt.Println("no ignored patterns configured.")
			return nil
		}
		fmt.Printf("ignored patterns for %s:\n", workDir)
		for _, p := range patterns {
			fmt.Printf("  %s\n", p)
		}
		return nil
	}

	action := remaining[0]
	switch action {
	case "list", "ls":
		patterns := loadIgnorePatterns(repo, workDir, configKey)
		if len(patterns) == 0 {
			fmt.Println("no ignored patterns configured.")
			return nil
		}
		fmt.Printf("ignored patterns for %s:\n", workDir)
		for _, p := range patterns {
			fmt.Printf("  %s\n", p)
		}
		return nil
	case "add":
		if len(remaining) < 2 {
			return fmt.Errorf("usage: forkman ignore [--dir SUBDIR] add <pattern>")
		}
		newPattern := remaining[1]
		existing, _ := gitConfigGet(repo, configKey)
		var list []string
		if existing != "" {
			list = strings.Split(existing, ",")
		}
		list = append(list, newPattern)
		if err := gitConfigSet(repo, configKey, strings.Join(list, ",")); err != nil {
			return err
		}
		fmt.Printf("added ignore pattern %q to %s\n", newPattern, configKey)
		return nil
	case "remove", "rm":
		if len(remaining) < 2 {
			return fmt.Errorf("usage: forkman ignore [--dir SUBDIR] remove <pattern>")
		}
		remPattern := remaining[1]
		existing, _ := gitConfigGet(repo, configKey)
		if existing == "" {
			fmt.Printf("pattern %q not found in %s\n", remPattern, configKey)
			return nil
		}
		var list []string
		for _, p := range strings.Split(existing, ",") {
			if strings.TrimSpace(p) != strings.TrimSpace(remPattern) {
				list = append(list, strings.TrimSpace(p))
			}
		}
		if len(list) == 0 {
			_, _ = gitRaw(repo, "config", "--local", "--unset", configKey)
		} else {
			if err := gitConfigSet(repo, configKey, strings.Join(list, ",")); err != nil {
				return err
			}
		}
		fmt.Printf("removed ignore pattern %q from %s\n", remPattern, configKey)
		return nil
	default:
		newPattern := action
		existing, _ := gitConfigGet(repo, configKey)
		var list []string
		if existing != "" {
			list = strings.Split(existing, ",")
		}
		list = append(list, newPattern)
		if err := gitConfigSet(repo, configKey, strings.Join(list, ",")); err != nil {
			return err
		}
		fmt.Printf("added ignore pattern %q to %s\n", newPattern, configKey)
		return nil
	}
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	dir := fs.String("dir", "", "unregister only this vendored subdirectory")
	fs.Parse(args)

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	repo := absClean(target)
	if root, err := repoRoot(target); err == nil {
		repo = absClean(root)
	}

	if *dir != "" {
		found, err := removeSub(repo, *dir)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%s is not registered as a vendored fork of %s", *dir, repo)
		}
		// Drop the repo from the registry once nothing is left to sync.
		if subs, _ := loadSubForks(repo); len(subs) == 0 && !hasRepoFork(repo) {
			_, _ = unregisterFork(repo)
		}
		return nil
	}

	found, err := unregisterFork(repo)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s is not registered", repo)
	}
	fmt.Printf("unregistered %s\n", repo)
	return nil
}
