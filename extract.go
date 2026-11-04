package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// --- Redaction ---
//
// Runs only at ingest. The pull path can leak a repo-A secret into a repo-B
// session, so redact-in-place — but keep surrounding prose, which is the
// valuable context. Add a pattern, add a fixture in extract_test.go.

var redactionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]+`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]+`),
	regexp.MustCompile(`(?i)(postgres|mysql|mongodb)(\+srv)?://[^\s]*:[^\s]*@[^\s]+`),
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._-]{20,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

func redact(s string) string {
	for _, r := range redactionPatterns {
		s = r.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// --- Turn-fusing thresholds (tuned in Phase 2) ---

const (
	shortTurnChars = 40
	maxUnitChars   = 4000
	unsettleAge    = 24 * time.Hour
)

// approvals are bare user replies that, alone, carry no decision. We borrow
// the preceding assistant message so the *decision* they're approving survives.
var approvals = map[string]bool{
	"ok": true, "okay": true, "k": true, "yes": true, "y": true,
	"sure": true, "go": true, "go ahead": true, "continue": true,
	"done": true, "yep": true, "yeah": true, "please": true,
}

// noisePrefixes are harness-injected blocks we never want as the head of a
// fused unit.
var noisePrefixes = []string{
	"<command-name>",
	"<local-command",
	"<system-reminder>",
	"Caveat:",
}

func isNoise(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range noisePrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func isShortTurn(userText string, threshold int) bool {
	t := strings.TrimSpace(userText)
	if threshold <= 0 {
		threshold = shortTurnChars
	}
	if len([]rune(t)) < threshold {
		return true
	}
	return approvals[strings.ToLower(t)]
}

// --- JSONL parsing (defensive: unknown shapes are skipped, not fatal) ---

type rawEntry struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid"`
	Cwd       string          `json:"cwd"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"` // tool_use only
}

// userText extracts the textual content of a user message (string or block
// array), ignoring tool_result blocks — those are tool output, not user intent.
func userText(content json.RawMessage) (string, bool) {
	if len(content) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, true
	}
	var blocks []block
	if json.Unmarshal(content, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				sb.WriteString(b.Text)
				sb.WriteByte('\n')
			}
		}
		if sb.Len() == 0 {
			return "", false
		}
		return strings.TrimRight(sb.String(), "\n"), true
	}
	return "", false
}

// assistantParts returns the assistant's text + tool_use names (never tool
// output). Both may be empty.
func assistantParts(content json.RawMessage) (text string, tools []string) {
	if len(content) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, nil
	}
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return "", nil
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				sb.WriteString(b.Text)
				sb.WriteByte('\n')
			}
		case "tool_use":
			if b.Name != "" {
				tools = append(tools, b.Name)
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n"), tools
}

// --- unit + file extraction ---

type unit struct {
	ID   string
	Text string
	Cwd  string
	Repo string
	TS   int64
}

type entryView struct {
	uuid     string
	cwd      string
	ts       int64
	isUser   bool
	userText string
	asstText string
	tools    []string
}

// extractFile parses one session jsonl into fused units. sessionID is the
// filename stem; ids are sha256(sessionID + entryUUID). shortTurnN overrides
// the const threshold (<=0 means use the default) — the Phase 2 tuning knob.
func extractFile(path, sessionID string, shortTurnN int) ([]unit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	var views []entryView
	for {
		line, err := br.ReadBytes('\n')
		if len(line) != 0 {
			if trimmed := bytes.TrimSpace(line); len(trimmed) != 0 {
				if ev, ok := parseEntry(trimmed); ok {
					views = append(views, ev)
				}
			}
		}
		if err != nil {
			break
		}
	}

	// Group: one user entry + the following assistant entries until the next
	// user entry. Borrow the previous turn's final assistant message when the
	// user turn is too short to stand alone.
	var (
		units        []unit
		curUser      *entryView
		curSpan      []entryView
		prevAsstText string
	)
	flush := func() {
		if curUser == nil {
			return
		}
		var asst string
		var tools []string
		for _, a := range curSpan {
			if a.asstText != "" {
				asst += a.asstText + "\n"
			}
			tools = append(tools, a.tools...)
		}
		asst = strings.TrimRight(asst, "\n")

		fused := fuseTurn(curUser.userText, asst, tools)
		if isShortTurn(curUser.userText, shortTurnN) {
			if prevAsstText == "" {
				curUser, curSpan = nil, nil
				return
			}
			fused = "Assistant: " + prevAsstText + "\n\n" + fused
		}

		// cwd: prefer the user entry's, fall back to an assistant entry's.
		cwd := curUser.cwd
		if cwd == "" {
			for _, a := range curSpan {
				if a.cwd != "" {
					cwd = a.cwd
					break
				}
			}
		}

		units = append(units, unit{
			ID:   sha256Hex(sessionID + ":" + curUser.uuid),
			Text: redact(truncateRunes(fused, maxUnitChars)),
			Cwd:  cwd,
			Repo: repoOf(cwd),
			TS:   curUser.ts,
		})

		if asst != "" {
			prevAsstText = asst
		}
		curUser, curSpan = nil, nil
	}

	for i := range views {
		ev := views[i]
		if ev.isUser {
			flush()
			cp := ev
			curUser = &cp
		} else {
			if curUser != nil {
				curSpan = append(curSpan, ev)
			}
		}
	}
	flush()
	return units, nil
}

// parseEntry returns an entryView only for entries we keep (user text or
// assistant content). System/summary/etc. lines are dropped silently — see
// Risks: "unknown types are skipped, not fatal".
func parseEntry(line []byte) (entryView, bool) {
	var e rawEntry
	if json.Unmarshal(line, &e) != nil {
		return entryView{}, false
	}
	ev := entryView{
		uuid: e.UUID,
		cwd:  e.Cwd,
		ts:   parseUnixSeconds(e.Timestamp),
	}
	var msg rawMessage
	if len(e.Message) > 0 {
		_ = json.Unmarshal(e.Message, &msg)
	}
	switch e.Type {
	case "user":
		text, ok := userText(msg.Content)
		if !ok || strings.TrimSpace(text) == "" {
			return entryView{}, false
		}
		if isNoise(text) {
			return entryView{}, false
		}
		ev.isUser = true
		ev.userText = strings.TrimSpace(text)
		return ev, true
	case "assistant":
		text, tools := assistantParts(msg.Content)
		if text == "" && len(tools) == 0 {
			return entryView{}, false
		}
		ev.asstText = strings.TrimSpace(text)
		ev.tools = tools
		return ev, true
	default:
		return entryView{}, false
	}
}

// fuseTurn composes the canonical unit shape. Empty sections collapse.
func fuseTurn(user, asst string, tools []string) string {
	var sb strings.Builder
	sb.WriteString("User: ")
	sb.WriteString(user)
	if asst != "" {
		sb.WriteString("\nAssistant: ")
		sb.WriteString(asst)
	}
	if len(tools) > 0 {
		sb.WriteString("\nTools: ")
		sb.WriteString(strings.Join(dedupTools(tools), ", "))
	}
	return sb.String()
}

func dedupTools(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// --- file walk ---

// extractAll walks ~/.claude/projects/*/*.jsonl, applying the two mtime guards:
// skip files <24h old (unsettled), and skip files older than the
// last successful index unless force. Returns units in walk order.
// shortTurnN overrides the short-turn char threshold (Phase 2 knob).
func extractAll(force bool, lastIndex string, shortTurnN int) ([]unit, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".claude", "projects")
	if _, err := os.Stat(root); err != nil {
		return nil, nil // nothing to index yet — not an error
	}

	var last time.Time
	if lastIndex != "" {
		if t, err := time.Parse(time.RFC3339, lastIndex); err == nil {
			last = t
		}
	}
	now := time.Now()
	var units []unit
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort walk
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if now.Sub(info.ModTime()) < unsettleAge {
			return nil
		}
		if !force && !last.IsZero() && info.ModTime().Before(last) {
			return nil
		}
		sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		us, err := extractFile(path, sessionID, shortTurnN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: skip %s: %v\n", path, err)
			return nil
		}
		units = append(units, us...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return units, nil
}

// --- helpers ---

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// repoOf returns the last path segment of cwd. Cross-platform: filepath.Base
// handles both / and \ on Windows; on Linux only /, but a Windows path in cwd
// is unlikely.
func repoOf(cwd string) string {
	cwd = strings.TrimRight(cwd, `/\`)
	if cwd == "" {
		return ""
	}
	if i := strings.LastIndexAny(cwd, `/\`); i >= 0 {
		return cwd[i+1:]
	}
	return cwd
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// parseUnixSeconds accepts the common Claude Code timestamp formats. Returns 0
// if unparseable — search still works, only date display is affected.
func parseUnixSeconds(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}
