// Package tools is what the assistant can reach besides its own memory.
//
// Every tool here is reached by a sentence spoken out loud in a room, which means it is reachable
// by a visitor, by a child, and by the television. That single fact decides the design:
//
//   - The registry is an ALLOWLIST. A tool that is not registered cannot be called, and there is no
//     general "run this" tool, because a general one is every tool at once.
//   - Each tool declares whether it READS or WRITES. Reads run immediately. Writes are reported to
//     the caller as needing a spoken confirmation, and the caller decides.
//   - Nothing destructive is here at all. Not guarded, not confirmed: absent. A prompt is a
//     preference and a confirmation stops accidents, so neither is a place to put the only thing
//     standing between a sentence and a deleted vault.
//
// Every tool also has to finish fast. The assistant is silent while one runs, that silence is billed
// at five cents a minute, and a person notices it at about a second.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Access says what a tool is allowed to do. It is not advice; the registry enforces it.
type Access int

const (
	// Read cannot change anything, so it needs no confirmation and cannot be regretted.
	Read Access = iota
	// Write changes something the owner would want to know about. The caller must confirm before
	// running it, and the tool must be able to say what it is about to do.
	Write
)

func (a Access) String() string {
	if a == Write {
		return "write"
	}
	return "read"
}

// Tool is one thing the assistant can do.
type Tool struct {
	Name string
	// Description is read by the model, so it is written for the model: what the tool answers, in
	// the words someone would actually use out loud.
	Description string
	// Params is a JSON Schema object for the arguments. Nil means the tool takes none.
	Params map[string]any
	Access Access
	// Ambient marks a tool that is cheap, local, takes no arguments, and answers something he asks
	// often. These are run BEFORE the model is asked anything and their answers are handed to it up
	// front, which removes a whole model round trip from the questions he actually asks: the first
	// call used to exist only so the model could say which tool it wanted.
	//
	// Only put a tool here if it is fast and free. It runs on every delegation, including the ones
	// that turn out to be about the capital of France.
	Ambient bool
	// Timeout bounds the silence. Past it the tool is abandoned and the assistant says so.
	Timeout time.Duration
	// Run answers in ONE OR TWO SENTENCES, because the answer is spoken aloud and is capped at 500
	// tokens by the API that carries it. A tool that returns a table has not finished its job.
	Run func(ctx context.Context, args map[string]any) (string, error)
}

// Registry holds the tools this assistant may use.
type Registry struct {
	mu sync.RWMutex
	m  map[string]*Tool
}

func NewRegistry() *Registry { return &Registry{m: map[string]*Tool{}} }

// Add registers a tool. A duplicate name is a programming error and panics, because two tools
// answering to one spoken name is exactly the ambiguity this package exists to avoid.
func (r *Registry) Add(t *Tool) {
	if t == nil || t.Name == "" || t.Run == nil {
		panic("tools: a tool needs a name and something to run")
	}
	if t.Timeout <= 0 {
		t.Timeout = 5 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[t.Name]; dup {
		panic("tools: duplicate tool " + t.Name)
	}
	r.m[t.Name] = t
}

// Get returns a tool by name. Anything not registered simply does not exist.
func (r *Registry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.m[name]
	return t, ok
}

// Names lists what is registered, sorted so logs and tests are stable.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Schemas renders the registry in the shape the Responses API wants for function tools.
func (r *Registry) Schemas() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		t := r.m[n]
		params := t.Params
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  params,
		})
	}
	return out
}

// Ambient runs every ambient tool at once and returns what they said, as lines the model reads
// before it is asked anything. A tool that fails is simply absent: this is a head start, not a
// dependency, and one broken sensor must not fail the question.
func (r *Registry) Ambient(ctx context.Context) string {
	r.mu.RLock()
	var picked []*Tool
	for _, t := range r.m {
		if t.Ambient {
			picked = append(picked, t)
		}
	}
	r.mu.RUnlock()
	if len(picked) == 0 {
		return ""
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].Name < picked[j].Name })

	type said struct {
		name, text string
	}
	out := make(chan said, len(picked))
	for _, t := range picked {
		go func(t *Tool) {
			s, err := r.Call(ctx, t.Name, "", false)
			if err != nil {
				out <- said{t.Name, ""}
				return
			}
			out <- said{t.Name, s}
		}(t)
	}
	got := map[string]string{}
	for range picked {
		s := <-out
		if s.text != "" {
			got[s.name] = s.text
		}
	}
	var b strings.Builder
	for _, t := range picked {
		if s := got[t.Name]; s != "" {
			fmt.Fprintf(&b, "%s: %s\n", t.Name, s)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "What is true right now, already looked up so you do not have to ask for it:\n" + b.String()
}

// ErrNeedsConfirmation is returned when a write tool is called without the caller having confirmed.
//
// It carries the EXACT call that was refused, arguments included, not just a description. The
// caller has to be able to run that one call and nothing else when it hears a yes; asking the model
// again with permission already granted lets it come back with a different write, or three.
type ErrNeedsConfirmation struct {
	Tool string
	Args string // raw JSON, exactly as the model asked for it
	What string // the same thing in a sentence, to be said out loud
}

func (e *ErrNeedsConfirmation) Error() string {
	return fmt.Sprintf("%s needs a spoken confirmation: %s", e.Tool, e.What)
}

// Call runs a registered tool. confirmed says the owner has agreed out loud to a write; it is
// ignored for reads, which never need it.
func (r *Registry) Call(ctx context.Context, name string, rawArgs string, confirmed bool) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		// Naming an unregistered tool is not an error to recover from quietly: it means the model
		// invented one, and the answer has to say so rather than pretend.
		return "", fmt.Errorf("no such tool %q; this assistant has: %v", name, r.Names())
	}
	args := map[string]any{}
	if rawArgs != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return "", fmt.Errorf("%s: arguments were not valid JSON: %w", name, err)
		}
	}
	if t.Access == Write && !confirmed {
		return "", &ErrNeedsConfirmation{Tool: name, Args: rawArgs, What: describe(name, args)}
	}

	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	done := make(chan struct{})
	var out string
	var err error
	go func() {
		defer close(done)
		defer func() {
			// A tool must not be able to take the assistant down mid-call.
			if p := recover(); p != nil {
				err = fmt.Errorf("%s panicked: %v", name, p)
			}
		}()
		out, err = t.Run(ctx, args)
	}()

	select {
	case <-done:
		return out, err
	case <-ctx.Done():
		return "", fmt.Errorf("%s took longer than %s and was abandoned", name, t.Timeout)
	}
}

// describe turns a pending write into a sentence a person can agree or object to.
func describe(name string, args map[string]any) string {
	if len(args) == 0 {
		return name
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := name + " with "
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s=%v", k, args[k])
	}
	return s
}
