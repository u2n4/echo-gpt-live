// Package memory is what the assistant knows about him between calls.
//
// The shape is copied from what OpenAI moved ChatGPT to in 2026, because the reasoning behind it
// applies here even harder. ChatGPT's memory is NOT a vector search over old conversations: there
// is no database being queried when you ask a question. It is a small profile, synthesised in the
// background from past conversations, that is simply present in the context every time.
//
// Retrieval would be the obvious design and it is the wrong one here. He has already told me the
// assistant is too slow, and a search before every answer is another round trip on the critical
// path of a conversation that bills by the minute. A profile costs NOTHING at question time: it is
// a couple of hundred tokens of text that were going to be sent anyway.
//
// So:
//
//   - READING is free and instant. The profile is in the prompt before he speaks.
//   - WRITING happens AFTER the call has ended and the billing has stopped. A cheap model reads
//     what was said, decides what is worth keeping, and rewrites the profile. He is not waiting.
//   - It is a plain text file he can open, read, correct and delete. A memory he cannot see is a
//     memory he cannot disagree with.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxFacts bounds the profile. This is not a disk limit, it is an ATTENTION limit: everything here
// is in the prompt of every single call, so a profile that grows without end slowly drowns the
// question he just asked. Old facts have to lose to new ones.
const maxFacts = 40

// maxFactLen keeps one fact to something a person would actually say.
const maxFactLen = 200

// Profile is the small set of things worth remembering about him.
type Profile struct {
	mu    sync.Mutex
	path  string
	facts []string
}

// Open loads the profile, creating nothing until there is something to write. A missing file is an
// empty memory, not an error: the first call on a new machine must work.
func Open(path string) (*Profile, error) {
	p := &Profile{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p.facts = append(p.facts, line)
	}
	return p, nil
}

// Text renders the profile for a prompt. Empty when there is nothing worth saying, so a new
// assistant does not announce that it remembers nothing about him.
func (p *Profile) Text() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("ما تعرفه عنه من قبل. استعمله بدون ما تعلن إنك تتذكر، وإذا ناقض شي قاله الحين فالحين أصح:\n")
	for _, f := range p.facts {
		b.WriteString("- " + f + "\n")
	}
	return b.String()
}

// Facts returns a copy, for a model that is about to rewrite them.
func (p *Profile) Facts() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.facts...)
}

// Remember adds one fact he asked for by name. A duplicate is not added twice, and the newest fact
// wins the space when the profile is full.
func (p *Profile) Remember(fact string) error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	fact = strings.Join(strings.Fields(fact), " ")
	if fact == "" {
		return fmt.Errorf("nothing to remember")
	}
	fact = clip(fact)
	p.mu.Lock()
	for _, f := range p.facts {
		if strings.EqualFold(f, fact) {
			p.mu.Unlock()
			return nil // already known; saying it twice does not make it truer
		}
	}
	p.facts = append(p.facts, fact)
	if len(p.facts) > maxFacts {
		p.facts = p.facts[len(p.facts)-maxFacts:]
	}
	p.mu.Unlock()
	return p.save()
}

// Replace swaps the whole profile, which is what the after-the-call pass produces. It refuses an
// empty list: a model that returns nothing must not be able to erase everything he told it.
func (p *Profile) Replace(facts []string) error {
	return p.ReplaceFrom(nil, facts)
}

// ReplaceFrom swaps the profile for facts, keeping anything that was added SINCE snapshot was taken.
//
// The after-the-call pass reads the profile, thinks for a while, and writes back. If he said
// "remember that I..." on the next call while it was thinking, that fact was in neither its input
// nor its output, and Replace erased it. Codex found it. A fact present now that the snapshot did
// not have is such an addition and survives; a fact the snapshot had and the model dropped was
// dropped on purpose.
func (p *Profile) ReplaceFrom(snapshot, facts []string) error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	var clean []string
	for _, f := range facts {
		f = strings.Join(strings.Fields(f), " ")
		if f == "" {
			continue
		}
		clean = append(clean, clip(f))
	}
	if len(clean) == 0 {
		return fmt.Errorf("refusing to empty the profile")
	}
	p.mu.Lock()
	if snapshot != nil {
		was := map[string]bool{}
		for _, f := range snapshot {
			was[strings.ToLower(f)] = true
		}
		now := map[string]bool{}
		for _, f := range clean {
			now[strings.ToLower(f)] = true
		}
		for _, f := range p.facts {
			if k := strings.ToLower(f); !was[k] && !now[k] {
				clean = append(clean, f)
			}
		}
	}
	if len(clean) > maxFacts {
		clean = clean[len(clean)-maxFacts:]
	}
	p.facts = clean
	p.mu.Unlock()
	return p.save()
}

// clip keeps a fact to maxFactLen CHARACTERS. Cutting bytes split Arabic letters in half.
func clip(fact string) string {
	if r := []rune(fact); len(r) > maxFactLen {
		return string(r[:maxFactLen])
	}
	return fact
}

// Forget drops every fact. Only ever called because he asked.
func (p *Profile) Forget() error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	p.mu.Lock()
	p.facts = nil
	p.mu.Unlock()
	return p.save()
}

// save writes through a temporary file and a rename, so a crash mid-write leaves the previous
// profile intact rather than half a profile.
func (p *Profile) save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	facts := append([]string(nil), p.facts...)
	path := p.path

	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# What the assistant remembers about him.\n")
	b.WriteString("# Plain text on purpose: read it, correct it, delete a line. It is yours.\n")
	b.WriteString("# Last written " + time.Now().Format(time.RFC3339) + "\n\n")
	for _, f := range facts {
		b.WriteString("- " + f + "\n")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".memory-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
