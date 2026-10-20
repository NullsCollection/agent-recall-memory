# memdb

Long-term memory for coding agents. It indexes your past Claude Code sessions into a local SQLite
store so that "how did we solve this before?" has an answer — including when the answer lives in a
different repo.

Everything runs on your machine. One binary, one dependency, no network on the read path beyond a
local Ollama call.

```
~/.claude/projects/**/*.jsonl ──► memdb index ──┐
                                                 ├──► ~/.claude/memdb.sqlite
              "remember this" ──► memdb add  ────┘             │
                                                               ▼
                             recall skill ──► memdb search ──► top-K hits
```

## The problem

Every session starts cold. The decision you made three weeks ago in another repo — why you picked
subtree over filter-repo, which auth approach you settled on, what the fix for that build error
actually was — is sitting in a JSONL file on disk that nothing ever reads again.

`memdb` reads them.

## How it works

Each user turn is fused with the assistant's reply and the *names* of the tools it called (never
tool output) into one semantic unit:

```
User: how did we split the monorepo?
Assistant: The split is done: local branch `api-history` now holds the API's full history...
Tools: Bash, Read
```

Units get embedded with a local Ollama model, stored next to their raw text, and searched with a
brute-force cosine scan. At a few hundred rows that's sub-millisecond, so there's no ANN index to
maintain — and because the raw text is always kept, changing the embedding model is `memdb reindex`
rather than a schema migration.

Short turns ("ok", "yes, do that", "A") borrow the preceding assistant message, so the *decision*
being approved survives instead of a memory that just says "ok".

## Install

**Prerequisites:** Go 1.25+, and [Ollama](https://ollama.com) running with the embedding model:

```bash
ollama pull qwen3-embedding:0.6b
```

Then:

```bash
git clone https://github.com/NullsCollection/agent-recall-memory.git
cd agent-recall-memory
go install ./...
```

The binary lands in `$GOPATH/bin` (`%USERPROFILE%\go\bin` on Windows) — make sure that's on your
PATH. The database is created at `~/.claude/memdb.sqlite` on first run.

## Usage

```
memdb index [--force] [--quiet]              walk sessions, embed, upsert (idempotent)
memdb add [--cwd .] -- <text>                store a manual memory
memdb search [--top-k 5] [--cwd .] [--json] -- <query>
memdb reindex [--force]                      re-embed stored text on the current model
memdb stats                                  row counts by repo / kind / model
memdb eval [--golden golden.txt]             run golden queries, print hit/miss + rank
```

First backfill — watch this one, it takes a few minutes:

```bash
memdb index --force
memdb stats
memdb search -- "why did we pick that auth approach"
```

Each hit prints `repo · kind · date · score` followed by the text, so you always know which project
a memory came from.

### Indexing rules

- Files modified in the last **24 hours** are skipped — a live session is still being written to.
- Files older than the last successful index are skipped unless `--force`.
- Harness noise (`<command-name>`, `<system-reminder>`, `Caveat:` blocks) is dropped.
- Tool *output* is never indexed. Tool names are.
- Row ids are `sha256(sessionID + entryUUID)`, so re-indexing is an idempotent upsert.

### Secrets

Redaction runs at ingest, never on the read path — cross-repo recall means a secret from repo A
could otherwise surface in a repo B session. API keys, GitHub tokens, AWS keys, Slack tokens,
database URLs with inline credentials, bearer tokens, and PEM private keys are replaced with
`[REDACTED]`.

Surrounding prose is deliberately preserved, because the context is the part worth keeping. This is
a best-effort net over realistic cases, not a guarantee — the database is local-only either way.

## Integrations

**Claude Code** — two skills in `skills/`, copied to `~/.claude/skills/`:

| Skill | Wraps | Fires on |
|---|---|---|
| `recall-memory` | `memdb search` | "remember when we…", "like last time", "did we already decide…" |
| `remember-this` | `memdb add` | "remember this", "note this for later" |

Indexing runs itself via a `SessionStart` hook in `~/.claude/settings.json`, detached so it never
blocks the prompt:

```jsonc
{
  "permissions": {
    "allow": ["Bash(memdb search:*)", "Bash(memdb add:*)"]
  },
  "hooks": {
    "SessionStart": [
      { "type": "command", "command": "cmd /c start /b memdb index --quiet" }
    ]
  }
}
```

Under `--quiet` the indexer fails silently if Ollama is down — the next session start retries.
Search, by contrast, fails loudly with an actionable message, because a silent failure there would
look like "no memories" instead of "no Ollama".

**opencode** — no plugin needed, just a note in `~/.config/opencode/AGENTS.md` telling it to run
`memdb search --top-k 5 -- "<query>"`. Memories are produced on the Claude Code side; anything that
can run a shell command can read them.

## Tuning

Retrieval quality is a number, not a vibe. Copy `golden.example.txt` to `golden.txt`, fill it with
your own `query ||| expected-substring` pairs, and run:

```bash
memdb eval
```

You get HIT/MISS with rank per query, a summary score, and the raw-cosine spread of good hits.
Then change one knob at a time — `--top-k`, `--repo-boost`, `--query-prefix`, `--short-turn` — and
re-run.

The one rule that matters: **pick the target memories by browsing the store, not by searching it.**
If you choose them by searching, you're only proving that search finds what search already found.

On the reference corpus (253 units across 21 repos) the tuned configuration scores **13/14, mean
rank 1.8**. The single biggest win was the model's retrieval instruction prefix on queries
(+1 hit); the same-repo score boost turned out to *cost* accuracy on a cross-repo query set and was
left at its minimum.

## Design notes

- **SQLite + brute-force cosine**, not a vector database. At this corpus size a linear scan is
  faster than the index lookup would be, and there's nothing to operate.
- **One dependency:** `modernc.org/sqlite` — pure Go, no cgo, so cross-compiling stays trivial.
- **Four source files**, and the discipline to keep it there: `main.go` (dispatch, eval, stats),
  `store.go` (schema, upsert, load), `embed.go` (Ollama client, cosine, top-K), `extract.go`
  (JSONL → units, redaction).
- **Adding another agent harness is another function, not an interface.** Only extraction is
  harness-specific; the store is the protocol.

`PLAN.md` carries the full reasoning, the phase-by-phase build log, and the deferred items — each
with the trigger condition that would justify building it.

## Status and limitations

Working and in daily use. Known edges:

- Developed on Windows. The Go core is cross-platform; the `SessionStart` hook command above is
  Windows-specific and needs the POSIX equivalent elsewhere.
- Resumed sessions can produce duplicate rows, which eat top-K slots.
- Changing `--short-turn` and re-indexing leaves behind rows the new threshold would drop; a clean
  sweep needs the store rebuilt from empty.
- Sessions running inside a container aren't visible to the host indexer.

## License

MIT
