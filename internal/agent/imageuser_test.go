package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every image must state its user as NUMBERS, and this is the test that says so.
//
// The images all end `USER agent` once, which reads better and was wrong. An image's USER may
// be a name or an id, but resolving a name means reading /etc/passwd out of the image's own
// Linux root filesystem, and off Linux the host cannot mount it: containerd stores the string
// in Process.User.Username and leaves the uid at zero (see internal/sandbox/imageconfig.go).
// Nothing in the guest reads that field back, so on macOS and Windows a named user meant
// **uid 0** — silently, with no line in any log.
//
// The agent that found it is the one defined in this package: `claude` runs
// `claude --dangerously-skip-permissions` (agent.go), which Claude Code refuses outright when
// its euid is 0 — "cannot be used with root/sudo privileges for security reasons". So the
// sandbox came up and the agent would not start. Every other agent here was equally root; it
// just did not object.
//
// A numeric USER needs no filesystem and is honoured identically on all three hosts, so this
// is a property of the Dockerfiles rather than of any one platform, and it is asserted here
// rather than in a doc comment because a doc comment cannot fail.
func TestAgentImagesRunAsANumericNonRootUser(t *testing.T) {
	dir := filepath.Join("..", "..", "images")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	// Every agent image is `FROM ${BASE_IMAGE}`, so an image that sets no USER of its own
	// inherits the base's — which is correct and must not be reported as root. Only the
	// EFFECTIVE user is a fact about what runs.
	base, err := finalUser(filepath.Join(dir, "base", "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the base image: %v", err)
	}
	if base == "" {
		t.Fatal("images/base/Dockerfile declares no USER, so every agent image runs as root")
	}

	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			// images/ also holds a README.
			continue
		}
		path := filepath.Join(dir, entry.Name(), "Dockerfile")
		user, err := finalUser(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		checked++

		inherited := ""
		if user == "" && entry.Name() != "base" {
			// Legitimate: base already dropped privileges and this image never took them
			// back. What would NOT be legitimate is ending on the `USER root` these images
			// use to install things, and that is a USER line, so it is caught below.
			user, inherited = base, " (inherited from base)"
		}

		if user == "" {
			t.Errorf("%s declares no USER, so the image runs as root", path)
			continue
		}
		if problem := numericNonRoot(user); problem != "" {
			t.Errorf("%s ends with `USER %s`%s: %s", path, user, inherited, problem)
		}
	}

	if checked == 0 {
		t.Fatalf("no Dockerfiles found under %s; this test asserted nothing", dir)
	}
	t.Logf("checked %d images", checked)
}

// finalUser returns the argument of the LAST USER instruction in a Dockerfile, or "" when it
// has none. The last one is what lands in the image config; the earlier `USER root` in each
// agent image exists to install things and is then dropped.
func finalUser(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var final string
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "USER" {
			final = fields[1]
		}
	}
	return final, nil
}

// numericNonRoot returns "" when a USER argument is a non-zero uid:gid pair, and otherwise
// says what is wrong with it.
func numericNonRoot(user string) string {
	uid, gid, hasGID := strings.Cut(user, ":")
	id, err := strconv.Atoi(uid)
	if err != nil {
		// "root" is both a name and uid 0, and saying only "cannot be resolved" would
		// undersell it: that one is root on every host, not just off Linux.
		if uid == "root" {
			return "that is root"
		}
		return "a NAME cannot be resolved off Linux and becomes uid 0 there; use the " +
			"numeric form"
	}
	if id == 0 {
		return "that is root"
	}
	// A bare uid leaves gid 0, which is what containerd's WithUserID settles on too —
	// legal, but these images all have a group of their own, so requiring both keeps the
	// primary group from silently being root's.
	if !hasGID {
		return "name the gid too, or the primary group is 0"
	}
	if g, err := strconv.Atoi(gid); err != nil || g == 0 {
		return "the group must be a non-zero number"
	}
	return ""
}
