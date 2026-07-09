package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// registryPath returns $XDG_CONFIG_HOME/forkman/registry.json (or ~/.config/...).
func registryPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "forkman", "registry.json"), nil
}

// loadRegistry reads the registry, pruning paths that no longer exist as git
// repos. Pruned entries are reported to stderr; the pruned list is persisted.
func loadRegistry() ([]string, error) {
	path, err := registryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("registry %s is corrupt: %w", path, err)
	}

	kept := paths[:0:0]
	var pruned []string
	for _, p := range paths {
		if mustExist(p) && isGitRepo(p) {
			kept = append(kept, p)
		} else {
			pruned = append(pruned, p)
		}
	}
	if len(pruned) > 0 {
		for _, p := range pruned {
			fmt.Fprintf(os.Stderr, "warning: pruning stale fork %s\n", p)
		}
		_ = saveRegistry(kept)
	}
	return kept, nil
}

// saveRegistry writes paths (deduped + sorted) to the registry file.
func saveRegistry(paths []string) error {
	path, err := registryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	set := map[string]bool{}
	uniq := make([]string, 0, len(paths))
	for _, p := range paths {
		if !set[p] {
			set[p] = true
			uniq = append(uniq, p)
		}
	}
	sort.Strings(uniq)
	data, err := json.MarshalIndent(uniq, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// registerFork adds repo to the registry (idempotent).
func registerFork(repo string) error {
	paths, err := loadRegistry()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p == repo {
			return nil // already present
		}
	}
	return saveRegistry(append(paths, repo))
}

// unregisterFork removes repo from the registry. Returns whether it was present.
func unregisterFork(repo string) (bool, error) {
	paths, err := loadRegistry()
	if err != nil {
		return false, err
	}
	out := make([]string, 0, len(paths))
	found := false
	for _, p := range paths {
		if p == repo {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return false, nil
	}
	return true, saveRegistry(out)
}
