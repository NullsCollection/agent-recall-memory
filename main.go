package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// usage prints the CLI surface.
func usage() {
	fmt.Fprintln(os.Stderr, `memdb — agent memory

usage: memdb <command> [flags] -- <args>

  index   [--force] [--quiet] [--short-turn N]   walk sessions, embed, upsert (idempotent)
  add     [--cwd .] -- <text>                    push a manual/document memory
  search  [--top-k 5] [--cwd .] [--json] -- <query>
  reindex [--force]                              re-embed all stored text on the current model
  stats                                          row counts by repo / kind / model
  eval    [--golden golden.txt] [--top-k N] [--repo-boost F] [--query-prefix P]
                                                run golden queries, print hit/miss + rank

DB: ~/.claude/memdb.sqlite
embeddings: `+embeddingModel+` (`+fmt.Sprintf("%d", embeddingDim)+`-dim, via Ollama at `+ollamaURL+`)`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "index":
		err = cmdIndex(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "search":
		err = cmdSearch(os.Args[2:])
	case "reindex":
		err = cmdReindex(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "eval":
		err = cmdEval(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --- index ---

func cmdIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: memdb index [--force] [--quiet] [--short-turn N]")
	}
	force := fs.Bool("force", false, "re-index even files older than last_indexed")
	quiet := fs.Bool("quiet", false, "no stdout on success (for SessionStart hook)")
	shortTurn := fs.Int("short-turn", shortTurnChars, "char threshold below which a user turn borrows the prior assistant message")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	err = runIndex(db, *force, *shortTurn, *quiet)
	recordIndexRun(db, err)
	if err != nil && *quiet {
		// --quiet fails silently — output would
		// pollute every session and the next start retries. The error is kept
		// in meta so `memdb stats` can still surface it.
		return nil
	}
	return err
}

func runIndex(db *sql.DB, force bool, shortTurn int, quiet bool) error {
	start := time.Now()
	last, err := metaGet(db, metaLastIndexed)
	if err != nil {
		return err
	}
	units, err := extractAll(force, last, shortTurn)
	if err != nil {
		return err
	}

	// Skip units already stored with the same text, so a run killed partway
	// through a backlog resumes instead of re-embedding from scratch.
	var todo []unit
	for _, u := range units {
		same, err := storedUnchanged(db, u.ID, u.Text)
		if err != nil {
			return err
		}
		if !same {
			todo = append(todo, u)
		}
	}

	// Commit per batch: one failed batch keeps everything before it.
	for i := 0; i < len(todo); i += embedBatchSize {
		j := min(i+embedBatchSize, len(todo))
		if err := indexBatch(db, todo[i:j]); err != nil {
			return fmt.Errorf("batch at offset %d: %w", i, err)
		}
	}

	// Watermark is start-24h, not now: extractAll skipped files still inside
	// the unsettled window, and they must be picked up on a later run.
	cutoff := start.Add(-unsettleAge).UTC().Format(time.RFC3339)
	if err := metaSet(db, metaLastIndexed, cutoff); err != nil {
		return err
	}
	if !quiet {
		if len(todo) == 0 {
			fmt.Println("nothing to index")
		} else {
			fmt.Printf("indexed %d units (%d unchanged)\n", len(todo), len(units)-len(todo))
		}
	}
	return nil
}

// indexBatch embeds one batch and upserts it in its own transaction.
func indexBatch(db *sql.DB, us []unit) error {
	texts := make([]string, len(us))
	for i, u := range us {
		texts[i] = u.Text
	}
	vecs, err := embedBatch(texts)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for i, u := range us {
		m := memory{
			ID:     u.ID,
			Text:   u.Text,
			Vector: vecs[i],
			Cwd:    u.Cwd,
			Repo:   u.Repo,
			Kind:   "turn",
			TS:     u.TS,
		}
		if err := upsert(tx, m); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// recordIndexRun stamps each attempt and its outcome in meta, so a hook that
// fails silently on every session start still shows up in `memdb stats`.
func recordIndexRun(db *sql.DB, runErr error) {
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	_ = metaSet(db, metaLastAttempt, time.Now().UTC().Format(time.RFC3339))
	_ = metaSet(db, metaLastError, msg)
}

// --- add ---

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: memdb add [--cwd .] -- <text>") }
	cwd := fs.String("cwd", "", "working directory (defaults to $PWD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return fmt.Errorf("no text provided (use -- to separate flags from text)")
	}
	if *cwd == "" {
		if c, err := os.Getwd(); err == nil {
			*cwd = c
		}
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	clean := redact(truncateRunes(text, maxUnitChars))
	vec, err := embedOne(clean)
	if err != nil {
		return err
	}
	m := memory{
		ID:     "doc:" + sha256Hex(clean),
		Text:   clean,
		Vector: vec,
		Cwd:    *cwd,
		Repo:   repoOf(*cwd),
		Kind:   "doc",
		TS:     time.Now().Unix(),
	}
	if err := upsert(db, m); err != nil {
		return err
	}
	fmt.Println("added", m.ID)
	return nil
}

// --- search ---

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: memdb search [--top-k 5] [--cwd .] [--json] -- <query>")
	}
	topK := fs.Int("top-k", 5, "number of hits to return")
	cwd := fs.String("cwd", "", "working directory (defaults to $PWD, enables same-repo boost)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("no query provided")
	}
	if *cwd == "" {
		if c, err := os.Getwd(); err == nil {
			*cwd = c
		}
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	qv, err := embedQuery(query)
	if err != nil {
		return err
	}
	rows, err := loadAll(db, embeddingModel)
	if err != nil {
		return err
	}
	hits := searchRows(rows, qv, repoOf(*cwd), *topK, repoBoost)

	if *asJSON {
		return printJSON(hits)
	}
	if len(hits) == 0 {
		fmt.Println("no hits")
		return nil
	}
	for _, h := range hits {
		date := "-"
		if h.Mem.TS > 0 {
			date = time.Unix(h.Mem.TS, 0).UTC().Format("2006-01-02")
		}
		repo := h.Mem.Repo
		if repo == "" {
			repo = "(none)"
		}
		fmt.Printf("%s · %s · %s · %.4f\n", repo, h.Mem.Kind, date, h.Score)
		fmt.Println(indent(h.Mem.Text))
		fmt.Println()
	}
	return nil
}

func printJSON(hits []hit) error {
	type out struct {
		Repo  string  `json:"repo"`
		Kind  string  `json:"kind"`
		Text  string  `json:"text"`
		Score float64 `json:"score"`
		TS    int64   `json:"ts"`
	}
	o := make([]out, 0, len(hits))
	for _, h := range hits {
		o = append(o, out{
			Repo:  h.Mem.Repo,
			Kind:  h.Mem.Kind,
			Text:  h.Mem.Text,
			Score: float64(h.Score),
			TS:    h.Mem.TS,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(o)
}

// --- reindex ---

func cmdReindex(args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: memdb reindex [--force] (re-embeds rows not yet on the current model)")
	}
	force := fs.Bool("force", false, "re-embed even rows already on the current model")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	ids, texts, err := rowsNeedingEmbedding(db, *force)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("nothing to reindex")
		return nil
	}
	vecs, err := embedMany(texts)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for i, id := range ids {
		if err := setVector(tx, id, vecs[i]); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("reindexed %d rows on %s\n", len(ids), embeddingModel)
	return nil
}

// --- stats ---

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: memdb stats") }
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&total); err != nil {
		return err
	}
	fmt.Printf("total: %d\n", total)
	fmt.Println("model:", embeddingModel, fmt.Sprintf("(%d-dim)", embeddingDim))

	printGroup := func(title, q string, args ...any) {
		fmt.Println(title + ":")
		rows, err := db.Query(q, args...)
		if err != nil {
			fmt.Println("  (error:", err, ")")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				return
			}
			if k == "" {
				k = "(none)"
			}
			fmt.Printf("  %-32s %d\n", k, n)
		}
	}

	printGroup("by repo", `SELECT repo, COUNT(*) FROM memories GROUP BY repo ORDER BY COUNT(*) DESC, repo`)
	printGroup("by kind", `SELECT kind, COUNT(*) FROM memories GROUP BY kind ORDER BY COUNT(*) DESC`)
	printGroup("by model", `SELECT model, COUNT(*) FROM memories GROUP BY model ORDER BY COUNT(*) DESC`)

	last, _ := metaGet(db, metaLastIndexed)
	attempt, _ := metaGet(db, metaLastAttempt)
	lastErr, _ := metaGet(db, metaLastError)
	now := time.Now()
	fmt.Println("last_indexed:", describeAge(last, now))
	if attempt != "" {
		fmt.Println("last_attempt:", describeAge(attempt, now))
	}
	if lastErr != "" {
		fmt.Println("last_error:", lastErr)
	}
	if warn := indexHealth(last, lastErr, now); warn != "" {
		fmt.Println("WARNING:", warn)
	}
	return nil
}

// staleAfter is how old last_indexed may get before stats warns. Claude Code
// deletes transcripts after cleanupPeriodDays (default 30), so a stalled
// indexer has to be noticed well inside that window.
const staleAfter = 7 * 24 * time.Hour

// indexHealth returns a warning for stats, or "" when indexing looks healthy.
func indexHealth(lastIndexed, lastErr string, now time.Time) string {
	if lastErr != "" {
		return "last index run failed — run `memdb index` to see it live"
	}
	if lastIndexed == "" {
		return "never indexed — run `memdb index`"
	}
	t, err := time.Parse(time.RFC3339, lastIndexed)
	if err != nil {
		return ""
	}
	if age := now.Sub(t); age > staleAfter {
		return fmt.Sprintf("last_indexed is %d days old — Claude Code deletes transcripts after cleanupPeriodDays (default 30), unindexed ones are lost",
			int(age.Hours()/24))
	}
	return ""
}

// describeAge renders an RFC3339 meta timestamp with its age in days.
func describeAge(ts string, now time.Time) string {
	if ts == "" {
		return "(never)"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return fmt.Sprintf("%s (%d days ago)", ts, int(now.Sub(t).Hours()/24))
}

// --- eval ---
//
// Turns tuning into a number. Parses `query ||| expected-substring` lines from
// a golden file, runs each query, reports HIT/MISS + rank, and prints a summary
// plus the raw-cosine distribution of good hits — that distribution is the
// threshold signal for Phase 5's auto-inject.

type goldenItem struct {
	Query  string
	Expect string
}

// parseGolden reads one `query ||| expected-substring` per line. Blank lines
// and `#` comments are skipped.
func parseGolden(path string) ([]goldenItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []goldenItem
	for i, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		idx := strings.Index(s, "|||")
		if idx < 0 {
			return nil, fmt.Errorf("%s:%d: missing '|||' separator", path, i+1)
		}
		q := strings.TrimSpace(s[:idx])
		e := strings.TrimSpace(s[idx+3:])
		if q == "" || e == "" {
			return nil, fmt.Errorf("%s:%d: empty query or expectation", path, i+1)
		}
		items = append(items, goldenItem{Query: q, Expect: e})
	}
	return items, nil
}

// findRank returns the 1-indexed rank of the first hit whose Text contains
// `expect` (case-insensitive), or 0 if none in the slice.
func findRank(hits []hit, expect string) int {
	needle := strings.ToLower(expect)
	for i, h := range hits {
		if strings.Contains(strings.ToLower(h.Mem.Text), needle) {
			return i + 1
		}
	}
	return 0
}

func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: memdb eval [--golden golden.txt] [--top-k N] [--repo-boost F] [--query-prefix P] [--cwd .]")
	}
	goldenPath := fs.String("golden", "golden.txt", "golden queries file (lines of: query ||| expected-substring)")
	topK := fs.Int("top-k", 10, "search depth for evaluation")
	boost := fs.Float64("repo-boost", float64(repoBoost), "same-repo score multiplier (1.0 = off, default uses the const)")
	// Defaults to what `search` actually does, so a bare `memdb eval` measures
	// the shipped behaviour. Pass --query-prefix "" to measure without it.
	prefix := fs.String("query-prefix", queryPrefix, "string prepended to each query before embedding (qwen3 retrieval instruct)")
	cwd := fs.String("cwd", "", "working directory (defaults to $PWD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cwd == "" {
		if c, err := os.Getwd(); err == nil {
			*cwd = c
		}
	}
	cwdRepo := repoOf(*cwd)

	items, err := parseGolden(*goldenPath)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no golden queries in %s", *goldenPath)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := loadAll(db, embeddingModel)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no memories indexed yet — run `memdb index` first")
	}

	hits := 0
	var ranks []int
	var goodScores []float64 // raw (pre-boost) cosines of matched hits

	fmt.Printf("eval: %d queries | rows=%d | top-k=%d | repo-boost=%.2f | prefix=%q | cwd-repo=%q\n\n",
		len(items), len(rows), *topK, *boost, *prefix, cwdRepo)
	for i, it := range items {
		qv, err := embedOne(*prefix + it.Query)
		if err != nil {
			return err
		}
		results := searchRows(rows, qv, cwdRepo, *topK, float32(*boost))
		rank := findRank(results, it.Expect)
		if rank > 0 {
			hits++
			ranks = append(ranks, rank)
			goodScores = append(goodScores, float64(results[rank-1].Raw))
			fmt.Printf("[%2d/%d] HIT   rank=%d  raw=%.4f  q=%q\n", i+1, len(items), rank, results[rank-1].Raw, it.Query)
		} else {
			topScore := float32(0)
			if len(results) > 0 {
				topScore = results[0].Score
			}
			fmt.Printf("[%2d/%d] MISS  top=%.4f  q=%q  want~%q\n", i+1, len(items), topScore, it.Query, it.Expect)
		}
	}

	fmt.Println()
	fmt.Printf("summary: %d/%d hit (%.0f%%)", hits, len(items), 100*float64(hits)/float64(len(items)))
	if len(ranks) > 0 {
		sum := 0
		for _, r := range ranks {
			sum += r
		}
		fmt.Printf(", mean rank %.1f", float64(sum)/float64(len(ranks)))
	}
	fmt.Println()

	if len(goodScores) > 0 {
		sort.Float64s(goodScores)
		min := goodScores[0]
		max := goodScores[len(goodScores)-1]
		median := goodScores[len(goodScores)/2]
		fmt.Printf("good-hit raw cosine: min=%.4f median=%.4f max=%.4f  (Phase 5 threshold signal)\n",
			min, median, max)
	}
	return nil
}

// --- format helpers ---

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}
