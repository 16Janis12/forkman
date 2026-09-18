package main

import (
	"testing"
)

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input    string
		valid    bool
		expected semver
	}{
		{"v1.2.3", true, semver{raw: "v1.2.3", major: 1, minor: 2, patch: 3}},
		{"1.2.3", true, semver{raw: "1.2.3", major: 1, minor: 2, patch: 3}},
		{"refs/tags/v2.10.30", true, semver{raw: "v2.10.30", major: 2, minor: 10, patch: 30}},
		{"tags/0.0.1", true, semver{raw: "0.0.1", major: 0, minor: 0, patch: 1}},
		{"invalid", false, semver{}},
		{"v1.2", false, semver{}},
		{"1.2", false, semver{}},
		{"", false, semver{}},
	}

	for _, tc := range tests {
		got, ok := parseSemver(tc.input)
		if ok != tc.valid {
			t.Errorf("parseSemver(%q) ok = %v; expected %v", tc.input, ok, tc.valid)
			continue
		}
		if ok && got != tc.expected {
			t.Errorf("parseSemver(%q) = %+v; expected %+v", tc.input, got, tc.expected)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int // -1 for a < b, 1 for a > b, 0 for equal
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.1.0", "v1.2.0", -1},
		{"v1.9.0", "v1.10.0", -1}, // Critical: numerical vs lexicographical!
		{"v1.10.0", "v1.9.0", 1},
		{"v0.9.0", "v0.10.0", -1},
		{"v0.10.0", "v0.9.0", 1},
		{"v1.99.99", "v2.0.0", -1},
		{"v2.0.0", "v1.99.99", 1},
		{"0.1.0", "0.2.0", -1},
		{"1.0.0", "v1.0.0", -1}, // raw tiebreaker
	}

	for _, tc := range tests {
		svA, okA := parseSemver(tc.a)
		svB, okB := parseSemver(tc.b)
		if !okA || !okB {
			t.Fatalf("failed to parse %q or %q", tc.a, tc.b)
		}
		res := compareSemver(svA, svB)
		if res != tc.expected {
			t.Errorf("compareSemver(%q, %q) = %d; expected %d", tc.a, tc.b, res, tc.expected)
		}
	}
}

func TestSemverUpgradeType(t *testing.T) {
	tests := []struct {
		oldTag   string
		newTag   string
		expected string
	}{
		{"v1.0.0", "v2.0.0", "MAJOR"},
		{"v1.2.3", "v2.0.0", "MAJOR"},
		{"v0.9.0", "v1.0.0", "MAJOR"},
		{"1.0.0", "2.1.0", "MAJOR"},
		{"v1.2.0", "v1.3.0", "MINOR"},
		{"v1.2.3", "v1.3.0", "MINOR"},
		{"v0.9.0", "v0.10.0", "MINOR"},
		{"v1.2.3", "v1.2.4", "PATCH"},
		{"v0.9.1", "v0.9.2", "PATCH"},
		{"v1.0.0", "v1.0.0", ""},      // same version
		{"v2.0.0", "v1.9.0", ""},      // downgrade / older
		{"", "v2.0.0", "MAJOR"},       // infer from initial tag
		{"", "v0.3.0", "MINOR"},       // infer from initial tag
		{"", "v0.0.5", "PATCH"},       // infer from initial tag
		{"non-semver", "v2.0.0", "MAJOR"},
		{"v1.0.0", "non-semver", ""},
	}

	for _, tc := range tests {
		got := semverUpgradeType(tc.oldTag, tc.newTag)
		if got != tc.expected {
			t.Errorf("semverUpgradeType(%q, %q) = %q; expected %q", tc.oldTag, tc.newTag, got, tc.expected)
		}
	}
}

func TestSelectLatestSemanticTag(t *testing.T) {
	tags := []string{
		"v0.1.0",
		"0.2.0",
		"v0.10.0",
		"v0.9.0",
		"v1.0.0",
		"v1.10.2",
		"v1.2.9",
		"non-semver",
		"nightly-2026",
		"v2.0.0",
		"v3.0", // not 3 parts
	}

	latest, err := selectLatestSemanticTag(tags, "", "upstream")
	if err != nil {
		t.Fatalf("selectLatestSemanticTag failed: %v", err)
	}
	if latest != "v2.0.0" {
		t.Fatalf("expected latest tag 'v2.0.0', got %q", latest)
	}

	// Test with custom pattern
	customTags := []string{"release-1.0.0", "release-1.1.0", "v2.0.0"}
	latestCustom, err := selectLatestSemanticTag(customTags, `release-[0-9]+\.[0-9]+\.[0-9]+`, "upstream")
	if err != nil {
		t.Fatalf("custom selectLatestSemanticTag failed: %v", err)
	}
	if latestCustom != "release-1.1.0" {
		t.Fatalf("expected 'release-1.1.0', got %q", latestCustom)
	}

	// Test when no tags match
	_, err = selectLatestSemanticTag([]string{"foo", "bar"}, "", "upstream")
	if err == nil {
		t.Fatalf("expected error when no tags match pattern, got nil")
	}
}

func TestNormalizeMode(t *testing.T) {
	validLatest := []string{"latest", "latest-rev", "latest_rev", "LATEST", "rev", "latest rev"}
	for _, in := range validLatest {
		got, err := normalizeMode(in)
		if err != nil || got != modeLatestRev {
			t.Errorf("normalizeMode(%q) = (%q, %v); expected %q", in, got, err, modeLatestRev)
		}
	}

	validTags := []string{"tags", "tag", "semantic-tags", "semantic_tags", "SEMANTIC TAGS", "semver"}
	for _, in := range validTags {
		got, err := normalizeMode(in)
		if err != nil || got != modeSemanticTags {
			t.Errorf("normalizeMode(%q) = (%q, %v); expected %q", in, got, err, modeSemanticTags)
		}
	}

	invalid := []string{"foo", "master", "branch", "v1"}
	for _, in := range invalid {
		_, err := normalizeMode(in)
		if err == nil {
			t.Errorf("normalizeMode(%q) succeeded; expected error", in)
		}
	}
}
