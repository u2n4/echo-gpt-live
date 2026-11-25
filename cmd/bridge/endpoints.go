package main

import (
	"strings"

	"github.com/u2n4/echo-gpt-live/internal/tools"
)

// parseEndpoints reads "name=url,name=url" into the fixed list the home-network tool works from.
//
// The URLs live here, in configuration, and never in anything the model says. A tool that accepted a
// URL would be a browser operated by whoever is standing in the room.
func parseEndpoints(s string) []tools.Endpoint {
	var out []tools.Endpoint
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			continue
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		out = append(out, tools.Endpoint{
			Name: name,
			URL:  url,
			Says: tools.JSONField("summary", "%v"),
		})
	}
	return out
}
