package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// pullProgressInterval is how often a pull reports itself. Slow enough not to scroll a
// terminal, fast enough that a stalled download is visibly stalled.
const pullProgressInterval = 2 * time.Second

// reportPullProgress prints how far an image pull has got, until ctx is cancelled.
//
// # Why this exists rather than a spinner
//
// A pull is the longest thing `boks run` does — hundreds of megabytes on a cold machine — and
// before this it was two lines with a silence between them. A silence is indistinguishable
// from a hang, and the natural reaction to a hang is Ctrl-C, which leaves a half-unpacked
// snapshot to clean up. What the user needs is not decoration but the two facts that
// distinguish slow from stuck: how many bytes have arrived, and whether that number is moving.
//
// # How it reads them
//
// containerd's client.Pull gives no progress channel. What it does give is the content store,
// where every in-flight download is an "ingest" with an offset and, usually, a total — the
// same source `ctr` reads for its own progress bars. Polling it is the supported way to see
// inside a pull that is already running.
//
// A status with Total == 0 is a layer whose size the registry did not declare. It is counted
// toward the byte total and not toward the expected one, so a pull of such layers reports
// bytes without a percentage rather than a percentage that is wrong.
//
// Nothing here can fail the pull. Every error from the content store is dropped: this function
// describes work happening elsewhere, and a progress reporter that could abort the thing it is
// reporting on would be a worse bug than no progress at all.
func reportPullProgress(ctx context.Context, c *client.Client, say func(string, ...any), bar func(done, expected int64, layers int)) {
	ticker := time.NewTicker(pullProgressInterval)
	defer ticker.Stop()

	var lastBytes int64
	stalledFor := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		statuses, err := c.ContentStore().ListStatuses(ctx)
		if err != nil || len(statuses) == 0 {
			// No active ingests means the bytes are in and containerd is unpacking,
			// which is disk work this cannot see. Saying nothing is right: the caller
			// prints a line when the pull returns.
			continue
		}

		var done, expected int64
		for _, s := range statuses {
			done += s.Offset
			if s.Total > 0 {
				expected += s.Total
			}
		}

		// A number that has not moved between two ticks is the case this function is
		// most useful for, so it is called out rather than left for the user to notice
		// by comparing two identical lines.
		if done == lastBytes {
			stalledFor += pullProgressInterval
		} else {
			stalledFor = 0
		}
		lastBytes = done

		if bar != nil && stalledFor < 3*pullProgressInterval {
			bar(done, expected, len(statuses))
			continue
		}
		switch {
		case stalledFor >= 3*pullProgressInterval:
			say("image: %s downloaded, no progress for %s (%d layer(s) in flight)",
				humanBytes(done), stalledFor.Round(time.Second), len(statuses))
		case expected > 0:
			say("image: %s of %s (%d%%), %d layer(s)",
				humanBytes(done), humanBytes(expected), done*100/expected, len(statuses))
		default:
			say("image: %s downloaded, %d layer(s)", humanBytes(done), len(statuses))
		}
	}
}

// humanBytes formats a byte count the way a person reads one.
//
// Powers of 1024 with the units named for them, matching what `boks purge` prints, so two
// parts of the same CLI do not disagree about what "MiB" means.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// pullBar returns the function that draws a download as one bar redrawn in place, or nil when
// Progress is not a terminal and the line-per-tick form is what should be used. A bar written
// to a pipe or a log would be a thousand carriage returns.
func (c Config) pullBar() func(done, expected int64, layers int) {
	if c.Progress == nil || !c.ProgressTTY {
		return nil
	}
	return func(done, expected int64, layers int) {
		fmt.Fprint(c.Progress, "\r\033[K"+renderPullBar(done, expected, layers))
	}
}

// endPullBar clears the bar's line so what is printed next starts at the margin.
func (c Config) endPullBar() {
	if c.Progress != nil && c.ProgressTTY {
		fmt.Fprint(c.Progress, "\r\033[K")
	}
}

// renderPullBar draws "    [█████░░░░░]  56.6 MiB / 720.5 MiB · 7 layers". Without a known
// total there is nothing to fill a bar against, so it says the bytes and the layers only.
func renderPullBar(done, expected int64, layers int) string {
	const width = 10
	if expected <= 0 {
		return fmt.Sprintf("    %s downloaded · %d layers", humanBytes(done), layers)
	}
	filled := int(done * width / expected)
	filled = max(0, min(width, filled))
	return fmt.Sprintf("    [%s%s]  %s / %s · %d layers",
		strings.Repeat("█", filled), strings.Repeat("░", width-filled),
		humanBytes(done), humanBytes(expected), layers)
}
