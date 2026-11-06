package tools

// Things on the home network.
//
// The bridge sits on the same LAN as everything else he owns, so anything already serving HTTP in
// the house is one request away. What makes that safe rather than alarming is that the assistant
// cannot choose where to send the request: each endpoint is registered by name, with its URL fixed
// in configuration, and the spoken sentence selects a name and nothing else.
//
// The alternative, a tool that takes a URL, would be a browser operated by anyone in the room. It is
// deliberately not offered.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Endpoint is one named thing on the network the assistant may ask about.
type Endpoint struct {
	Name string // what it is called out loud
	URL  string // fixed here, never chosen by the model
	// Says turns the response body into one spoken sentence. Nil reads a short body as-is.
	Says func(body []byte) string
}

// HomeNetwork registers one read-only tool over a fixed set of named endpoints.
func HomeNetwork(endpoints []Endpoint, client *http.Client) *Tool {
	if client == nil {
		// No redirects. The whole point of this tool is that the model picks a NAME and the URL is
		// fixed in configuration; a redirect would let the named service decide where the request
		// actually goes, which hands the choice back to somebody outside this file.
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: noRedirect}
	}
	copyClient := *client
	copyClient.CheckRedirect = noRedirect
	client = &copyClient
	byName := map[string]Endpoint{}
	names := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		byName[e.Name] = e
		names = append(names, e.Name)
	}
	sort.Strings(names)

	return &Tool{
		Name: "home_network",
		Description: "Ask something on the home network for its status. Available: " +
			strings.Join(names, ", ") + ". Answers questions about those things by name.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"what": map[string]any{
					"type":        "string",
					"description": "which thing on the network to ask",
					"enum":        names,
				},
			},
			"required": []string{"what"},
		},
		Access:  Read,
		Timeout: 8 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			what, _ := args["what"].(string)
			ep, ok := byName[what]
			if !ok {
				return "", fmt.Errorf("can only ask about: %s", strings.Join(names, ", "))
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL, nil)
			if err != nil {
				return "", err
			}
			res, err := client.Do(req)
			if err != nil {
				return fmt.Sprintf("%s did not answer.", what), nil
			}
			defer res.Body.Close()
			body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
			if err != nil {
				return "", fmt.Errorf("%s response could not be read: %w", what, err)
			}
			if res.StatusCode >= 300 {
				return fmt.Sprintf("%s answered with an error, status %d.", what, res.StatusCode), nil
			}
			if ep.Says != nil {
				return ep.Says(body), nil
			}
			return oneLine(string(body), 300), nil
		},
	}
}

// oneLine flattens a response into something speakable and short.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "..."
	}
	if s == "" {
		return "It answered with nothing."
	}
	return s
}

// JSONField builds a Says function that reads one field out of a JSON body, which covers most small
// home services without needing code per service.
func JSONField(field, sentence string) func([]byte) string {
	return func(body []byte) string {
		var m map[string]any
		if json.Unmarshal(body, &m) != nil {
			return oneLine(string(body), 200)
		}
		v, ok := m[field]
		if !ok {
			return oneLine(string(body), 200)
		}
		return fmt.Sprintf(sentence, v)
	}
}
