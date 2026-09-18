package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const defaultTagPattern = `v?[0-9]+\.[0-9]+\.[0-9]+`

const (
	modeLatestRev    = "latest-rev"
	modeSemanticTags = "semantic-tags"
)

type semver struct {
	raw   string
	major int
	minor int
	patch int
}

var semverExtractRe = regexp.MustCompile(`v?([0-9]+)\.([0-9]+)\.([0-9]+)`)

func parseSemver(tag string) (semver, bool) {
	tag = cleanTag(tag)
	m := semverExtractRe.FindStringSubmatch(tag)
	if len(m) < 4 {
		return semver{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	patch, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return semver{}, false
	}
	return semver{raw: tag, major: major, minor: minor, patch: patch}, true
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return -1
		}
		return 1
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return -1
		}
		return 1
	}
	if a.raw < b.raw {
		return -1
	} else if a.raw > b.raw {
		return 1
	}
	return 0
}

// semverUpgradeType compares oldTag with newTag and returns "MAJOR", "MINOR", "PATCH", or "".
// If oldTag is empty or non-semver, it infers the highest non-zero component of newTag.
func semverUpgradeType(oldTag, newTag string) string {
	newVer, okNew := parseSemver(newTag)
	if !okNew {
		return ""
	}
	oldVer, okOld := parseSemver(oldTag)
	if !okOld {
		if newVer.major > 0 {
			return "MAJOR"
		}
		if newVer.minor > 0 {
			return "MINOR"
		}
		if newVer.patch > 0 {
			return "PATCH"
		}
		return ""
	}
	if newVer.major > oldVer.major {
		return "MAJOR"
	}
	if newVer.major == oldVer.major && newVer.minor > oldVer.minor {
		return "MINOR"
	}
	if newVer.major == oldVer.major && newVer.minor == oldVer.minor && newVer.patch > oldVer.patch {
		return "PATCH"
	}
	return ""
}

func normalizeMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	m = strings.ReplaceAll(m, "_", "-")
	m = strings.ReplaceAll(m, " ", "-")
	switch m {
	case "", "default":
		return "", nil
	case "latest", "latest-rev", "rev":
		return modeLatestRev, nil
	case "tags", "tag", "semantic-tags", "semver":
		return modeSemanticTags, nil
	default:
		return "", fmt.Errorf("invalid mode %q: expected 'latest' (or 'latest-rev') or 'tags' (or 'semantic-tags')", mode)
	}
}

func compileTagRegex(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		pattern = defaultTagPattern
	}
	anchored := pattern
	if !strings.HasPrefix(anchored, "^") {
		anchored = "^" + anchored
	}
	if !strings.HasSuffix(anchored, "$") {
		anchored = anchored + "$"
	}
	return regexp.Compile(anchored)
}

func selectLatestSemanticTag(tags []string, pattern, remote string) (string, error) {
	re, err := compileTagRegex(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid tag pattern %q: %w", pattern, err)
	}

	var matching []semver
	for _, t := range tags {
		t = cleanTag(t)
		if re.MatchString(t) {
			if sv, ok := parseSemver(t); ok {
				matching = append(matching, sv)
			}
		}
	}

	if len(matching) == 0 {
		pat := pattern
		if pat == "" {
			pat = defaultTagPattern
		}
		return "", fmt.Errorf("no semantic tags matching %q found on %s", pat, remote)
	}

	sort.Slice(matching, func(i, j int) bool {
		return compareSemver(matching[i], matching[j]) < 0
	})

	return matching[len(matching)-1].raw, nil
}

func listRemoteTags(repoDir, remote string) ([]string, error) {
	out, err := gitRaw(repoDir, "ls-remote", "--tags", remote)
	if err != nil {
		// Fallback: check local tags
		tagOut, tagErr := gitRaw(repoDir, "tag", "-l")
		if tagErr == nil && strings.TrimSpace(tagOut) != "" {
			var tags []string
			for _, line := range strings.Split(tagOut, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					tags = append(tags, line)
				}
			}
			return tags, nil
		}
		return nil, fmt.Errorf("listing tags on %s: %w", remote, err)
	}

	var tags []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		ref := parts[1]
		if strings.HasSuffix(ref, "^{}") {
			continue
		}
		if strings.HasPrefix(ref, "refs/tags/") {
			tag := strings.TrimPrefix(ref, "refs/tags/")
			if !seen[tag] {
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
	}
	return tags, nil
}

func findLatestSemanticTag(repoDir, remote, pattern string) (string, error) {
	tags, err := listRemoteTags(repoDir, remote)
	if err != nil {
		return "", err
	}
	return selectLatestSemanticTag(tags, pattern, remote)
}

func findLatestSemanticTagShadow(repoDir string, sf subFork, pattern string) (string, error) {
	out, err := gitShadowRaw(repoDir, sf, "ls-remote", "--tags", sf.Remote)
	if err != nil {
		// Fallback: check local tags in shadow repo
		tagOut, tagErr := gitShadowRaw(repoDir, sf, "tag", "-l")
		if tagErr == nil && strings.TrimSpace(tagOut) != "" {
			var tags []string
			for _, line := range strings.Split(tagOut, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					tags = append(tags, line)
				}
			}
			return selectLatestSemanticTag(tags, pattern, sf.Remote)
		}
		return "", fmt.Errorf("listing tags on %s: %w", sf.Remote, err)
	}

	var tags []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		ref := parts[1]
		if strings.HasSuffix(ref, "^{}") {
			continue
		}
		if strings.HasPrefix(ref, "refs/tags/") {
			tag := strings.TrimPrefix(ref, "refs/tags/")
			if !seen[tag] {
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
	}
	return selectLatestSemanticTag(tags, pattern, sf.Remote)
}
