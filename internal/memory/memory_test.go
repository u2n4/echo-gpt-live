package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProfileRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory.md")
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Text() != "" {
		t.Error("a new profile must say nothing rather than announce that it knows nothing")
	}
	if err := m.Remember("يشتغل على مساعد صوتي اسمه سول"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remember("يشتغل على مساعد صوتي اسمه سول"); err != nil {
		t.Fatal(err)
	}
	if len(m.Facts()) != 1 {
		t.Errorf("saying it twice does not make it two facts: %v", m.Facts())
	}

	again, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Facts()) != 1 || !strings.Contains(again.Facts()[0], "سول") {
		t.Errorf("did not survive a restart: %v", again.Facts())
	}
	if !strings.Contains(again.Text(), "سول") {
		t.Error("the prompt text should carry the fact")
	}
}

// A model that returns nothing must not be able to erase everything he told it.
// The after-the-call pass reads the profile, thinks, and writes back. A fact he asked for in
// between must survive; a fact the model dropped on purpose must not.
func TestReplaceFromKeepsWhatWasAddedMeanwhile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory.md")
	m, _ := Open(p)
	_ = m.Remember("he drinks his coffee black")
	_ = m.Remember("he works nights")
	snapshot := m.Facts()
	_ = m.Remember("his brother is called Hassan") // said while the model was thinking
	if err := m.ReplaceFrom(snapshot, []string{"he drinks his coffee black"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(m.Facts(), "|")
	if !strings.Contains(got, "Hassan") {
		t.Fatalf("the fact added meanwhile was erased: %q", got)
	}
	if strings.Contains(got, "nights") {
		t.Fatalf("the fact the model dropped came back: %q", got)
	}
}

// Facts are cut at 200 characters, not 200 bytes: an Arabic letter is two bytes and must not be
// split in half.
func TestFactsAreClippedByCharacter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory.md")
	m, _ := Open(p)
	long := strings.Repeat("ع", 250)
	_ = m.Remember(long)
	got := m.Facts()[0]
	if r := []rune(got); len(r) != maxFactLen || !utf8.ValidString(got) {
		t.Fatalf("clipped to %d runes, valid=%v", len(r), utf8.ValidString(got))
	}
}

func TestReplaceRefusesToEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory.md")
	m, _ := Open(p)
	_ = m.Remember("something worth keeping")
	if err := m.Replace(nil); err == nil {
		t.Fatal("an empty rewrite must be refused")
	}
	if err := m.Replace([]string{"", "   "}); err == nil {
		t.Fatal("a rewrite of nothing but blanks must be refused")
	}
	if len(m.Facts()) != 1 {
		t.Errorf("the profile was damaged by a refused write: %v", m.Facts())
	}
}

// Everything here rides in every prompt, so the cap is an attention limit, not a disk limit.
func TestProfileIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memory.md")
	m, _ := Open(p)
	for i := 0; i < maxFacts*3; i++ {
		if err := m.Remember(strings.Repeat("x", 400) + string(rune('a'+i%26)) + itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(m.Facts()); got > maxFacts {
		t.Errorf("%d facts, cap is %d", got, maxFacts)
	}
	for _, f := range m.Facts() {
		if len(f) > maxFactLen {
			t.Errorf("a fact is %d bytes, cap is %d", len(f), maxFactLen)
		}
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "memory.md")
	m, _ := Open(p)
	_ = m.Remember("one")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
