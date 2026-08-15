package update

import (
	"strconv"
	"strings"
)

// Version comparison, for the one question this package asks: is the release on GitHub newer
// than the binary that is running?
//
// This is deliberately not a semver library. Boks' tags are `vMAJOR.MINOR.PATCH` with an
// optional pre-release suffix, the comparison is used for exactly one purpose, and a
// dependency whose failure mode is "nags the user about an update that does not exist" is not
// worth taking for thirty lines of arithmetic. What is worth having is the tests, which is
// where the ordering rules below are pinned.

// version is a parsed release version. Pre is the pre-release suffix with its leading hyphen
// removed, empty for a final release.
type version struct {
	major, minor, patch int
	pre                 string
}

// parseVersion reads `v1.2.3`, `1.2.3`, or either with a `-rc.1` style suffix.
//
// It reports ok=false for anything it does not fully understand, and every caller treats that
// as "do not say anything". That is the important property: an unparseable version must make
// Boks quiet, never make it guess. `dev` — the default for a local build — lands here.
func parseVersion(s string) (version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return version{}, false
	}

	var v version
	// Build metadata carries no ordering by construction, so it is discarded rather than
	// compared.
	if plus := strings.IndexByte(s, '+'); plus >= 0 {
		s = s[:plus]
	}
	if hyphen := strings.IndexByte(s, '-'); hyphen >= 0 {
		v.pre = s[hyphen+1:]
		s = s[:hyphen]
		if v.pre == "" {
			return version{}, false
		}
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		// A leading `+` or `-` would be accepted by Atoi and is not a version component.
		if p == "" || strings.ContainsAny(p[:1], "+-") {
			return version{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, true
}

// newer reports whether other is a later version than v.
//
// The pre-release rule is semver's, and it is the one that matters here: `1.0.0` is newer than
// `1.0.0-rc.1`, so someone running a release candidate is told when the real release lands.
// Between two pre-releases the suffixes are compared as plain strings, which orders `rc.1`
// before `rc.2` and is the only case Boks' own tags can produce.
func (v version) newer(other version) bool {
	switch {
	case other.major != v.major:
		return other.major > v.major
	case other.minor != v.minor:
		return other.minor > v.minor
	case other.patch != v.patch:
		return other.patch > v.patch
	case other.pre == v.pre:
		return false
	case v.pre == "":
		// v is a final release and other is a pre-release of the same number.
		return false
	case other.pre == "":
		return true
	default:
		return other.pre > v.pre
	}
}

// IsNewer reports whether latest is a release the running current version should be told
// about. Anything unparseable on either side reports false: see parseVersion.
func IsNewer(current, latest string) bool {
	cur, ok := parseVersion(current)
	if !ok {
		return false
	}
	next, ok := parseVersion(latest)
	if !ok {
		return false
	}
	return cur.newer(next)
}
