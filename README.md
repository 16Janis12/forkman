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
forkman init   [--upstream URL] [--remote NAME] [--branch NAME] [--ignore PATTERNS]
forkman sync   [--all] [--rebase] [--dry-run] [PATH...]
forkman list
forkman status [PATH]
forkman ignore [--dir SUBDIR] [add|remove] [PATTERN]
forkman remove [PATH]
```

### 1. Register a fork

Run inside a cloned fork:

```sh
forkman init --upstream https://github.com/original/project.git --ignore "docs,config/*.yaml"
```

This adds an `upstream` remote, records `fork.*` keys in the repo's git config,
and adds the repo to a global registry (`~/.config/forkman/registry.json`, or
`$XDG_CONFIG_HOME`). Re-running `init` is safe — it just updates the config.

Flags:
- `--upstream URL` — the original repository. Omit to adopt an existing remote.
- `--remote NAME` — remote name (default `upstream`).
- `--branch NAME` — upstream branch to track (default: the remote's HEAD).
- `--ignore PATTERNS` — comma-separated folder or file patterns to ignore from upstream sync.

You can also use a `.forkignore` file in the repo root (or vendored subdirectory) to define ignored paths, or use `forkman ignore add <pattern>`.

### 2. Ignoring folders / files from sync

When you customize files or folders in your fork that should **not** receive upstream changes (or cause merge conflicts):

- Add patterns to a `.forkignore` file in the repo (or sub-fork directory), e.g.:
  ```
  docs/
  custom_config.json
  branding/
  ```
- Or configure via CLI:
  ```sh
  forkman ignore add docs
  forkman ignore list
  ```

During `forkman sync`:
- In **whole-repo** and **shadow** mode, local versions of ignored files are strictly preserved and upstream modifications or new files in those folders are discarded. If upstream changes produce conflicts only within ignored folders, they are resolved automatically.
- In **replay** mode, ignored paths are excluded from upstream diffs so local files remain untouched.

### 3. Sync

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

### 3. Vendored subdirectories

If you cloned an upstream repo *into* another repo, there is one push that
matters — the parent's. `forkman init --dir` registers the subdirectory so
upstream changes land in it and ride along with the parent's own commits:

```sh
cd parent-repo
forkman init --dir vendor/proj --upstream https://github.com/original/proj.git
forkman sync
git push            # the parent's remote, the only one you push to
```

Two modes, picked automatically from whether the subdirectory still has a `.git`.

#### Shadow mode (`.git` still there)

Git records any directory containing a `.git` as a **gitlink** (mode `160000`),
so the parent tracks a pointer rather than the files — and `.gitignore` cannot
change that. So `init` moves the git dir to `.git/forkman/<name>.git` and points
it back at the subdirectory with `core.worktree`. The parent then sees plain
files, while the history survives intact.

That history is what makes syncing cheap: upstream is merged with a **real merge
base**, so your local edits and upstream's are combined properly.

```
forkman sync
  shadow repo: commit local drift, then `git merge upstream/main`
  parent repo: commit the resulting file changes
  you:         git push
```

Local edits reach these files through the parent repo, so the shadow repo's
index is stale by design; each sync commits that drift first — that is the local
side of the merge. On conflict the merge is aborted (the parent's files are left
exactly as they were) and upstream is parked in `fork-sync/<branch>-<date>`
inside the shadow repo. `forkman status` prints the `git --git-dir=… --work-tree=…`
invocation for driving it by hand.

`forkman remove --dir vendor/proj` moves the git dir back to `vendor/proj/.git`,
leaving an ordinary nested clone.

#### Replay mode (`.git` deleted)

Without a `.git` there is no shared history and nothing to merge against.
`init` records the upstream commit your copy corresponds to (`--base REF`,
defaulting to the upstream tip at registration time), and each sync replays
`git diff <base>..<new tip>` onto `vendor/proj/`:

1. Stash uncommitted changes (auto-restored afterward).
2. `git apply --directory=vendor/proj/`; if that fails because of your local
   edits, retry as a 3-way merge so both sides are kept.
3. Commit the result in the parent repo and advance the recorded base.
4. **On conflict**: reset the subdirectory back to how it was, save the patch to
   `.git/forkman/<prefix>-<date>.patch`, and print the command to replay it:
   ```sh
   git apply --3way --directory=vendor/proj/ .git/forkman/vendor-proj-20260807.patch
   ```

The upstream remote is added to the *parent* repo as `upstream-<prefix>` (shadow
mode instead uses the nested clone's own `origin`), so one parent can host
several vendored copies. A repo may also be a fork itself and host vendored
copies; `sync` handles both.

### 4. Inspect

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
  - `fork.sub.<prefix>.{mode,upstream,remote,branch}` — one vendored
    subdirectory each; plus `base` (replay: the upstream commit the copy is
    level with) or `gitdir` (shadow: where the parked history lives)
- **Global** — `~/.config/forkman/registry.json`, a list of fork paths for
  `--all`. Stale entries are pruned automatically.

## Automate

```sh
# nightly: sync all forks, log conflicts
0 3 * * *  forkman sync --all >> ~/.local/state/forkman.log 2>&1
```
