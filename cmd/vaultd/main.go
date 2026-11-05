// vaultd serves the Obsidian vault to the assistant, on the machine that actually holds it.
//
// The bridge runs on a Linux box. The vault is 36 MB of markdown on a Windows machine on the same
// LAN. They are different computers with no share between them, so the vault is
// reached over the LAN through this, and only through this.
//
// It serves exactly three things and no more:
//
//	GET  /health          how this machine is doing, one JSON object
//	GET  /search?q=...    matching lines, with the file and when it last changed
//	POST /append {text}   one line added to today's inbox note
//
// There is no delete, no overwrite, no rename, no move, and no way to name a path. Append writes to
// one file whose name is derived from the date, and search only ever reads. The reason is that
// everything here is ultimately triggered by a sentence spoken aloud in a room, where the speaker
// might be a visitor, a child, or the television.
//
// Usage:
//
//	vaultd --vault "C:\path\to\vault" --listen 0.0.0.0:8791 --key-file <file>
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// What one search is allowed to cost. A question asked out loud can be repeated as fast as someone
// can speak, and a scan with no ceiling is a way to make this machine unusable by talking.
const (
	maxScanFiles = 20000
	maxScanBytes = 256 << 20 // the vault is 36 MB; this is a ceiling, not a target
	maxFileBytes = 4 << 20   // no note a person wrote is bigger than this
	maxSnippet   = 300       // the answer is spoken; a whole matching line can be a megabyte
)

type hit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
	Updated string `json:"updated"`
}

func main() {
	var (
		vault   = flag.String("vault", os.ExpandEnv("$USERPROFILE\\ObsidianVault"), "the vault directory")
		listen  = flag.String("listen", "0.0.0.0:8791", "address to serve on; the LAN, not the internet")
		keyFile = flag.String("key-file", "", "file holding the shared secret; required")
		inbox   = flag.String("inbox", "00-inbox", "folder inside the vault where spoken notes land")
		maxHits = flag.Int("max-hits", 5, "how many matches to return")
	)
	flag.Parse()

	if *keyFile == "" {
		fmt.Fprintln(os.Stderr, "vaultd: --key-file is required; an open endpoint into his notes is not acceptable")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaultd: key file:", err)
		os.Exit(2)
	}
	key := strings.TrimSpace(string(raw))
	if len(key) < 16 {
		fmt.Fprintln(os.Stderr, "vaultd: the key is too short to be a secret")
		os.Exit(2)
	}
	if st, err := os.Stat(*vault); err != nil || !st.IsDir() {
		fmt.Fprintln(os.Stderr, "vaultd: vault directory not found:", *vault)
		os.Exit(2)
	}
	// The inbox is one folder name, not a path. An operator-supplied "..\\..\\Desktop" would be as
	// effective as any request-supplied traversal, and there is no reason to allow it.
	if *inbox == "" || strings.ContainsAny(*inbox, `/\`) || strings.Contains(*inbox, "..") {
		fmt.Fprintln(os.Stderr, "vaultd: --inbox must be a single folder name inside the vault")
		os.Exit(2)
	}

	mux := http.NewServeMux()

	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// A constant-time compare is overkill for a LAN secret, but the habit costs nothing.
			if subtleEqual(r.Header.Get("X-Vault-Key"), key) {
				next(w, r)
				return
			}
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}

	mux.HandleFunc("/health", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"machine": hostname(),
			"os":      runtime.GOOS,
			"cpus":    runtime.NumCPU(),
			"time":    time.Now().Format(time.RFC3339),
			"summary": fmt.Sprintf("%s is up, %d cores.", hostname(), runtime.NumCPU()),
		})
	}))

	mux.HandleFunc("/search", guard(func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			writeJSON(w, []hit{})
			return
		}
		hits, err := search(r.Context(), *vault, q, *maxHits)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, hits)
	}))

	mux.HandleFunc("/append", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "post only", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body) != nil ||
			strings.TrimSpace(body.Text) == "" {
			http.Error(w, "nothing to write", http.StatusBadRequest)
			return
		}
		rel, err := appendNote(*vault, *inbox, body.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"path": rel})
	}))

	log.Printf("vaultd serving %s on %s", *vault, *listen)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// search walks the vault for markdown containing q, case-insensitively. 36 MB of text is small
// enough that an index would be a system to maintain rather than a speed-up worth having.
//
// It stops when the caller stops caring. Without that, a question asked and abandoned leaves a
// whole-vault scan running, and questions can be asked faster than scans finish.
//
// It also follows no symbolic link, ever. A junction inside the vault pointing at C:\Users would
// otherwise make every file on the machine searchable by voice, and Go 1.22 has no os.Root to lean
// on, so the rule is the blunt one: if it is not a real directory or a real file, it does not exist.
func search(ctx context.Context, root, q string, max int) ([]hit, error) {
	needle := strings.ToLower(q)
	type scored struct {
		hit
		mod time.Time
	}
	var found []scored
	var files, bytes int

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the vault is not a reason to fail the question
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if files > maxScanFiles || bytes > maxScanBytes {
			return filepath.SkipAll
		}
		if d.IsDir() {
			// Obsidian's own state and anything hidden is not notes.
			if name := d.Name(); strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Not a regular file means a link, a device or a junction. WalkDir reports the entry
		// without following it, so this is the point where the decision is still ours.
		if !d.Type().IsRegular() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > maxFileBytes {
			return nil
		}
		files++
		// No early stop. Stopping the walk and then sorting by modification time would rank only
		// whatever the directory order happened to reach first, and "where did I put that" is
		// exactly the question where the answer is often in the corner not yet visited. 36 MB of
		// markdown is a fraction of a second to read.
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		bytes += len(b)
		text := string(b)
		if !strings.Contains(strings.ToLower(text), needle) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		info, _ := d.Info()
		var mod time.Time
		if info != nil {
			mod = info.ModTime()
		}
		for i, line := range strings.Split(text, "\n") {
			if strings.Contains(strings.ToLower(line), needle) {
				// The snippet is bounded HERE, at the server. A note can hold one line a megabyte
				// long, and the client reads a capped number of bytes, so an unbounded line there
				// is a truncated JSON body that fails to decode rather than a long answer.
				snip := strings.TrimSpace(line)
				// by runes: his notes are Arabic, two bytes a letter, and a byte cut inside one
				// puts invalid UTF-8 into the JSON the bridge decodes
				if r := []rune(snip); len(r) > maxSnippet {
					snip = strings.TrimSpace(string(r[:maxSnippet])) + "..."
				}
				found = append(found, scored{
					hit: hit{
						Path:    filepath.ToSlash(rel),
						Line:    i + 1,
						Snippet: snip,
						Updated: humanAgo(mod),
					},
					mod: mod,
				})
				break // one line per file keeps the spoken answer short
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The most recently edited note is almost always the one he means.
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	out := make([]hit, 0, max)
	for i, f := range found {
		if i >= max {
			break
		}
		out = append(out, f.hit)
	}
	return out, nil
}

// appendNote adds one line to today's inbox note, creating it if it does not exist. It cannot touch
// any other file, because the name is built from the date and nothing in the request.
func appendNote(root, inbox, text string) (string, error) {
	dir := filepath.Join(root, inbox)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// A date-generated filename stops the REQUEST from naming a path. It does not stop the
	// FILESYSTEM from redirecting one: an inbox that is a junction, or today's note that is a link,
	// sends the append wherever the link points. The name being safe is not the same as the write
	// being confined, so the confinement is checked here rather than assumed.
	if err := insideRoot(root, dir); err != nil {
		return "", err
	}
	name := "voice-" + time.Now().Format("2006-01-02") + ".md"
	full := filepath.Join(dir, name)
	if st, err := os.Lstat(full); err == nil && !st.Mode().IsRegular() {
		return "", fmt.Errorf("today's note is not a regular file; refusing to write through it")
	}

	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		fmt.Fprintf(f, "# Spoken notes, %s\n\n", time.Now().Format("2006-01-02"))
	}
	line := strings.Join(strings.Fields(text), " ")
	if _, err := fmt.Fprintf(f, "- %s — %s\n", time.Now().Format("15:04"), line); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(inbox, name)), nil
}

// insideRoot resolves every link on both paths and checks that one really is under the other. It is
// the check that os.Root would make for us on a newer toolchain; this host is on Go 1.22.
func insideRoot(root, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q resolves outside the vault", path)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("vaultd: response encode failed: %v", err)
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "this machine"
	}
	return h
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%.0f minutes ago", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0f hours ago", d.Hours())
	default:
		return fmt.Sprintf("%.0f days ago", d.Hours()/24)
	}
}

// subtleEqual compares without leaking length through timing more than it has to.
//
// This was handwritten once and was wrong: `for i := range a` over a STRING yields the byte offsets
// where runes START, so every continuation byte of a multi-byte character went uncompared.
// "éééééééé" and "êêêêêêêê" are different sixteen-byte secrets that the old loop called equal, and
// both passed the length check. Handwritten authentication code bought an authentication bug, so it
// is gone; the standard library has done this correctly for a decade.
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
