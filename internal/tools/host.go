package tools

// The machine the assistant is running on, and the jobs on it.
//
// He starts long work and leaves: training runs, renders, agent fleets, slicing jobs. Checking on
// them costs a walk to a machine. The bridge is already on that machine, so the walk is the only
// part that was ever necessary.
//
// Nothing here can change anything. There is no start, no stop, no restart and no shell. That is
// not an oversight and not a setting: a sentence spoken by a visitor must not be able to end a
// twelve-hour training run, and the only reliable way to guarantee that is for the capability to be
// absent.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HostStatus reports how the machine is doing, in a sentence.
func HostStatus() *Tool {
	return &Tool{
		Name: "host_status",
		Description: "How the Linux machine running the assistant is doing: uptime, load, free " +
			"memory and free disk. Answers 'how is the machine', 'كيف الجهاز', 'is it busy', " +
			"'how much space is left'.",
		Access:  Read,
		Ambient: true,
		Timeout: 4 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			var parts []string

			if up := readFirstLine("/proc/uptime"); up != "" {
				if secs, err := strconv.ParseFloat(strings.Fields(up)[0], 64); err == nil {
					parts = append(parts, "up "+humanDuration(time.Duration(secs)*time.Second))
				}
			}
			if load := readFirstLine("/proc/loadavg"); load != "" {
				f := strings.Fields(load)
				if len(f) > 0 {
					parts = append(parts, "load "+f[0])
				}
			}
			if total, avail, ok := memInfo(); ok {
				parts = append(parts, fmt.Sprintf("%.1f of %.1f GB memory free",
					float64(avail)/1024/1024, float64(total)/1024/1024))
			}
			if free, ok := diskFreeGB(ctx, "/"); ok {
				parts = append(parts, fmt.Sprintf("%.0f GB disk free", free))
			}
			if len(parts) == 0 {
				return "", fmt.Errorf("nothing about the machine could be read")
			}
			return "The machine is " + strings.Join(parts, ", ") + ".", nil
		},
	}
}

// HostJobs reports what heavy work is running, which is the question he actually asks.
func HostJobs(watch []string) *Tool {
	return &Tool{
		Name: "host_jobs",
		Description: "What long-running work is going on right now on the Linux machine, and how " +
			"long each has been running. Answers 'what is running', 'وش شغال', 'did it finish', " +
			"'هل خلص', 'is the render done'.",
		Access:  Read,
		Timeout: 5 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			// Top processes by CPU, which is what a long job looks like from outside.
			out, err := exec.CommandContext(ctx, "ps", "-eo", "etimes=,pcpu=,comm=", "--sort=-pcpu").Output()
			if err != nil {
				return "", fmt.Errorf("the process list could not be read: %w", err)
			}
			type job struct {
				name string
				secs int
				cpu  float64
			}
			var busy []job
			sc := bufio.NewScanner(strings.NewReader(string(out)))
			for sc.Scan() {
				f := strings.Fields(sc.Text())
				if len(f) < 3 {
					continue
				}
				secs, _ := strconv.Atoi(f[0])
				cpu, _ := strconv.ParseFloat(f[1], 64)
				name := f[2]
				// A job worth mentioning is one that is both working and has been for a while.
				if cpu >= 20 && secs >= 60 {
					busy = append(busy, job{name, secs, cpu})
				}
			}
			sort.Slice(busy, func(i, j int) bool { return busy[i].cpu > busy[j].cpu })
			if len(busy) > 3 {
				busy = busy[:3]
			}

			var said []string
			for _, b := range busy {
				said = append(said, fmt.Sprintf("%s at %.0f%% for %s", b.name, b.cpu, humanDuration(time.Duration(b.secs)*time.Second)))
			}
			// Named services he cares about, whether or not they are burning CPU.
			for _, svc := range watch {
				if st, since, ok := serviceState(ctx, svc); ok {
					said = append(said, fmt.Sprintf("%s is %s%s", svc, st, since))
				}
			}
			if len(said) == 0 {
				// Not "the machine is idle", and not "it finished". This looked for processes over
				// twenty percent CPU for over a minute, and a render waiting on a disk, a job
				// sleeping between steps, and a finished job all look identical from here.
				return "Nothing is using much processor right now. That is not the same as finished.", nil
			}
			return strings.Join(said, ", ") + ".", nil
		},
	}
}

// HostLogs reports whether anything has gone wrong lately, which is the other half of the question.
func HostLogs(services []string) *Tool {
	allowed := map[string]bool{}
	for _, s := range services {
		allowed[s] = true
	}
	return &Tool{
		Name: "host_logs",
		Description: "Recent errors or warnings from a named service on the Linux machine. " +
			"Answers 'did anything go wrong', 'في أخطاء', 'why did it fail'. The service must be " +
			"one of: " + strings.Join(services, ", "),
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{
					"type":        "string",
					"description": "which service to look at",
					"enum":        services,
				},
			},
			"required": []string{"service"},
		},
		Access:  Read,
		Timeout: 6 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			svc, _ := args["service"].(string)
			// An allowlist, not a filter: an unnamed service is not read at all.
			if !allowed[svc] {
				return "", fmt.Errorf("I can only look at these services: %s", strings.Join(services, ", "))
			}
			out, err := exec.CommandContext(ctx, "journalctl", "--user", "-u", svc,
				"-p", "warning", "-n", "20", "--no-pager", "-o", "cat").Output()
			if err != nil {
				return "", fmt.Errorf("the log for %s could not be read: %w", svc, err)
			}
			lines := nonEmptyLines(string(out))
			if len(lines) == 0 {
				return fmt.Sprintf("No warnings or errors from %s.", svc), nil
			}
			last := lines[len(lines)-1]
			if len(last) > 200 {
				last = last[:200] + "..."
			}
			// "The last 20 lines" is not "recent" and a line is not a warning: one stack trace is
			// twenty lines and one entry. Say what was actually counted.
			return fmt.Sprintf("The last %d log lines from %s are warnings or worse, with no time "+
				"limit on how old they are. The latest: %s", len(lines), svc, last), nil
		},
	}
}

func readFirstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(string(b), '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func memInfo() (totalKB, availKB int, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
	}
	return totalKB, availKB, totalKB > 0
}

// diskFreeGB takes the context because the registry's timeout can only abandon a goroutine, not
// stop it. A context-free subprocess here survives the timeout with its pipes and its worker, and
// repeating the question accumulates them.
func diskFreeGB(ctx context.Context, path string) (float64, bool) {
	out, err := exec.CommandContext(ctx, "df", "-BG", "--output=avail", path).Output()
	if err != nil {
		return 0, false
	}
	lines := nonEmptyLines(string(out))
	if len(lines) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(lines[1]), "G"), 64)
	return v, err == nil
}

func serviceState(ctx context.Context, unit string) (state, since string, ok bool) {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", unit,
		"-p", "ActiveState", "-p", "ActiveEnterTimestamp", "--value").Output()
	if err != nil {
		return "", "", false
	}
	lines := nonEmptyLines(string(out))
	if len(lines) == 0 {
		return "", "", false
	}
	state = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", strings.TrimSpace(lines[1])); err == nil {
			since = " for " + humanDuration(time.Since(t))
		}
	}
	return state, since, state != ""
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// humanDuration says a length of time the way a person would, because it is going to be spoken.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.0f seconds", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	default:
		return fmt.Sprintf("%.0f days", d.Hours()/24)
	}
}
