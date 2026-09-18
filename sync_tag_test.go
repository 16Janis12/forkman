package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanTag(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"v1.0.0", "v1.0.0"},
		{"tags/v1.0.0", "v1.0.0"},
		{"refs/tags/v1.0.0", "v1.0.0"},
		{"  v1.0.0  ", "v1.0.0"},
		{"  refs/tags/v2.3.4  ", "v2.3.4"},
		{"", ""},
	}

	for _, tc := range tests {
		got := cleanTag(tc.input)
		if got != tc.expected {
			t.Errorf("cleanTag(%q) = %q; expected %q", tc.input, got, tc.expected)
		}
	}
}

func TestSyncToSpecificTagWholeRepo(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	downstream := filepath.Join(dir, "downstream")

	// 1. Initialize upstream with v1.0.0 and v2.0.0 and an untagged commit on main
	_ = os.MkdirAll(upstream, 0o755)
	if _, err := gitRaw(dir, "init", "-b", "main", upstream); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "commit v1")
	_, _ = gitRaw(upstream, "tag", "-a", "v1.0.0", "-m", "release v1.0.0")

	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v2"), 0o644)
	_ = os.WriteFile(filepath.Join(upstream, "feature_v2.txt"), []byte("feature v2"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "commit v2")
	_, _ = gitRaw(upstream, "tag", "-a", "v2.0.0", "-m", "release v2.0.0")

	// Commit 3 on main after v2.0.0
	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v3-unreleased"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "commit v3 unreleased")

	// 2. Clone downstream at v1.0.0
	if _, err := gitRaw(dir, "clone", "-b", "main", upstream, downstream); err != nil {
		t.Fatal(err)
	}
	_, _ = gitRaw(downstream, "reset", "--hard", "v1.0.0")
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)

	// Configure downstream fork
	_ = gitConfigSet(downstream, "fork.upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstreamRemote", "upstream")
	_ = gitConfigSet(downstream, "fork.upstreamBranch", "main")

	// Add local custom commit in downstream
	_ = os.WriteFile(filepath.Join(downstream, "local.txt"), []byte("local work"), 0o644)
	_, _ = gitRaw(downstream, "add", "-A")
	_, _ = gitRaw(downstream, "commit", "-m", "my local changes")

	// 3. Sync to specific tag v2.0.0
	res := syncOne(downstream, syncOptions{Tag: "v2.0.0"})
	if res.Status != "merged 1" {
		t.Fatalf("expected status 'merged 1', got %q (%s)", res.Status, res.Detail)
	}

	// 4. Verify v2.0.0 content is present, but v3 unreleased commit is NOT present
	fContent, _ := os.ReadFile(filepath.Join(downstream, "file.txt"))
	if strings.TrimSpace(string(fContent)) != "v2" {
		t.Errorf("expected file.txt to be 'v2', got %q", string(fContent))
	}
	featContent, _ := os.ReadFile(filepath.Join(downstream, "feature_v2.txt"))
	if strings.TrimSpace(string(featContent)) != "feature v2" {
		t.Errorf("expected feature_v2.txt to exist with 'feature v2', got %q", string(featContent))
	}
	localContent, _ := os.ReadFile(filepath.Join(downstream, "local.txt"))
	if strings.TrimSpace(string(localContent)) != "local work" {
		t.Errorf("expected local.txt to remain untouched, got %q", string(localContent))
	}

	// 5. Subsequent sync to same tag should report up-to-date
	res2 := syncOne(downstream, syncOptions{Tag: "v2.0.0"})
	if res2.Status != "up-to-date" {
		t.Errorf("expected status 'up-to-date', got %q (%s)", res2.Status, res2.Detail)
	}
}

func TestSyncToConfiguredTag(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	downstream := filepath.Join(dir, "downstream")

	_ = os.MkdirAll(upstream, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", upstream)
	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "v1")
	_, _ = gitRaw(upstream, "tag", "v1.0.0")

	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v2"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "v2")
	_, _ = gitRaw(upstream, "tag", "v2.0.0")

	// Clone downstream
	_, _ = gitRaw(dir, "clone", "-b", "main", upstream, downstream)
	_, _ = gitRaw(downstream, "reset", "--hard", "v1.0.0")
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)

	_ = gitConfigSet(downstream, "fork.upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstreamRemote", "upstream")
	_ = gitConfigSet(downstream, "fork.upstreamTag", "v2.0.0")

	cfg, err := loadForkConfig(downstream)
	if err != nil {
		t.Fatalf("loadForkConfig: %v", err)
	}
	if cfg.Tag != "v2.0.0" {
		t.Fatalf("expected cfg.Tag to be 'v2.0.0', got %q", cfg.Tag)
	}

	// Sync without passing --tag flag (should use configured fork.upstreamTag)
	res := syncOne(downstream, syncOptions{})
	if res.Status != "merged 1" {
		t.Fatalf("expected status 'merged 1', got %q (%s)", res.Status, res.Detail)
	}

	fContent, _ := os.ReadFile(filepath.Join(downstream, "file.txt"))
	if strings.TrimSpace(string(fContent)) != "v2" {
		t.Errorf("expected file.txt to be 'v2', got %q", string(fContent))
	}
}

func TestSyncNonexistentTag(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	downstream := filepath.Join(dir, "downstream")

	_ = os.MkdirAll(upstream, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", upstream)
	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "v1")

	_, _ = gitRaw(dir, "clone", "-b", "main", upstream, downstream)
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstreamRemote", "upstream")

	res := syncOne(downstream, syncOptions{Tag: "v9.9.9"})
	if res.Status != "error" {
		t.Fatalf("expected status 'error', got %q", res.Status)
	}
	if !strings.Contains(res.Detail, "fetch failed") {
		t.Errorf("expected fetch failed error detail, got %q", res.Detail)
	}
}

func TestSyncTagConflict(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	downstream := filepath.Join(dir, "downstream")

	_ = os.MkdirAll(upstream, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", upstream)
	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "v1")
	_, _ = gitRaw(upstream, "tag", "v1.0.0")

	_ = os.WriteFile(filepath.Join(upstream, "file.txt"), []byte("upstream conflicting content"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "v2")
	_, _ = gitRaw(upstream, "tag", "v2.0.0")

	// Clone downstream at v1.0.0
	_, _ = gitRaw(dir, "clone", "-b", "main", upstream, downstream)
	_, _ = gitRaw(downstream, "reset", "--hard", "v1.0.0")
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstreamRemote", "upstream")

	// Create local conflicting edit
	_ = os.WriteFile(filepath.Join(downstream, "file.txt"), []byte("downstream conflicting content"), 0o644)
	_, _ = gitRaw(downstream, "add", "-A")
	_, _ = gitRaw(downstream, "commit", "-m", "local conflict commit")

	res := syncOne(downstream, syncOptions{Tag: "v2.0.0"})
	if res.Status != "CONFLICT" {
		t.Fatalf("expected status 'CONFLICT', got %q (%s)", res.Status, res.Detail)
	}

	// Downstream worktree should be clean and file should keep downstream content
	dirty, err := isDirty(downstream)
	if err != nil || dirty {
		t.Errorf("expected clean working tree after aborted conflict, got dirty=%v, err=%v", dirty, err)
	}
	fContent, _ := os.ReadFile(filepath.Join(downstream, "file.txt"))
	if strings.TrimSpace(string(fContent)) != "downstream conflicting content" {
		t.Errorf("expected downstream content preserved, got %q", string(fContent))
	}

	// Conflict branch should be parked
	branches := syncBranches(downstream)
	if len(branches) == 0 {
		t.Fatalf("expected parked conflict branch, found none")
	}
}

func TestSyncVendoredReplayToTag(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	parent := filepath.Join(dir, "parent")

	_ = os.MkdirAll(upstream, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", upstream)
	_ = os.WriteFile(filepath.Join(upstream, "sub.txt"), []byte("sub v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "sub v1")
	_, _ = gitRaw(upstream, "tag", "v1.0.0")

	_ = os.WriteFile(filepath.Join(upstream, "sub.txt"), []byte("sub v2"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "sub v2")
	_, _ = gitRaw(upstream, "tag", "v2.0.0")

	// Setup parent repo with vendored copy
	subDir := filepath.Join(parent, "vendor", "lib")
	_ = os.MkdirAll(subDir, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", parent)
	_ = os.WriteFile(filepath.Join(subDir, "sub.txt"), []byte("sub v1"), 0o644)
	_, _ = gitRaw(parent, "add", "-A")
	_, _ = gitRaw(parent, "commit", "-m", "initial parent")

	// Init replay sub-fork tracking tag v1.0.0
	err := initSub(parent, subDir, upstream, "upstream-lib", "", "v1.0.0", "", "")
	if err != nil {
		t.Fatalf("initSub failed: %v", err)
	}

	sf, err := loadSubFork(parent, "vendor/lib")
	if err != nil {
		t.Fatalf("loadSubFork failed: %v", err)
	}
	if sf.Tag != "v1.0.0" {
		t.Fatalf("expected sf.Tag 'v1.0.0', got %q", sf.Tag)
	}

	// Sync to tag v2.0.0
	res := syncReplay(parent, sf, syncOptions{Tag: "v2.0.0"})
	if !strings.HasPrefix(res.Status, "merged") {
		t.Fatalf("expected merged status, got %q (%s)", res.Status, res.Detail)
	}

	subContent, _ := os.ReadFile(filepath.Join(subDir, "sub.txt"))
	if strings.TrimSpace(string(subContent)) != "sub v2" {
		t.Errorf("expected 'sub v2', got %q", string(subContent))
	}
}

func TestSyncVendoredShadowToTag(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	parent := filepath.Join(dir, "parent")

	_ = os.MkdirAll(upstream, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", upstream)
	_ = os.WriteFile(filepath.Join(upstream, "sub.txt"), []byte("shadow v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "shadow v1")
	_, _ = gitRaw(upstream, "tag", "v1.0.0")

	_ = os.WriteFile(filepath.Join(upstream, "sub.txt"), []byte("shadow v2"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "shadow v2")
	_, _ = gitRaw(upstream, "tag", "v2.0.0")

	// Setup parent repo
	_ = os.MkdirAll(parent, 0o755)
	_, _ = gitRaw(dir, "init", "-b", "main", parent)
	_ = os.WriteFile(filepath.Join(parent, "root.txt"), []byte("parent"), 0o644)
	_, _ = gitRaw(parent, "add", "-A")
	_, _ = gitRaw(parent, "commit", "-m", "initial parent")

	// Clone nested repo inside parent
	nested := filepath.Join(parent, "vendor", "shadowlib")
	_, _ = gitRaw(dir, "clone", "-b", "main", upstream, nested)
	_, _ = gitRaw(nested, "reset", "--hard", "v1.0.0")

	// Init shadow mode sub-fork
	err := initSub(parent, nested, upstream, "origin", "", "v1.0.0", "", "")
	if err != nil {
		t.Fatalf("initShadow failed: %v", err)
	}

	sf, err := loadSubFork(parent, "vendor/shadowlib")
	if err != nil {
		t.Fatalf("loadSubFork failed: %v", err)
	}
	if sf.Mode != modeShadow {
		t.Fatalf("expected modeShadow, got %q", sf.Mode)
	}
	if sf.Tag != "v1.0.0" {
		t.Fatalf("expected Tag 'v1.0.0', got %q", sf.Tag)
	}

	// Sync shadow to v2.0.0
	res := syncShadow(parent, sf, syncOptions{Tag: "v2.0.0"})
	if !strings.HasPrefix(res.Status, "merged") {
		t.Fatalf("expected merged status, got %q (%s)", res.Status, res.Detail)
	}

	shadowContent, _ := os.ReadFile(filepath.Join(nested, "sub.txt"))
	if strings.TrimSpace(string(shadowContent)) != "shadow v2" {
		t.Errorf("expected 'shadow v2', got %q", string(shadowContent))
	}
}
