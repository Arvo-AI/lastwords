package agent

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Arvo-AI/lastwords/internal/procfs"
	"github.com/Arvo-AI/lastwords/internal/store"
)

type section struct {
	Title string
	Body  string
}

func typesUID(s string) types.UID { return types.UID(s) }

func headlinePressure(c *store.Capture) string {
	m := c.Memory
	h := fmt.Sprintf("%s/%s at %.0f%% of its %s memory limit", c.Pod, c.Container, 100*m.Ratio, procfs.Bytes(m.LimitBytes))
	if m.GrowthBytesPerS > 64*1024 {
		h += fmt.Sprintf(", growing %s/s", procfs.Bytes(uint64(m.GrowthBytesPerS)))
		switch {
		case m.SecondsToLimit > 0 && m.SecondsToLimit < 1:
			h += " (OOM imminent)"
		case m.SecondsToLimit >= 1 && m.SecondsToLimit < 3600:
			h += fmt.Sprintf(" (OOM in about %s)", (time.Duration(m.SecondsToLimit) * time.Second).Round(time.Second))
		}
	}
	return h
}

func headlineDeath(c *store.Capture, limit uint64) string {
	t := c.Termination
	when := t.FinishedAt.UTC().Format("15:04:05 UTC")
	if t.Reason == "OOMKilled" {
		h := fmt.Sprintf("%s/%s was OOMKilled at %s", c.Pod, c.Container, when)
		if limit > 0 {
			h += fmt.Sprintf(" (limit %s)", procfs.Bytes(limit))
		}
		if n := countSnapshots(c.Findings); n > 0 {
			h += fmt.Sprintf("; %d snapshot(s) taken before the kill", n)
		}
		return h
	}
	return fmt.Sprintf("%s/%s crashed at %s: %s, exit code %d", c.Pod, c.Container, when, nz(t.Reason, "Error"), t.ExitCode)
}

// render builds summary.md: the one file a human or an agent should read first.
func render(c *store.Capture, samples []Sample, secs []section) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", c.Headline)
	fmt.Fprintf(&b, "- **Capture:** `%s` (%s, trigger: %s)\n", c.ID, c.Kind, c.Trigger)
	fmt.Fprintf(&b, "- **When:** %s\n", c.Time.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Where:** pod `%s/%s`, container `%s`, node `%s`\n", c.Namespace, c.Pod, nz(c.Container, "-"), c.Node)
	if c.Owner != "" {
		fmt.Fprintf(&b, "- **Workload:** %s\n", c.Owner)
	}
	if c.Image != "" {
		fmt.Fprintf(&b, "- **Image:** `%s`\n", c.Image)
	}
	if c.Runtime != "" {
		fmt.Fprintf(&b, "- **Runtime:** %s\n", c.Runtime)
	}
	if c.RestartCount > 0 {
		fmt.Fprintf(&b, "- **Restarts so far:** %d\n", c.RestartCount)
	}
	if m := c.Memory; m != nil {
		b.WriteString("\n## Memory\n\n")
		fmt.Fprintf(&b, "- Working set %s of %s limit (%.0f%%); total usage incl. reclaimable cache %s\n", procfs.Bytes(m.WorkingSetBytes), limitStr(m.LimitBytes), 100*m.Ratio, procfs.Bytes(m.UsageBytes))
		if m.GrowthBytesPerS != 0 {
			fmt.Fprintf(&b, "- Growth over the last 10 s: %s/s", signedBytes(m.GrowthBytesPerS))
			if m.SecondsToLimit >= 1 {
				fmt.Fprintf(&b, ", limit reached in about %.0f s", m.SecondsToLimit)
			}
			b.WriteString("\n")
		}
		if m.PSIFullAvg10 > 0 {
			fmt.Fprintf(&b, "- Memory pressure (PSI full avg10): %.1f%% of time all tasks were stalled on memory\n", m.PSIFullAvg10)
		}
		if len(m.Breakdown) > 0 {
			var parts []string
			for _, k := range []string{"anon", "file", "kernel", "shmem", "sock", "slab", "rss", "cache"} {
				if v, ok := m.Breakdown[k]; ok && v > 0 {
					parts = append(parts, fmt.Sprintf("%s %s", k, procfs.Bytes(v)))
				}
			}
			if len(parts) > 0 {
				fmt.Fprintf(&b, "- Breakdown: %s\n", strings.Join(parts, ", "))
			}
		}
		if sp := Sparkline(samples, 48); sp != "" {
			fmt.Fprintf(&b, "- Timeline: `%s`\n", sp)
		}
	}
	if t := c.Termination; t != nil {
		b.WriteString("\n## Termination\n\n")
		fmt.Fprintf(&b, "- Reason %s, exit code %d", nz(t.Reason, "-"), t.ExitCode)
		if t.Signal != 0 {
			fmt.Fprintf(&b, ", signal %d", t.Signal)
		}
		fmt.Fprintf(&b, "\n- Ran from %s to %s (%s)\n", t.StartedAt.UTC().Format(time.RFC3339), t.FinishedAt.UTC().Format(time.RFC3339), t.FinishedAt.Sub(t.StartedAt).Round(time.Second))
	}
	if len(c.Findings) > 0 {
		b.WriteString("\n## What lastwords found\n\n")
		for _, f := range c.Findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	for _, s := range secs {
		fmt.Fprintf(&b, "\n## %s\n\n%s", s.Title, s.Body)
	}
	if len(c.Related) > 0 {
		b.WriteString("\n## Related captures\n\n")
		for _, r := range c.Related {
			fmt.Fprintf(&b, "- `%s` (kubectl lastwords show %s)\n", r, r)
		}
	}
	b.WriteString("\n## Files\n\n")
	for _, f := range c.Files {
		fmt.Fprintf(&b, "- `%s` (%s): %s\n", f.Name, procfs.Bytes(uint64(f.Bytes)), f.Description)
	}
	b.WriteString("- `summary.md`: this file\n")
	if len(c.Errors) > 0 {
		b.WriteString("\n## Could not collect\n\n")
		for _, e := range c.Errors {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	return b.String()
}

// countSnapshots reads the snapshot count back from the findings text.
func countSnapshots(findings []string) int {
	for _, f := range findings {
		var n int
		if _, err := fmt.Sscanf(f, "%d snapshot(s) of what was using the memory", &n); err == nil {
			return n
		}
	}
	return 0
}

func limitStr(n uint64) string {
	if n == 0 {
		return "no"
	}
	return procfs.Bytes(n)
}

func signedBytes(v float64) string {
	if v < 0 {
		return "-" + procfs.Bytes(uint64(-v))
	}
	return "+" + procfs.Bytes(uint64(v))
}
