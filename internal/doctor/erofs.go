package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// erofsMinimum is the oldest erofs-utils containerd's EROFS snapshotter works with.
//
// Below it, the tool exists and runs, so nothing in doctor's old presence check noticed —
// the failure surfaced later, while unpacking an image. Distributions ship older: Ubuntu
// 24.04 LTS is on 1.7.1. docs/troubleshooting.md has carried this number for a while; this
// check is what stops it from being something the user has to read a document to learn.
var erofsMinimum = toolVersion{major: 1, minor: 8}

// toolVersion is a major.minor version, which is all any minimum here is expressed in.
type toolVersion struct {
	major, minor int
}

func (v toolVersion) String() string { return fmt.Sprintf("%d.%d", v.major, v.minor) }

// olderThan compares by major then minor. Patch releases are not compared because no
// requirement is stated in terms of one.
func (v toolVersion) olderThan(other toolVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	return v.minor < other.minor
}

// versionProbe runs a tool to ask its version and returns everything it printed.
//
// It is a parameter of the check rather than a direct exec call so that tests can hand it
// the outputs that matter — an ancient version, a format nobody has seen, a tool that fails
// to run — without needing that erofs-utils installed on the machine running them.
type versionProbe func(ctx context.Context, path string, args ...string) (string, error)

// runVersionProbe is the real thing. Both streams are captured: which one a tool prints its
// version on is not something to rely on.
func runVersionProbe(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return string(out), err
}

// parseToolVersion extracts a version from the first line a tool prints.
//
// erofs-utils has been observed to print both spellings:
//
//	mkfs.erofs (erofs-utils) 1.9        # Debian/Ubuntu package, run on this machine
//	mkfs.erofs (erofs-utils) v1.9.1     # tagged builds print a leading v
//
// followed by a line listing the available compressors, which is ignored. The fields of the
// first line are read from the right, and the first one that begins with an optional "v" and
// a major.minor pair wins — that is where every observed format puts it, and reading from the
// right keeps the tool's own name (mkfs.erofs) out of the way.
//
// Anything else returns ok=false. The caller must not turn that into a verdict about the
// host: an unrecognised version string says something about the parser, not about the tool.
func parseToolVersion(out string) (version toolVersion, text string, ok bool) {
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(strings.TrimSpace(line))
	for i := len(fields) - 1; i >= 0; i-- {
		if v, text, ok := parseVersionField(fields[i]); ok {
			return v, text, true
		}
	}
	return toolVersion{}, "", false
}

// parseVersionField parses one whitespace-separated field as a version, requiring at least
// major.minor so that a bare number elsewhere on the line cannot be mistaken for one. A
// suffix such as -rc1 or a git description is kept in the reported text and ignored for
// comparison.
func parseVersionField(field string) (version toolVersion, text string, ok bool) {
	text = strings.Trim(field, "()[],;")
	digits := strings.TrimPrefix(text, "v")

	major, rest, found := strings.Cut(digits, ".")
	if !found {
		return toolVersion{}, "", false
	}
	// The minor component ends at the next separator, whatever it is: 1.9.1, 1.9-rc1,
	// 1.9+git0 and 1.9~beta all carry a usable 9.
	minor := rest
	if i := strings.IndexFunc(minor, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minor = minor[:i]
	}
	majorN, err := strconv.Atoi(major)
	if err != nil || minor == "" {
		return toolVersion{}, "", false
	}
	minorN, err := strconv.Atoi(minor)
	if err != nil {
		return toolVersion{}, "", false
	}
	return toolVersion{major: majorN, minor: minorN}, text, true
}

// erofsVersionResult grades the mkfs.erofs at path against the minimum the snapshotter needs.
//
// Only a version that was both parsed and found wanting is a failure. A tool that could not
// be run, or that printed something this parser does not recognise, is a warning: the binary
// is installed and may well be new enough, and declaring a working host broken over
// unfamiliar output would be a worse bug than the gap this check closes.
func erofsVersionResult(ctx context.Context, path string, probe versionProbe) Result {
	out, err := probe(ctx, path, "-V")
	if err != nil && strings.TrimSpace(out) == "" {
		return Result{
			Status: StatusWarn,
			Detail: "could not read the mkfs.erofs version",
			Remedy: fmt.Sprintf("Running '%s -V' failed: %v\n"+
				"The erofs snapshotter needs erofs-utils >= %s, and Boks could not confirm\n"+
				"which version is installed. If image pulls fail while unpacking, check it\n"+
				"by hand.", path, err, erofsMinimum),
		}
	}

	version, text, ok := parseToolVersion(out)
	if !ok {
		return Result{
			Status: StatusWarn,
			Detail: "unrecognised mkfs.erofs version",
			Remedy: fmt.Sprintf("'%s -V' printed\n  %q\n"+
				"which Boks could not read as a version. The erofs snapshotter needs\n"+
				"erofs-utils >= %s; check by hand that this one is new enough.",
				path, firstLine(out), erofsMinimum),
		}
	}
	if version.olderThan(erofsMinimum) {
		return Result{
			Status: StatusFail,
			Detail: fmt.Sprintf("mkfs.erofs %s is older than %s", text, erofsMinimum),
			Remedy: fmt.Sprintf("%s is erofs-utils %s.\n"+
				"containerd's erofs snapshotter needs %s or later. An older one is not\n"+
				"rejected up front — an image pull fails partway through, while the layer\n"+
				"is unpacked.\n"+
				"Ubuntu 24.04 LTS ships 1.7.1, so a distribution package may not be enough:\n"+
				"install a newer erofs-utils from backports, from a later release, or from\n"+
				"source (https://git.kernel.org/pub/scm/linux/kernel/git/xiang/erofs-utils.git).",
				path, text, erofsMinimum),
		}
	}
	return Result{Status: StatusOK, Detail: text}
}

// firstLine is the tool's version banner without the commentary that follows it, for quoting
// back to the user.
func firstLine(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(line)
}
