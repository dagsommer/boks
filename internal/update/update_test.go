package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseVersionAndOrdering(t *testing.T) {
	// The pairs that decide whether anything is ever printed. The negative cases matter
	// more than the positive ones: every one of them is a way to nag a user about an
	// update that does not exist.
	cases := []struct {
		current, latest string
		want            bool
		why             string
	}{
		{"v0.1.0", "v0.2.0", true, "a newer minor"},
		{"v0.1.0", "v0.1.1", true, "a newer patch"},
		{"v0.9.0", "v1.0.0", true, "a newer major"},
		{"0.1.0", "v0.2.0", true, "the v prefix is optional on either side"},
		{"v0.2.0", "v0.1.0", false, "an older release is not news"},
		{"v0.1.0", "v0.1.0", false, "the same version is not news"},
		{"v0.10.0", "v0.9.0", false, "components compare numerically, not as strings"},
		{"v0.9.0", "v0.10.0", true, "and the other way round"},
		{"dev", "v9.9.9", false, "a local build is never behind"},
		{"v0.1.0", "", false, "an empty answer says nothing"},
		{"v0.1.0", "not-a-version", false, "an unparseable answer says nothing"},
		{"v0.1.0", "v0.1", false, "a two-component version is not one we produce"},
		{"v0.1.0-rc.1", "v0.1.0", true, "a final release supersedes its own candidate"},
		{"v0.1.0", "v0.1.0-rc.1", false, "and a candidate never supersedes the release"},
		{"v0.1.0-rc.1", "v0.1.0-rc.2", true, "candidates order among themselves"},
		{"v0.1.0", "v0.1.1+build.5", true, "build metadata is ignored, not rejected"},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v — %s", c.current, c.latest, got, c.want, c.why)
		}
	}
}

// fakeFetch records whether it was called and returns a fixed answer.
type fakeFetch struct {
	calls  int
	latest string
	err    error
}

func (f *fakeFetch) fetch(context.Context) (string, error) {
	f.calls++
	return f.latest, f.err
}

func baseConfig(t *testing.T, f *fakeFetch) Config {
	t.Helper()
	return Config{
		StateDir: t.TempDir(),
		Current:  "v0.1.0",
		Getenv:   func(string) string { return "" },
		Now:      func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
		fetch:    f.fetch,
	}
}

// The disclosure has to come before the request, not alongside it. A test that only checked
// the text would pass while the request went out first, so this asserts the call count.
func TestFirstRunDisclosesAndMakesNoRequest(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)

	notice, done := Notify(cfg)
	<-done

	if notice == nil || !notice.Disclosure {
		t.Fatalf("first run should disclose, got %+v", notice)
	}
	if notice.Latest != "" {
		t.Errorf("the disclosure run must not also report a version, got %q", notice.Latest)
	}
	if f.calls != 0 {
		t.Errorf("the first run made %d request(s); it must make none before disclosing", f.calls)
	}
}

func TestSecondRunChecksAndThirdRunReports(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)

	_, done := Notify(cfg) // discloses
	<-done

	notice, done := Notify(cfg)
	<-done
	if f.calls != 1 {
		t.Fatalf("the run after the disclosure made %d requests, want 1", f.calls)
	}
	if notice != nil {
		t.Errorf("the checking run reports nothing — the answer arrives after it: %+v", notice)
	}

	// The clock has not moved, so no second request; the cached answer is what speaks.
	notice, done = Notify(cfg)
	<-done
	if f.calls != 1 {
		t.Errorf("a warm cache made another request (%d total)", f.calls)
	}
	if notice == nil || notice.Latest != "v9.9.9" {
		t.Fatalf("want the cached newer version reported, got %+v", notice)
	}
	if notice.Upgrade == "" {
		t.Error("a notice with no upgrade instruction is half a message")
	}
}

func TestStaleCacheRechecks(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)
	_, done := Notify(cfg)
	<-done
	_, done = Notify(cfg)
	<-done

	cfg.Now = func() time.Time { return time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC) }
	_, done = Notify(cfg)
	<-done
	if f.calls != 2 {
		t.Errorf("a cache older than the interval made %d requests, want 2", f.calls)
	}
}

// A machine that cannot reach GitHub must not try on every single run.
func TestFailedCheckStillBacksOff(t *testing.T) {
	f := &fakeFetch{err: errors.New("dial tcp: no route to host")}
	cfg := baseConfig(t, f)
	_, done := Notify(cfg)
	<-done

	for range 3 {
		notice, done := Notify(cfg)
		<-done
		if notice != nil {
			t.Fatalf("a failed check must say nothing, got %+v", notice)
		}
	}
	if f.calls != 1 {
		t.Errorf("a failing check retried on every run (%d requests); it must back off", f.calls)
	}
}

func TestOptOuts(t *testing.T) {
	for _, key := range []string{"BOKS_NO_UPDATE_CHECK", "DO_NOT_TRACK", "CI"} {
		t.Run(key, func(t *testing.T) {
			f := &fakeFetch{latest: "v9.9.9"}
			cfg := baseConfig(t, f)
			cfg.Getenv = func(k string) string {
				if k == key {
					return "1"
				}
				return ""
			}
			for range 2 {
				notice, done := Notify(cfg)
				<-done
				if notice != nil {
					t.Fatalf("%s=1 still produced %+v", key, notice)
				}
			}
			if f.calls != 0 {
				t.Errorf("%s=1 still made %d request(s)", key, f.calls)
			}
			// And it must not have left a disclosure behind: turning the check
			// back on should still disclose before its first request.
			if _, err := os.Stat(cachePath(cfg.StateDir)); err == nil {
				t.Error("an opted-out run wrote a cache file")
			}
		})
	}
}

func TestOptOutFalseValuesDoNotDisable(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)
	cfg.Getenv = func(k string) string {
		if k == "CI" {
			return "false"
		}
		return ""
	}
	notice, done := Notify(cfg)
	<-done
	if notice == nil || !notice.Disclosure {
		t.Errorf("CI=false is not an opt-out, got %+v", notice)
	}
}

func TestDevBuildNeverChecks(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)
	cfg.Current = "dev"
	notice, done := Notify(cfg)
	<-done
	if notice != nil || f.calls != 0 {
		t.Errorf("a dev build produced %+v after %d requests", notice, f.calls)
	}
}

func TestNoStateDirIsSilent(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)
	cfg.StateDir = ""
	notice, done := Notify(cfg)
	<-done
	if notice != nil || f.calls != 0 {
		t.Errorf("with nowhere to record a disclosure it must stay quiet, got %+v", notice)
	}
}

func TestCorruptCacheIsTreatedAsEmpty(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath(cfg.StateDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	notice, done := Notify(cfg)
	<-done
	if notice == nil || !notice.Disclosure {
		t.Errorf("a corrupt cache should re-disclose rather than assume consent, got %+v", notice)
	}
}

// Losing the record costs a repeated notice; it must never cost a failed command, so an
// unwritable state directory has to be survivable.
func TestUnwritableStateDirDoesNotPanic(t *testing.T) {
	f := &fakeFetch{latest: "v9.9.9"}
	cfg := baseConfig(t, f)
	blocked := filepath.Join(cfg.StateDir, "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = filepath.Join(blocked, "under-a-file")
	notice, done := Notify(cfg)
	<-done
	if notice == nil || !notice.Disclosure {
		t.Errorf("got %+v", notice)
	}
}

// Every platform's rules, exercised on whatever platform the suite runs on. An earlier
// version called Detect() and so tested only the host's rules: on the Windows CI leg the two
// Homebrew cases became assertions that a Cellar path is NOT Homebrew, and failed.
func TestDetect(t *testing.T) {
	cases := []struct {
		goos, path string
		want       Method
		why        string
	}{
		{"darwin", "", MethodUnknown, "no path is not a reason to guess"},
		{"darwin", "/opt/homebrew/Cellar/boks/0.1.0/bin/boks", MethodHomebrew, "Apple silicon prefix"},
		{"darwin", "/usr/local/Cellar/boks/0.1.0/bin/boks", MethodHomebrew, "Intel prefix"},
		{"darwin", "/Users/someone/bin/boks", MethodUnknown, "a hand-installed binary"},

		{"linux", "/home/alice/.local/bin/boks", MethodUnknown, "a tarball install is not package managed"},
		{"linux", "/tmp/boks", MethodUnknown, "an arbitrary location"},

		{"windows", `C:\Users\a\AppData\Local\Microsoft\WinGet\Packages\dagsommer.boks\boks.exe`,
			MethodWinget, "a winget portable package"},
		{"windows", `C:\Users\a\AppData\Local\Microsoft\WinGet\Links\boks.exe`,
			MethodWinget, "the winget shim directory"},
		{"windows", `C:\Program Files\WinGet\Packages\dagsommer.boks\boks.exe`,
			MethodWinget, "a machine-scope install is not under AppData"},
		{"windows", `C:\tools\boks.exe`, MethodUnknown, "unpacked by hand"},
		{"windows", "/opt/homebrew/Cellar/boks/0.1.0/bin/boks", MethodUnknown,
			"a Cellar path on Windows is not a Homebrew install"},
	}
	for _, c := range cases {
		if got := detect(c.goos, c.path); got != c.want {
			t.Errorf("detect(%q, %q) = %v, want %v — %s", c.goos, c.path, got, c.want, c.why)
		}
	}
}

// The exported entry point has to agree with the parameterised one for the running platform,
// or the tests above would be checking a function nothing calls.
func TestDetectMatchesTheExportedEntryPoint(t *testing.T) {
	for _, path := range []string{"", "/tmp/boks", "/opt/homebrew/Cellar/boks/0.1.0/bin/boks"} {
		if got, want := Detect(path), detect(runtime.GOOS, path); got != want {
			t.Errorf("Detect(%q) = %v but detect(%q, %q) = %v", path, got, runtime.GOOS, path, want)
		}
	}
}

func TestUpgradeInstructionsAreDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]Method{}
	for _, m := range []Method{MethodUnknown, MethodHomebrew, MethodWinget, MethodDeb, MethodRPM} {
		got := m.Upgrade()
		if got == "" {
			t.Errorf("method %v has no upgrade instruction", m)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("methods %v and %v give the same instruction %q", prev, m, got)
		}
		seen[got] = m
	}
}

// The redirect shapes GitHub actually produces. The no-releases case is not hypothetical:
// it is what the real repository returned on 2026-08-15, and it is what every check returns
// until the first release is cut, so it must be a named answer rather than a parse failure.
func TestFetchLatestReadsTheRedirect(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		want     string
		wantErr  error
	}{
		{"a released repository", http.StatusFound,
			"https://github.com/dagsommer/boks/releases/tag/v1.2.3", "v1.2.3", nil},
		{"no releases yet", http.StatusFound,
			"https://github.com/dagsommer/boks/releases", "", ErrNoReleases},
		{"no releases, trailing slash", http.StatusFound,
			"https://github.com/dagsommer/boks/releases/", "", ErrNoReleases},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("asked for %s; a HEAD is enough and downloads no page", r.Method)
				}
				if ua := r.Header.Get("User-Agent"); ua != "boks" {
					t.Errorf("User-Agent %q carries more than it should", ua)
				}
				w.Header().Set("Location", c.location)
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			got, err := fetchFrom(context.Background(), srv.URL)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A redirect Boks does not understand must never become a version. Otherwise a GitHub
// error page or an intercepting proxy could make Boks nag about a release that does not
// exist, which is the failure mode that would destroy trust in the notice.
func TestFetchLatestRejectsNonVersions(t *testing.T) {
	for _, loc := range []string{
		"https://github.com/login?return_to=%2Fdagsommer%2Fboks",
		"https://github.com/dagsommer/boks/releases/tag/nightly",
		"https://github.com/dagsommer/boks/releases/tag/",
		"",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if loc != "" {
				w.Header().Set("Location", loc)
			}
			w.WriteHeader(http.StatusFound)
		}))
		got, err := fetchFrom(context.Background(), srv.URL)
		srv.Close()
		if err == nil {
			t.Errorf("Location %q produced version %q; it should have been refused", loc, got)
		}
	}
}

// The once-a-day bound has to hold for a process that exits before its check returns, which
// is most of them: the goroutine is abandoned, so anything it was going to write is lost.
//
// The assertion is on the record Notify leaves behind by the time it RETURNS, not on what a
// goroutine eventually does. That is both the real invariant and the only race-free way to
// state it — an earlier version of this test counted calls after spawning and could observe
// any number depending on scheduling.
func TestAttemptIsRecordedBeforeItCompletes(t *testing.T) {
	blocked := make(chan struct{})

	var calls atomic.Int32
	started := make(chan struct{}, 16)
	cfg := baseConfig(t, &fakeFetch{})
	cfg.fetch = func(ctx context.Context) (string, error) {
		calls.Add(1)
		started <- struct{}{}
		// Never returns while the test runs — exactly like a request outliving the
		// process that made it. It then fails, so that releasing it at the end of
		// the test cannot race t.TempDir's cleanup by writing the cache back.
		<-blocked
		return "", errors.New("released at end of test")
	}

	_, done := Notify(cfg) // discloses, no request
	<-done

	// The caller walks away without waiting, as boks does.
	_, abandoned := Notify(cfg)
	// Released only once every assertion below has been made, and waited for, so no
	// goroutine outlives the test.
	defer func() { close(blocked); <-abandoned }()

	// Synchronous invariant: the attempt is on disk already.
	if c := loadCache(cfg.StateDir); c.Checked.IsZero() {
		t.Fatal("Notify returned without recording the attempt, so a process that exits " +
			"now would check again on its next run")
	}
	<-started // the request really was started, so this is not passing vacuously

	// And every later run inside the interval must decline to start another.
	for range 5 {
		Notify(cfg)
	}
	select {
	case <-started:
		t.Errorf("a second request was started; %d in total", calls.Load())
	case <-time.After(100 * time.Millisecond):
	}
}
