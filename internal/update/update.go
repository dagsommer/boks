// Package update tells a user when a newer Boks exists, and what to type to get it.
//
// # Why this is not the telemetry Boks promised not to do
//
// Boks promises no telemetry, and that promise is load-bearing: it is a tool for running
// untrusted code, and a tool that phones home about what you ran with it would be a poor one.
// An update check sits close enough to that line to deserve an explicit account of where the
// line is.
//
// What is sent: nothing. The check is an HTTP HEAD for the GitHub releases-latest URL, which
// redirects to the tag of the newest release, and the answer is read out of the Location
// header. No version, no identifier, no counter, no install id, no sandbox name, no
// user-agent carrying a build. The comparison against the running version happens locally,
// which is why the request does not need to say what the running version is.
//
// What is unavoidably revealed: that some machine at your IP address asked GitHub about this
// repository, at most once a day. That is the whole of it, it is the same thing `git fetch`
// reveals, and it is disclosed to the user on the first run rather than described only here.
//
// The distinction that matters is not "does it touch the network" but "does it report on
// you". A check that sends no facts about your use of Boks reports nothing, and the value —
// a user on a version with a known sandbox-escape learns there is a fix — is real.
//
// # Three properties this must have
//
// It must never delay a command. `boks run` is the hot path and a hypervisor is starting
// behind it; a synchronous HTTP request on that path would be felt, and a hung one would be
// blamed on the VM. So the notice is printed from a cache, and the refresh that fills the
// cache is fired into the background and abandoned if the command finishes first. A user
// learns about a new release on the run *after* the check that found it. That is a fine price
// for a guarantee this simple.
//
// It must never fail a command. Every error here — no network, a proxy that refuses, a
// corrupt cache, an unwritable state directory — resolves to saying nothing.
//
// It must be refusable. BOKS_NO_UPDATE_CHECK=1 turns it off, DO_NOT_TRACK=1 is honoured
// because someone who has set it has already said this once, and CI is skipped because
// automation did not ask and should not spend the round trip.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// latestURL redirects to the newest release's tag page. HEAD against it, without following
// the redirect, answers "what is the newest version" in one round trip and no response body.
//
// The GitHub REST API would also answer it, in JSON, but it is rate limited to 60 requests an
// hour per IP for unauthenticated callers — shared by everyone behind one NAT, which is a
// company — and it returns a large object to read one field from. This URL is not rate
// limited in that way and its answer is a single header.
const latestURL = "https://github.com/dagsommer/boks/releases/latest"

// checkInterval is how long a cached answer is trusted. Releases are not frequent enough for
// a shorter one to tell anybody anything, and this is the interval a user is told about.
const checkInterval = 24 * time.Hour

// fetchTimeout bounds the background request. It is generous because nothing waits on it, and
// it exists only so an abandoned goroutine cannot hold a connection open indefinitely in a
// long-running `boks run`.
const fetchTimeout = 10 * time.Second

// cacheVersion is the schema version of the record. An unrecognised one reads as an empty
// record, which costs one extra check.
const cacheVersion = 1

// cache is what Boks remembers between runs.
type cache struct {
	V int `json:"v"`
	// Disclosed records that the user has been told the check happens. Until it is set,
	// no request is made — see Notify.
	Disclosed bool `json:"disclosed,omitempty"`
	// Checked is when the last attempt was *started*, successful or not — see Notify for
	// why it is written before the request rather than after. A machine behind a firewall
	// that blocks GitHub must not retry on every single run, and the cost of that choice
	// is that a failed check waits a day to try again.
	Checked time.Time `json:"checked,omitempty"`
	// Latest is the newest release tag seen. Empty when nothing has been learned yet.
	Latest string `json:"latest,omitempty"`
}

// Config is what a call to Notify needs to know. Every field has a working zero value except
// StateDir and Current, and the function hooks exist so tests can drive it without a network,
// a clock, or an environment.
type Config struct {
	// StateDir is where the cache lives. Empty disables the check: with nowhere to
	// remember a disclosure, there is no way to make the request honestly.
	StateDir string
	// Current is the running version, internal/cli.Version.
	Current string
	// ExePath is the running binary, used to work out the upgrade command. Optional.
	ExePath string

	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// Now defaults to time.Now.
	Now func() time.Time
	// fetch defaults to fetchLatest. Tests replace it.
	fetch func(context.Context) (string, error)
}

func (c Config) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Notice is what to print. A nil *Notice means print nothing, which is the overwhelmingly
// common case.
type Notice struct {
	// Disclosure is set on the one run where the user is told the check exists. When it
	// is set, no update is being reported: the two never appear together, because the
	// first run makes no request.
	Disclosure bool
	// Latest is the newer version, when there is one.
	Latest string
	// Upgrade is the command to run, or a URL when the install method is unrecognised.
	Upgrade string
}

// Notify reports what should be printed, and refreshes the cache in the background when it is
// stale.
//
// The returned channel is closed when any background work has finished. Production callers
// ignore it — the point of the background refresh is that nothing waits for it — and tests
// wait on it so they are deterministic.
func Notify(cfg Config) (*Notice, <-chan struct{}) {
	done := make(chan struct{})

	if !cfg.enabled() {
		close(done)
		return nil, done
	}

	c := loadCache(cfg.StateDir)

	// First encounter: say that this happens, and make no request. Telling someone after
	// the fact that a request has already gone out is not disclosure, and the cost of
	// getting the order right is one day's delay in learning about a release, once, ever.
	if !c.Disclosed {
		c.Disclosed = true
		c.save(cfg.StateDir)
		close(done)
		return &Notice{Disclosure: true}, done
	}

	// Decide what to say from what is already known, before any network work, so the
	// notice costs nothing on a run where the cache is warm.
	var notice *Notice
	if c.Latest != "" && IsNewer(cfg.Current, c.Latest) {
		notice = &Notice{Latest: c.Latest, Upgrade: Detect(cfg.ExePath).Upgrade()}
	}

	if cfg.now().Sub(c.Checked) < checkInterval {
		close(done)
		return notice, done
	}

	// Record the attempt now, before making it, rather than when it returns.
	//
	// This is not bookkeeping tidiness, it is the only thing that makes the once-a-day
	// bound real. The refresh runs in a goroutine that is abandoned when the process
	// exits, and plenty of commands exit in well under the time a request takes — a `boks
	// run` against a containerd that is not there fails in about a tenth of a second.
	// Measured on 2026-08-15: with the timestamp written on completion, that run made a
	// request, died before the answer arrived, wrote nothing, and made another request the
	// next time, forever. Writing it here bounds requests to one a day whatever the
	// command does, and the cost is that a check killed mid-flight waits a day to retry.
	c.Checked = cfg.now()
	c.save(cfg.StateDir)

	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		fetch := cfg.fetch
		if fetch == nil {
			fetch = fetchLatest
		}
		latest, err := fetch(ctx)
		if err != nil || latest == "" {
			return
		}

		// Re-read rather than writing back the copy this run loaded: two boks
		// processes can be running, and the loser of that race should lose only its
		// own answer rather than reverting the winner's.
		c := loadCache(cfg.StateDir)
		c.Latest = latest
		c.save(cfg.StateDir)
	}()

	return notice, done
}

// enabled reports whether the check should run at all.
func (c Config) enabled() bool {
	if c.StateDir == "" {
		return false
	}
	// A local build has no release to be behind. This also keeps the check out of the
	// way of anyone developing Boks itself, who is by definition ahead of it.
	if _, ok := parseVersion(c.Current); !ok {
		return false
	}
	if truthy(c.getenv("BOKS_NO_UPDATE_CHECK")) {
		return false
	}
	// A community convention rather than a standard, but its meaning is unambiguous and
	// someone who has set it should not have to learn a second variable per tool.
	if truthy(c.getenv("DO_NOT_TRACK")) {
		return false
	}
	// Set by GitHub Actions, GitLab CI, CircleCI, Travis and others. Automation did not
	// ask to be told about releases and cannot act on the answer.
	if truthy(c.getenv("CI")) {
		return false
	}
	return true
}

// truthy reads the values people actually set these variables to. "0", "false" and empty are
// off; anything else is on, because someone who writes BOKS_NO_UPDATE_CHECK=yes means yes.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// ErrNoReleases reports that the repository has no published release. It is a normal answer
// rather than a fault — it is what every check returns until the first release is cut — so it
// is a named error a caller can phrase for a human instead of an opaque one.
var ErrNoReleases = errors.New("no release has been published yet")

// fetchLatest asks GitHub for the newest release tag.
func fetchLatest(ctx context.Context) (string, error) { return fetchFrom(ctx, latestURL) }

// fetchFrom is fetchLatest against a given URL, so the redirect handling can be tested
// against a local server rather than against GitHub.
func fetchFrom(ctx context.Context, url string) (string, error) {
	// The redirect is the answer, so following it would throw the answer away and
	// download a page to find it again.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	// GitHub requires a User-Agent. This one carries no version and no identifier: see
	// the package comment for why that is the point rather than an omission.
	req.Header.Set("User-Agent", "boks")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("no redirect to a release tag")
	}
	// .../releases/tag/v1.2.3
	idx := strings.LastIndex(loc, "/tag/")
	if idx < 0 {
		// Measured against the real repository on 2026-08-15, before the first
		// release existed: GitHub does not 404 for a repository with no releases, it
		// redirects /releases/latest to the /releases index. Reporting that as a
		// malformed redirect would send someone looking for a bug in Boks, so it is
		// named for what it is.
		if strings.HasSuffix(strings.TrimSuffix(loc, "/"), "/releases") {
			return "", ErrNoReleases
		}
		return "", errors.New("redirect did not name a tag: " + loc)
	}
	tag := strings.Trim(loc[idx+len("/tag/"):], "/")
	if _, ok := parseVersion(tag); !ok {
		return "", errors.New("redirect named something that is not a version: " + tag)
	}
	return tag, nil
}

// cachePath is where the record lives.
func cachePath(stateDir string) string { return filepath.Join(stateDir, "update.json") }

// loadCache reads the record. Anything unreadable, unparseable, or from another schema
// version reads as empty — which re-discloses and re-checks, and is the safe direction.
func loadCache(stateDir string) cache {
	raw, err := os.ReadFile(cachePath(stateDir))
	if err != nil {
		return cache{}
	}
	var c cache
	if err := json.Unmarshal(raw, &c); err != nil || c.V != cacheVersion {
		return cache{}
	}
	return c
}

// save writes the record, reporting nothing.
//
// Written to a temporary file and renamed, because the writer is a goroutine that may be
// abandoned when the process exits: a rename is atomic, so a killed process leaves either the
// old record or the new one, never half of either. The temporary name is fixed rather than
// random so that a process killed between create and rename leaves one stale file that the
// next run overwrites, instead of accumulating one per interrupted run.
func (c cache) save(stateDir string) {
	if stateDir == "" {
		return
	}
	c.V = cacheVersion
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return
	}
	tmp := cachePath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, cachePath(stateDir)); err != nil {
		_ = os.Remove(tmp)
	}
}

// Check asks for the newest release now, waiting for the answer.
//
// This is for `boks version --check`, where the user has explicitly asked and blocking is the
// expected behaviour. It ignores the cache in both directions: it does not read a stale answer
// to someone who asked for a fresh one, and it does not write one, so an explicit check never
// silences the daily one. The opt-outs are not consulted either — this is not the automatic
// check, it is a person typing a command that says "ask GitHub".
func Check(ctx context.Context) (string, error) { return fetchLatest(ctx) }
