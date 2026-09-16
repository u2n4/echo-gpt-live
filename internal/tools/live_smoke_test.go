package tools

import (
	"context"
	"os"
	"testing"
)

// A smoke test that runs the read-only tools against this actual machine. Skipped unless asked for,
// because its answers depend on what the machine is doing at the time.
func TestLiveReadOnlyToolsOnThisMachine(t *testing.T) {
	if os.Getenv("LIVE_TOOLS") == "" {
		t.Skip("set LIVE_TOOLS=1 to run the tools against this machine")
	}
	cases := []struct {
		tool *Tool
		args map[string]any
	}{
		{Spend(os.ExpandEnv("$HOME/.local/state/rtcdiag/spend.json"), 0.05, 2.0, nil), nil},
		{HostStatus(), nil},
		{HostJobs([]string{"bridge"}), nil},
		{HostLogs([]string{"bridge"}), map[string]any{"service": "bridge"}},
	}
	for _, c := range cases {
		out, err := c.tool.Run(context.Background(), c.args)
		if err != nil {
			t.Errorf("%-14s ERROR %v", c.tool.Name, err)
			continue
		}
		if out == "" {
			t.Errorf("%-14s said nothing", c.tool.Name)
		}
		t.Logf("%-14s %s", c.tool.Name, out)
	}
}
