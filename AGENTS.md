# AGENTS.md

## Status

Code shipped — four Go source files, `go.mod`, passing tests. The core decisions are **locked**
(vector store, embedding model, semantic unit, search scope, push trigger, redaction). If you
disagree with a locked decision, surface it to the user rather than silently picking differently.

Phase gates (do not start Phase N+1 before N's gate passes):

- Phase 0/1 — MET. Host backfill produced 253 rows; cross-repo search returned recognisable hits.
- Phase 2 — `memdb eval` + golden harness implemented and unit-tested, but the actual **eval-and-tune
  pass is host-only and not yet run** (no baseline score recorded in `golden.txt`). Treat any recall
  claim as unverified until that pass happens. This is the gate Phase 5's auto-inject threshold
  depends on.
- Phase 3 — MET (cold session recalled a real past decision, no permission prompt).
- Phase 4 — code shipped (`--quiet`, `remember-this` skill); host install of the SessionStart hook +
  `memdb add` permission + the three host verifications remain.
- Phase 5 — deferred. Every item has an explicit trigger; do not build unless its trigger has fired.

## Dev commands

```bash
go install ./...          # canonical install — puts `memdb` on $GOPATH/bin (Windows: %USERPROFILE%\go\bin)
go test ./...             # all unit tests: redaction fixture, extraction e2e, eval parsing, cosine
go vet ./...              # clean
gofmt -l .                # should print nothing
```

The DB lives at `~/.claude/memdb.sqlite` (outside the repo) and is **auto-created on first run** by
`openDB` (mkdir + schema + PRAGMAs). The store opens in WAL mode, so `-wal` and `-shm` sidecars
appear next to it. `.gitignore` covers `/memdb`, `/memdb.exe`, `*.sqlite` — never commit a database.

## Hard constraints (these will be violated by default)

- **One dependency only:** `modernc.org/sqlite` (pure Go, no cgo). No other SQLite driver, no ORM.
- **Four source files, hard limit.** Resist adding a fifth. Map all work to exactly one
  of `main.go` (CLI dispatch, `eval`, `stats`, JSON output), `store.go` (schema, `upsert`, `loadAll`,
  `reindex`, float32⇄BLOB, meta), `embed.go` (Ollama client, cosine, top-K, `queryPrefix`), or
  `extract.go` (jsonl→units, redaction, short-turn filter, mtime guards). A new harness source is
  **another function in `extract.go`**, not a new file or interface.
- **Brute-force cosine search.** No ANN. sqlite-vec / ANN is deferred with trigger ~100k rows; don't
  add it speculatively.
- **Raw `text` is always stored next to `vector`.** A model swap is `memdb reindex`, never a schema
  migration. Don't drop or normalize the source text.
- **Locked consts** in `store.go` / `embed.go`: model `qwen3-embedding:0.6b`, dim `1024`,
  `repoBoost = 1.15`, `queryPrefix` (qwen3 retrieval instruction). Editing any of them is a
  tune-and-re-`eval` decision, not a silent edit.

## External runtime

- **Ollama must be running** at `http://localhost:11434` with `qwen3-embedding:0.6b` pulled. By
  design:
  - `search` / `add` / `eval` **fail loudly** with an actionable message.
  - `index --quiet` **fails silently** (exit 0) so SessionStart output doesn't pollute every
    conversation; the next session start retries. Do not "fix" the silent failure. Every run
    records `last_attempt` / `last_error` in `meta`, and `memdb stats` prints a `WARNING:` when
    the last run failed or `last_indexed` is over 7 days old (Claude Code deletes transcripts
    after `cleanupPeriodDays`, default 30 — a stalled indexer loses history for good).
- The `queryPrefix` (qwen3 retrieval instruction) is auto-applied on `search` and on `eval`'s
  default. Pass `eval --query-prefix ""` to measure the no-prefix baseline.

## Extraction behavior (why a turn is/isn't in the store)

Agents commonly wonder why a specific turn is missing or counts look low. From `extract.go`:

- **24h-unsettled guard** — `extractAll` skips any `.jsonl` with mtime newer than 24h. Today's live
  session is **not** in the store. Not a bug.
- **`last_indexed` mtime guard** — files older than `last_indexed` are skipped unless `--force`.
  `last_indexed` is a watermark set to **run start − 24h**, not "now" — otherwise files skipped as
  unsettled would fall behind the watermark and never be indexed. The no-op path is walk+stat only
  (~10 ms).
- **Resume** — `index` commits per 32-unit batch and skips units already stored with identical
  text, so a run killed mid-backlog picks up where it stopped.
- **Short-turn filter** — user turns under 40 chars or bare approvals (`ok`, `yes`, `go`, …) borrow
  the **preceding** assistant message; if there isn't one, the unit is dropped. Tune via
  `index --force --short-turn N`.
- **Noise drop** — user entries starting with `<command-name>`, `<local-command`, `<system-reminder>`,
  `Caveat:` are harness noise, not real turns.
- **Tool output is never stored.** Only `tool_use` *names* are kept on the assistant side; `tool_result`
  blocks are dropped on both sides.

## Redaction (ingest only, never on the read path)

Regex set lives in `extract.go`; surrounding prose must survive redaction (context is the valuable
part). The pull path can leak a repo-A secret into a repo-B session — redact-in-place is mandatory.
**If you add a pattern, add a fixture in `extract_test.go`** that proves both the redaction and the
prose survival (`TestRedaction` is the model).

## Phase 5 (deferred — build only when the trigger has actually fired)

| Item                                 | Trigger                                                                 |
| ------------------------------------ | ----------------------------------------------------------------------- |
| Auto-inject (`UserPromptSubmit`)     | Phase 3 works but the model keeps failing to search when it should      |
| OpenAI embeddings                    | eval plateaus and you suspect the model                                 |
| LLM-distilled units                  | eval stays weak after Phase 2 tuning                                    |
| sqlite-vec / ANN                     | `memdb stats` passes ~100k rows                                         |
| Per-workspace config                 | you actually want different behaviour in two repos                      |
| Index opencode history               | you start using opencode for real work (today: test junk)               |
| Index Codex history                  | you want Codex's past work recallable                                   |

## Host vs. sandbox

memdb targets a **Windows host**: the SessionStart hook spawns via
`MSYS_NO_PATHCONV=1 cmd /c start /b memdb index --quiet >/dev/null 2>&1` (PATH is
`%USERPROFILE%\go\bin`). Go build, store, embed, and cosine logic are cross-platform, but the hook
command is Windows-only — keep `cmd /c start /b` as the detached spawn. Both extras are required:
Claude Code runs hooks through Git Bash, which rewrites `/c` to a path unless `MSYS_NO_PATHCONV=1`
is set (`cmd` then opens a shell and `memdb` never runs), and without the redirect the hook waits on
the run. In a sandbox without Ollama, every embed-dependent path fails; only `extract_test.go` and
pure-Go logic (cosine, parsing, redaction) are verifiable offline.
