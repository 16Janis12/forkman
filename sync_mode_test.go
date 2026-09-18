package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncSemanticTagsMode(t *testing.T) {
	tempDir := t.TempDir()
	upstream := filepath.Join(tempDir, "upstream")
	downstream := filepath.Join(tempDir, "downstream")

	_ = os.MkdirAll(upstream, 0o755)
	if _, err := gitRaw(tempDir, "init", "-b", "main", upstream); err != nil {
		t.Fatal(err)
	}
	_, _ = gitRaw(upstream, "config", "user.name", "Test")
	_, _ = gitRaw(upstream, "config", "user.email", "test@example.com")
	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("v0.9.0 content"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "v0.9.0 commit")
	_, _ = gitRaw(upstream, "tag", "v0.9.0")

	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("v0.10.0 content"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "v0.10.0 commit")
	_, _ = gitRaw(upstream, "tag", "v0.10.0") // higher than v0.9.0 numerically (minor bump)!

	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("v0.2.0 content"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "v0.2.0 commit")
	_, _ = gitRaw(upstream, "tag", "v0.2.0")

	// Add an unanchored / non-semantic tag
	_, _ = gitRaw(upstream, "tag", "nightly-build")

	// Clone downstream
	if _, err := gitRaw(tempDir, "clone", "-b", "main", upstream, downstream); err != nil {
		t.Fatal(err)
	}
	_, _ = gitRaw(downstream, "config", "user.name", "Test")
	_, _ = gitRaw(downstream, "config", "user.email", "test@example.com")

	// Reset downstream to v0.9.0
	_, _ = gitRaw(downstream, "reset", "--hard", "v0.9.0")
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)

	_ = gitConfigSet(downstream, "fork.upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstreamRemote", "upstream")
	_ = gitConfigSet(downstream, "fork.syncMode", modeSemanticTags)
	_ = gitConfigSet(downstream, "fork.upstreamTag", "v0.9.0")

	// Run sync in semantic-tags mode: v0.9.0 -> v0.10.0 is MINOR upgrade
	res := syncOne(downstream, syncOptions{Mode: modeSemanticTags})
	if !strings.HasPrefix(res.Status, "merged") {
		t.Fatalf("expected status 'merged ...', got %q (%s)", res.Status, res.Detail)
	}
	if res.Tag != "v0.10.0" {
		t.Fatalf("expected tag 'v0.10.0', got %q", res.Tag)
	}
	if res.UpgradeType != "MINOR" {
		t.Fatalf("expected upgrade type 'MINOR', got %q", res.UpgradeType)
	}

	contentBytes, _ := os.ReadFile(filepath.Join(downstream, "README.md"))
	content := string(contentBytes)
	if content != "v0.10.0 content" {
		t.Fatalf("expected downstream content 'v0.10.0 content', got %q", content)
	}

	// Verify tracked tag config was updated
	trackedTag, _ := gitConfigGet(downstream, "fork.upstreamTag")
	if trackedTag != "v0.10.0" {
		t.Fatalf("expected fork.upstreamTag to be 'v0.10.0', got %q", trackedTag)
	}

	// Now add v1.0.0 to upstream: v0.10.0 -> v1.0.0 is MAJOR upgrade
	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("v1.0.0 content"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "v1.0.0 commit")
	_, _ = gitRaw(upstream, "tag", "v1.0.0")

	// Sync again in semantic-tags mode
	res2 := syncOne(downstream, syncOptions{Mode: modeSemanticTags})
	if !strings.HasPrefix(res2.Status, "merged") {
		t.Fatalf("expected status 'merged ...', got %q (%s)", res2.Status, res2.Detail)
	}
	if res2.Tag != "v1.0.0" {
		t.Fatalf("expected tag 'v1.0.0', got %q", res2.Tag)
	}
	if res2.UpgradeType != "MAJOR" {
		t.Fatalf("expected upgrade type 'MAJOR', got %q", res2.UpgradeType)
	}

	content2Bytes, _ := os.ReadFile(filepath.Join(downstream, "README.md"))
	content2 := string(content2Bytes)
	if content2 != "v1.0.0 content" {
		t.Fatalf("expected downstream content 'v1.0.0 content', got %q", content2)
	}

	// Now add v1.0.1 to upstream: v1.0.0 -> v1.0.1 is PATCH upgrade
	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("v1.0.1 content"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "v1.0.1 commit")
	_, _ = gitRaw(upstream, "tag", "v1.0.1")

	res3 := syncOne(downstream, syncOptions{Mode: modeSemanticTags})
	if !strings.HasPrefix(res3.Status, "merged") {
		t.Fatalf("expected status 'merged ...', got %q (%s)", res3.Status, res3.Detail)
	}
	if res3.Tag != "v1.0.1" {
		t.Fatalf("expected tag 'v1.0.1', got %q", res3.Tag)
	}
	if res3.UpgradeType != "PATCH" {
		t.Fatalf("expected upgrade type 'PATCH', got %q", res3.UpgradeType)
	}
}

func TestSyncLatestRevMode(t *testing.T) {
	tempDir := t.TempDir()
	upstream := filepath.Join(tempDir, "upstream")
	downstream := filepath.Join(tempDir, "downstream")

	_ = os.MkdirAll(upstream, 0o755)
	if _, err := gitRaw(tempDir, "init", "-b", "main", upstream); err != nil {
		t.Fatal(err)
	}
	_, _ = gitRaw(upstream, "config", "user.name", "Test")
	_, _ = gitRaw(upstream, "config", "user.email", "test@example.com")
	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("initial upstream"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "initial commit")
	_, _ = gitRaw(upstream, "tag", "v1.0.0")

	// Commit on branch after tag
	_ = os.WriteFile(filepath.Join(upstream, "README.md"), []byte("latest branch revision"), 0o644)
	_, _ = gitRaw(upstream, "add", ".")
	_, _ = gitRaw(upstream, "commit", "-m", "post-tag commit on main")

	// Clone downstream at v1.0.0
	if _, err := gitRaw(tempDir, "clone", "-b", "main", upstream, downstream); err != nil {
		t.Fatal(err)
	}
	_, _ = gitRaw(downstream, "config", "user.name", "Test")
	_, _ = gitRaw(downstream, "config", "user.email", "test@example.com")
	_, _ = gitRaw(downstream, "reset", "--hard", "v1.0.0")
	_, _ = gitRaw(downstream, "remote", "add", "upstream", upstream)

	// Configure downstream with a pinned tag v1.0.0
	_ = gitConfigSet(downstream, "fork.upstream", upstream)
	_ = gitConfigSet(downstream, "fork.upstreamRemote", "upstream")
	_ = gitConfigSet(downstream, "fork.upstreamTag", "v1.0.0")

	// Sync with latest-rev mode: should ignore pinned tag and sync the tip of the branch
	res := syncOne(downstream, syncOptions{Mode: modeLatestRev})
	if !strings.HasPrefix(res.Status, "merged") {
		t.Fatalf("expected status 'merged ...', got %q (%s)", res.Status, res.Detail)
	}

	contentBytes, _ := os.ReadFile(filepath.Join(downstream, "README.md"))
	content := string(contentBytes)
	if content != "latest branch revision" {
		t.Fatalf("expected content 'latest branch revision', got %q", content)
	}
}

func TestGitHubActionOutputsAndSummary(t *testing.T) {
	tempDir := t.TempDir()

	ghOutputFile := filepath.Join(tempDir, "action_output.txt")
	ghSummaryFile := filepath.Join(tempDir, "action_summary.md")

	t.Setenv("GITHUB_OUTPUT", ghOutputFile)
	t.Setenv("GITHUB_STEP_SUMMARY", ghSummaryFile)

	results := []syncResult{
		{
			Repo:        "/path/to/repo",
			Status:      "merged 2",
			Detail:      "from tags/v2.0.0 (MAJOR upgrade: v1.9.0 -> v2.0.0)",
			Tag:         "v2.0.0",
			PrevTag:     "v1.9.0",
			UpgradeType: "MAJOR",
		},
	}

	opt := syncOptions{
		Mode: modeSemanticTags,
		Tag:  "v2.0.0",
	}

	writeGitHubOutput(results)
	writeGitHubSummary(results, opt)

	outBytes, err := os.ReadFile(ghOutputFile)
	if err != nil {
		t.Fatalf("failed to read GITHUB_OUTPUT: %v", err)
	}
	outStr := string(outBytes)
	if !strings.Contains(outStr, "status=merged 2") {
		t.Errorf("GITHUB_OUTPUT missing status: %s", outStr)
	}
	if !strings.Contains(outStr, "tag=v2.0.0") {
		t.Errorf("GITHUB_OUTPUT missing tag: %s", outStr)
	}
	if !strings.Contains(outStr, "upgrade_type=MAJOR") {
		t.Errorf("GITHUB_OUTPUT missing upgrade_type: %s", outStr)
	}
	if !strings.Contains(outStr, "upgrade-type=MAJOR") {
		t.Errorf("GITHUB_OUTPUT missing upgrade-type: %s", outStr)
	}
	if !strings.Contains(outStr, "synced=true") {
		t.Errorf("GITHUB_OUTPUT missing synced=true: %s", outStr)
	}

	sumBytes, err := os.ReadFile(ghSummaryFile)
	if err != nil {
		t.Fatalf("failed to read GITHUB_STEP_SUMMARY: %v", err)
	}
	sumStr := string(sumBytes)
	if !strings.Contains(sumStr, "### Forkman Sync Summary") {
		t.Errorf("GITHUB_STEP_SUMMARY missing header: %s", sumStr)
	}
	if !strings.Contains(sumStr, "v2.0.0") {
		t.Errorf("GITHUB_STEP_SUMMARY missing tag: %s", sumStr)
	}
	if !strings.Contains(sumStr, "**MAJOR**") {
		t.Errorf("GITHUB_STEP_SUMMARY missing upgrade badge: %s", sumStr)
	}
	if !strings.Contains(sumStr, "merged 2") {
		t.Errorf("GITHUB_STEP_SUMMARY missing status: %s", sumStr)
	}
}
