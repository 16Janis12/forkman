package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathMatchesIgnore(t *testing.T) {
	patterns := []string{"docs", "config/*.yaml", "build/", "src/temp.txt"}

	tests := []struct {
		path     string
		expected bool
	}{
		{"docs/readme.md", true},
		{"docs/sub/readme.md", true},
		{"docs", true},
		{"src/app.go", false},
		{"config/app.yaml", true},
		{"config/sub/app.yaml", false},
		{"build/bundle.js", true},
		{"src/temp.txt", true},
		{"src/other.txt", false},
	}

	for _, tc := range tests {
		got := pathMatchesIgnore(tc.path, patterns)
		if got != tc.expected {
			t.Errorf("pathMatchesIgnore(%q) = %v; expected %v", tc.path, got, tc.expected)
		}
	}
}

func TestLoadIgnorePatterns(t *testing.T) {
	tempDir := t.TempDir()
	forkignore := filepath.Join(tempDir, ".forkignore")
	content := "# This is a comment\n\ndocs/\nconfig/*.json\n"
	if err := os.WriteFile(forkignore, []byte(content), 0o644); err != nil {
		t.Fatalf("writing .forkignore: %v", err)
	}

	patterns := loadIgnorePatterns(tempDir, tempDir, "")
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns, got %d: %v", len(patterns), patterns)
	}
	if patterns[0] != "docs" || patterns[1] != "config/*.json" {
		t.Errorf("unexpected patterns: %v", patterns)
	}
}

func TestBuildPathspecExcludes(t *testing.T) {
	patterns := []string{"docs", "build"}
	excludes := buildPathspecExcludes(patterns)
	if len(excludes) != 2 || excludes[0] != ":(exclude)docs" || excludes[1] != ":(exclude)build" {
		t.Errorf("unexpected pathspec excludes: %v", excludes)
	}
}

func TestWholeRepoSyncWithIgnore(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	downstream := filepath.Join(dir, "downstream")

	_ = os.MkdirAll(filepath.Join(upstream, "docs"), 0o755)
	_ = os.MkdirAll(filepath.Join(upstream, "src"), 0o755)

	if _, err := gitRaw(dir, "init", "-b", "main", upstream); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(upstream, "src", "code.txt"), []byte("v1"), 0o644)
	_ = os.WriteFile(filepath.Join(upstream, "docs", "readme.md"), []byte("docs v1"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "initial upstream")

	// Clone downstream
	if _, err := gitRaw(dir, "clone", upstream, downstream); err != nil {
		t.Fatal(err)
	}

	// Register fork and ignore docs
	_ = os.WriteFile(filepath.Join(downstream, ".forkignore"), []byte("docs/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(downstream, "docs", "readme.md"), []byte("custom docs"), 0o644)
	_, _ = gitRaw(downstream, "add", "-A")
	_, _ = gitRaw(downstream, "commit", "-m", "custom local docs")

	if err := gitConfigSet(downstream, "fork.upstream", upstream); err != nil {
		t.Fatal(err)
	}
	if err := gitConfigSet(downstream, "fork.upstreamRemote", "upstream"); err != nil {
		t.Fatal(err)
	}
	if err := gitConfigSet(downstream, "fork.upstreamBranch", "main"); err != nil {
		t.Fatal(err)
	}
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)

	// Upstream makes updates to src and docs
	_ = os.WriteFile(filepath.Join(upstream, "src", "code.txt"), []byte("v2"), 0o644)
	_ = os.WriteFile(filepath.Join(upstream, "docs", "readme.md"), []byte("upstream docs v2"), 0o644)
	_ = os.WriteFile(filepath.Join(upstream, "docs", "extra.txt"), []byte("extra"), 0o644)
	_, _ = gitRaw(upstream, "add", "-A")
	_, _ = gitRaw(upstream, "commit", "-m", "upstream updates")

	// Sync
	res := syncOne(downstream, syncOptions{})
	if res.Status != "merged 1" {
		t.Fatalf("sync failed: status=%s detail=%s", res.Status, res.Detail)
	}

	// Check code updated
	codeBytes, _ := os.ReadFile(filepath.Join(downstream, "src", "code.txt"))
	if strings.TrimSpace(string(codeBytes)) != "v2" {
		t.Errorf("expected code to be 'v2', got %q", string(codeBytes))
	}

	// Check docs stayed custom
	docsBytes, _ := os.ReadFile(filepath.Join(downstream, "docs", "readme.md"))
	if strings.TrimSpace(string(docsBytes)) != "custom docs" {
		t.Errorf("expected docs to stay 'custom docs', got %q", string(docsBytes))
	}

	// Check upstream's extra file in docs was ignored
	if _, err := os.Stat(filepath.Join(downstream, "docs", "extra.txt")); err == nil {
		t.Errorf("docs/extra.txt was added despite docs being ignored")
	}
}
