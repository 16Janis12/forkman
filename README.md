# forkman

A tiny zero-dependency Go CLI for keeping private forks in sync with their
upstream (original) repositories.

Each fork's `origin` points at your private copy. `forkman` attaches the public
source as an `upstream` remote and pulls its changes in for you. When a merge
conflicts, it leaves your working branch untouched and **parks** the upstream
tip in a `fork-sync/<branch>-<date>` branch, so you can resolve on your own
schedule.

## Build

```sh
go build -o forkman .
# optionally: install to PATH
go install .
```

## Usage

```
forkman init   [--upstream URL] [--remote NAME] [--branch NAME]
forkman sync   [--all] [--rebase] [--dry-run] [PATH...]
forkman list
forkman status [PATH]
forkman remove [PATH]
```

### 1. Register a fork

Run inside a cloned fork:

```sh
forkman init --upstream https://github.com/original/project.git
```

This adds an `upstream` remote, records `fork.*` keys in the repo's git config,
and adds the repo to a global registry (`~/.config/forkman/registry.json`, or
`$XDG_CONFIG_HOME`). Re-running `init` is safe — it just updates the config.

Flags:
- `--upstream URL` — the original repository. Omit to adopt an existing remote.
- `--remote NAME` — remote name (default `upstream`).
- `--branch NAME` — upstream branch to track (default: the remote's HEAD).

### 2. Sync

```sh
forkman sync          # sync the current repo
forkman sync --all    # sync every registered fork, from anywhere
forkman sync ../other # sync a specific path
forkman sync --rebase # rebase onto upstream instead of merging
forkman sync --dry-run
```

Pipeline per repo:

1. Stash uncommitted changes (auto-restored afterward).
2. `git fetch upstream <branch>`.
3. If already up to date, report and skip.
4. `git merge --no-edit upstream/<branch>` (or `git rebase` with `--rebase`).
5. **On conflict**: abort the merge (your branch stays clean) and create
   `fork-sync/<branch>-<date>` at the upstream tip. Resolve later with:
   ```sh
   git merge fork-sync/main-20260709
   ```

Exit code is non-zero if any repo conflicted or errored — handy for cron.

### 3. Inspect

```sh
forkman list             # table of all forks: path, upstream, branch, clean/dirty
forkman status [PATH]    # divergence (ahead/behind), worktree state, parked branches
forkman remove [PATH]    # unregister a fork (does not touch the repo)
```

## Where state lives

- **Per-repo** — git config keys, so they travel with the repo:
  - `fork.upstream` — upstream URL
  - `fork.upstreamRemote` — remote name (default `upstream`)
  - `fork.upstreamBranch` — tracked branch
- **Global** — `~/.config/forkman/registry.json`, a list of fork paths for
  `--all`. Stale entries are pruned automatically.

## Automate

```sh
# nightly: sync all forks, log conflicts
0 3 * * *  forkman sync --all >> ~/.local/state/forkman.log 2>&1
```
