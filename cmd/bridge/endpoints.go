package main

import (
	"net/url"
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
		name, address, ok := strings.Cut(part, "=")
		name, address = strings.TrimSpace(name), strings.TrimSpace(address)
		if !ok || name == "" || address == "" {
			continue
		}
		u, err := url.Parse(address)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		out = append(out, tools.Endpoint{
			Name: name,
			URL:  u.String(),
			Says: tools.JSONField("summary", "%v"),
		})
	}
	return out
}
