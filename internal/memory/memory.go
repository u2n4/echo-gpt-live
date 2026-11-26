// Package memory is what the assistant knows about the household between calls.
//
// Two-layer design:
//
//   - Layer 1 (active profile: memory.md): up to maxFacts (80) concise, categorized facts that
//     ride in every call's prompt at 0 ms cost (family names, relationships, preferences, health,
//     item locations, important dates).
//   - Layer 2 (long-term archive: memory_archive.jsonl): an unbounded append-only journal in the
//     same directory. Every remembered fact and every reflection note is archived with its date
//     and category so older details are never lost when the active list reaches maxFacts, and can
//     be retrieved on demand via Search with Arabic normalization.
package memory

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// maxFacts bounds the active profile injected into every prompt. 80 short Arabic/English facts
// is ~1.5-2.5k tokens: generous enough for a whole family's names, preferences and routines while
// keeping live-call attention sharp. Anything older stays searchable in the archive.
const maxFacts = 80

// maxFactLen keeps one fact to something a person would actually say.
const maxFactLen = 220

type archiveEntry struct {
	At       string `json:"at"`
	Category string `json:"category,omitempty"`
	Text     string `json:"text"`
}

// Profile holds the active household memory (memory.md) and the searchable long-term archive
// (memory_archive.jsonl).
type Profile struct {
	mu          sync.Mutex
	fileMu      sync.Mutex // serializes atomic tmp+rename disk writes across concurrent tool calls
	path        string
	archivePath string
	facts       []string
	archive     []archiveEntry
}

// Open loads the active profile and its sibling long-term archive, creating nothing until there
// is something to write. Missing files are an empty memory, not an error.
func Open(path string) (*Profile, error) {
	p := &Profile{
		path:        path,
		archivePath: archivePathFor(path),
	}
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			p.facts = append(p.facts, line)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if p.archivePath != "" {
		if f, err := os.Open(p.archivePath); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 4096), 64<<10)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				var e archiveEntry
				if json.Unmarshal([]byte(line), &e) == nil && strings.TrimSpace(e.Text) != "" {
					p.archive = append(p.archive, e)
				}
			}
			_ = f.Close()
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return p, nil
}

func archivePathFor(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem = "memory"
	}
	return filepath.Join(dir, stem+"_archive.jsonl")
}

// Text renders the active profile for a prompt. Empty when nothing is saved yet.
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
	b.WriteString("الذاكرة النشطة لأهل البيت (الأسماء، الأقارب، التفضيلات، أماكن الأشياء، الصحة). استعملها بطبيعية بدون ما تعلن إنك تقرأ من ملف، وإذا ناقضها كلام قيل الحين فكلام الحين أصح:\n")
	for _, f := range p.facts {
		b.WriteString("- " + f + "\n")
	}
	return b.String()
}

// Facts returns a copy of the active facts.
func (p *Profile) Facts() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.facts...)
}

// ArchiveCount reports how many entries are stored in the long-term archive.
func (p *Profile) ArchiveCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.archive)
}

// Remember adds one fact to both the active profile and the long-term archive.
func (p *Profile) Remember(fact string) error {
	return p.RememberCategorized("", fact)
}

// RememberCategorized adds a fact with an optional category label to both the active profile and
// the long-term archive. Duplicates (after Arabic normalization) are skipped.
func (p *Profile) RememberCategorized(category, fact string) error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	fact = strings.Join(strings.Fields(fact), " ")
	if fact == "" {
		return fmt.Errorf("nothing to remember")
	}
	catLabel := categoryLabel(category)
	if catLabel != "" && !strings.HasPrefix(fact, "[") {
		fact = "[" + catLabel + "] " + fact
	}
	fact = clip(fact)
	normNew := normArabic(fact)

	p.mu.Lock()
	dupActive := false
	for _, f := range p.facts {
		if normArabic(f) == normNew {
			dupActive = true
			break
		}
	}
	if !dupActive {
		p.facts = append(p.facts, fact)
		if len(p.facts) > maxFacts {
			// Ensure anything evicted from the active window is preserved in the archive.
			for _, evicted := range p.facts[:len(p.facts)-maxFacts] {
				p.appendArchiveLocked("", evicted)
			}
			p.facts = p.facts[len(p.facts)-maxFacts:]
		}
	}
	archived := p.appendArchiveLocked(catLabel, fact)
	p.mu.Unlock()

	if err := p.save(); err != nil {
		return err
	}
	if archived {
		return p.saveArchive()
	}
	return nil
}

// Archive appends extra notes to the long-term archive without adding them to the active prompt.
func (p *Profile) Archive(notes []string) error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	p.mu.Lock()
	changed := false
	for _, n := range notes {
		n = clip(strings.Join(strings.Fields(n), " "))
		if n == "" {
			continue
		}
		if p.appendArchiveLocked("", n) {
			changed = true
		}
	}
	p.mu.Unlock()
	if !changed {
		return nil
	}
	return p.saveArchive()
}

func (p *Profile) appendArchiveLocked(category, text string) bool {
	norm := normArabic(text)
	if norm == "" {
		return false
	}
	for _, e := range p.archive {
		if normArabic(e.Text) == norm {
			return false
		}
	}
	p.archive = append(p.archive, archiveEntry{
		At:       time.Now().Format("2006-01-02"),
		Category: category,
		Text:     text,
	})
	return true
}

// Search searches both the active profile and the long-term archive using Arabic-normalized tokens.
// Returns up to limit distinct matching facts, ranked by token overlap and recency.
func (p *Profile) Search(query string, limit int) []string {
	if p == nil {
		return nil
	}
	if limit <= 0 {
		limit = 8
	}
	tokens := searchTokens(query)
	if len(tokens) == 0 {
		return nil
	}
	normQuery := normArabic(query)

	p.mu.Lock()
	active := append([]string(nil), p.facts...)
	arch := append([]archiveEntry(nil), p.archive...)
	p.mu.Unlock()

	type scored struct {
		text  string
		score int
		order int
	}
	seen := map[string]bool{}
	var hits []scored
	ord := 0

	consider := func(text string, bonus int) {
		ord++
		norm := normArabic(text)
		if norm == "" || seen[norm] {
			return
		}
		sc := 0
		if normQuery != "" && strings.Contains(norm, normQuery) {
			sc += 10
		}
		matchedTokens := 0
		for _, tok := range tokens {
			if strings.Contains(norm, tok) {
				matchedTokens++
				sc += 3
			}
		}
		if matchedTokens == 0 {
			return
		}
		if matchedTokens == len(tokens) {
			sc += 5
		}
		seen[norm] = true
		hits = append(hits, scored{text: text, score: sc + bonus, order: ord})
	}

	// Active facts first (slight priority for current facts).
	for i := len(active) - 1; i >= 0; i-- {
		consider(active[i], 2)
	}
	// Archive newest first.
	for i := len(arch) - 1; i >= 0; i-- {
		label := arch[i].Text
		if arch[i].At != "" {
			label = fmt.Sprintf("%s (%s)", arch[i].Text, arch[i].At)
		}
		consider(label, 0)
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].order < hits[j].order
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.text
	}
	return out
}

// ForgetMatching removes any fact matching all tokens of query from both the active profile and
// the long-term archive, returning the removed texts.
func (p *Profile) ForgetMatching(query string) ([]string, error) {
	if p == nil {
		return nil, fmt.Errorf("memory is off")
	}
	tokens := searchTokens(query)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("specify what to forget")
	}
	matches := func(s string) bool {
		wordSet := map[string]bool{}
		for _, w := range strings.Fields(normArabic(s)) {
			wordSet[w] = true
		}
		for _, tok := range tokens {
			if !wordSet[tok] {
				return false
			}
		}
		return true
	}

	p.mu.Lock()
	var removed []string
	seenRemoved := map[string]bool{}
	var keptFacts []string
	for _, f := range p.facts {
		if matches(f) {
			if k := normArabic(f); !seenRemoved[k] {
				seenRemoved[k] = true
				removed = append(removed, f)
			}
			continue
		}
		keptFacts = append(keptFacts, f)
	}
	var keptArch []archiveEntry
	for _, e := range p.archive {
		if matches(e.Text) {
			if k := normArabic(e.Text); !seenRemoved[k] {
				seenRemoved[k] = true
				removed = append(removed, e.Text)
			}
			continue
		}
		keptArch = append(keptArch, e)
	}
	if len(removed) == 0 {
		p.mu.Unlock()
		return nil, nil
	}
	p.facts = keptFacts
	p.archive = keptArch
	p.mu.Unlock()

	if err := p.save(); err != nil {
		return removed, err
	}
	if err := p.saveArchive(); err != nil {
		return removed, err
	}
	return removed, nil
}

// Replace swaps the whole active profile. Refuses an empty list.
func (p *Profile) Replace(facts []string) error {
	return p.ReplaceFrom(nil, facts)
}

// ReplaceFrom swaps the active profile for facts, keeping anything added since snapshot was taken
// and preserving every fact in the long-term archive.
func (p *Profile) ReplaceFrom(snapshot, facts []string) error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	var clean []string
	seen := map[string]bool{}
	for _, f := range facts {
		f = strings.Join(strings.Fields(f), " ")
		if f == "" {
			continue
		}
		f = clip(f)
		k := normArabic(f)
		if seen[k] {
			continue
		}
		seen[k] = true
		clean = append(clean, f)
	}
	if len(clean) == 0 {
		return fmt.Errorf("refusing to empty the profile")
	}
	p.mu.Lock()
	if snapshot != nil {
		was := map[string]bool{}
		for _, f := range snapshot {
			was[normArabic(f)] = true
		}
		now := map[string]bool{}
		for _, f := range clean {
			now[normArabic(f)] = true
		}
		for _, f := range p.facts {
			if k := normArabic(f); !was[k] && !now[k] {
				clean = append(clean, f)
				now[k] = true
			}
		}
	}
	// Archive all kept and any evicted facts so nothing is silently lost on cap overflow.
	archiveChanged := false
	for _, f := range clean {
		if p.appendArchiveLocked("", f) {
			archiveChanged = true
		}
	}
	if len(clean) > maxFacts {
		clean = clean[len(clean)-maxFacts:]
	}
	p.facts = clean
	p.mu.Unlock()

	if err := p.save(); err != nil {
		return err
	}
	if archiveChanged {
		return p.saveArchive()
	}
	return nil
}

// clip keeps a fact to maxFactLen CHARACTERS.
func clip(fact string) string {
	if r := []rune(fact); len(r) > maxFactLen {
		return string(r[:maxFactLen])
	}
	return fact
}

// Forget drops every active fact and clears the archive. Only ever called explicitly.
func (p *Profile) Forget() error {
	if p == nil {
		return fmt.Errorf("memory is off")
	}
	p.mu.Lock()
	p.facts = nil
	p.archive = nil
	p.mu.Unlock()
	if err := p.save(); err != nil {
		return err
	}
	return p.saveArchive()
}

func (p *Profile) save() error {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()

	p.mu.Lock()
	facts := append([]string(nil), p.facts...)
	path := p.path
	p.mu.Unlock()

	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Household memory (active profile). Plain text: read, edit, or delete any line.\n")
	b.WriteString("# Long-term searchable archive lives beside this file in memory_archive.jsonl.\n")
	b.WriteString("# Last written " + time.Now().Format(time.RFC3339) + "\n\n")
	for _, f := range facts {
		b.WriteString("- " + f + "\n")
	}
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p *Profile) saveArchive() error {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()

	p.mu.Lock()
	entries := append([]archiveEntry(nil), p.archive...)
	path := p.archivePath
	p.mu.Unlock()

	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func categoryLabel(cat string) string {
	switch strings.ToLower(strings.TrimSpace(cat)) {
	case "family_names", "family", "names", "أسماء وعائلة", "اسماء وعائله", "العائلة":
		return "أسماء وعائلة"
	case "preferences", "تفضيلات":
		return "تفضيلات"
	case "health", "medicine", "صحة وأدوية", "صحه وادويه", "الصحة":
		return "صحة وأدوية"
	case "locations", "places", "items", "أماكن وأشياء", "اماكن واشياء":
		return "أماكن وأشياء"
	case "dates", "events", "مواعيد ومناسبات":
		return "مواعيد ومناسبات"
	default:
		return ""
	}
}

// normArabic folds Arabic orthographic variants (alef forms, ة->ه, ى/ئ->ي, ؤ->و, harakat, tatweel)
// and punctuation so searches for "دكتور" or "فاطمة" match "فاطمه" or "الدكتور" reliably.
func normArabic(s string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0x064B && r <= 0x065F, r == 0x0640, r == 0x0670:
			continue
		case r == 'أ' || r == 'إ' || r == 'آ' || r == 'ٱ':
			r = 'ا'
		case r == 'ة':
			r = 'ه'
		case r == 'ى' || r == 'ئ':
			r = 'ي'
		case r == 'ؤ':
			r = 'و'
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

var arabicStopWords = map[string]bool{
	"في": true, "من": true, "على": true, "الي": true, "عن": true, "مع": true,
	"وش": true, "ايش": true, "ما": true, "هل": true, "وين": true, "متي": true,
	"اللي": true, "هذا": true, "هذه": true, "هو": true, "هي": true,
	"اسم": true, "تعرف": true, "تعرفين": true, "تتذكر": true, "تتذكرين": true,
	"the": true, "is": true, "what": true, "where": true, "who": true, "about": true,
}

func searchTokens(query string) []string {
	words := strings.Fields(normArabic(query))
	var out []string
	seen := map[string]bool{}
	for _, w := range words {
		if len([]rune(w)) < 2 {
			continue
		}
		if arabicStopWords[w] && len(words) > 1 {
			continue
		}
		// Strip leading Arabic definite article "ال" when the stem is at least 3 runes.
		stem := w
		if strings.HasPrefix(stem, "ال") && len([]rune(stem)) >= 5 {
			stem = strings.TrimPrefix(stem, "ال")
		}
		if !seen[stem] {
			seen[stem] = true
			out = append(out, stem)
		}
	}
	if len(out) == 0 && len(words) > 0 {
		return words
	}
	return out
}
