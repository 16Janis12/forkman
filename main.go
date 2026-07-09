// Command forkman manages private forks: it registers each fork with its
// upstream (original) remote and syncs upstream changes in, parking conflicts
// in a dedicated branch so the working branch stays clean.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

const usage = `forkman — manage your private forks

Usage:
  forkman init   [--upstream URL] [--remote NAME] [--branch NAME]
  forkman sync   [--all] [--rebase] [--dry-run] [PATH...]
  forkman list
  forkman status [PATH]
  forkman remove [PATH]

Commands:
  init    Register the current repo and attach its upstream (original) remote.
  sync    Fetch upstream and merge it in. On conflict, park upstream in a
          branch (fork-sync/<branch>-<date>) and leave your branch untouched.
  list    Show all registered forks.
  status  Show divergence from upstream for one repo.
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
	fs.BoolVar(&dryRun, "dry-run", false, "print git commands without mutating")
	fs.Parse(args)

	repo, err := repoRoot(".")
	if err != nil {
		return err
	}
	repo = absClean(repo)

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
	if *branch != "" {
		if err := gitConfigSet(repo, "fork.upstreamBranch", *branch); err != nil {
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
	fmt.Printf("registered %s\n  upstream: %s (%s)\n", repo, *remote, url)
	fmt.Printf("run `forkman sync` to pull upstream changes in.\n")
	return nil
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	all := fs.Bool("all", false, "sync every registered fork")
	rebase := fs.Bool("rebase", false, "rebase onto upstream instead of merging")
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

	if runSync(repos, syncOptions{Rebase: *rebase}) {
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
		cfg, err := loadForkConfig(repo)
		up, br, state := "?", "?", "?"
		if err == nil {
			up, br = cfg.URL, cfg.Branch
		}
		if dirty, err := isDirty(repo); err == nil {
			if dirty {
				state = "dirty"
			} else {
				state = "clean"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", repo, up, br, state)
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

	cfg, err := loadForkConfig(repo)
	if err != nil {
		return err
	}
	branch, _ := currentBranch(repo)
	upstreamRef := cfg.Remote + "/" + cfg.Branch

	fmt.Printf("repo:     %s\n", repo)
	fmt.Printf("branch:   %s\n", branch)
	fmt.Printf("upstream: %s (%s)\n", upstreamRef, cfg.URL)

	// Fetch quietly so ahead/behind reflects the remote's current tip.
	if !dryRun {
		_, _ = gitRaw(repo, "fetch", cfg.Remote, cfg.Branch)
	}
	if ahead, behind, err := aheadBehind(repo, upstreamRef, "HEAD"); err == nil {
		fmt.Printf("divergence: %d ahead, %d behind %s\n", ahead, behind, upstreamRef)
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

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	fs.Parse(args)

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	repo := absClean(target)
	if root, err := repoRoot(target); err == nil {
		repo = absClean(root)
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
