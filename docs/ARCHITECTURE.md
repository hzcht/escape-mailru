# Architecture — mexporter (Mail.ru / VK WorkSpace exporter)

## Overview

One-off web API exporter: captures Mail.ru/VK WorkSpace mailbox to `.eml` files via the public web API. Playwright (injected MCP) captures session cookies on first run; the Go tool uses those cookies to enumerate and download messages.

## Directory Structure

```
mexporter/
  README.md                 # User-facing documentation
  CHANGELOG.md              # Version history and changes
  AGENTS.md                 # Dev invariants, commands, architecture (repo root)
  RETROSPECTIVE.md          # Full run history, metrics, limitations
  run_mexporter.ps1         # Autonomous launcher (no OpenCode needed)
  .gitignore
  go/                       # Main Go module (Go 1.27, stdlib only)
    cmd/mexporter/
      main.go               # CLI entry, flags, TUI wiring
    internal/
      mailapi/              # API client and types
      layout/               # File naming and folder structure
      config/               # Config loading
      tui/                  # Folder selector
      exporter/             # Export engine (enumerate, download, journal)
      journal/              # JSONL + JSON snapshot
  scripts/                  # Utility scripts
    migrate_to_imap.py      # IMAP migration script (Python stdlib)
    README.md               # Script documentation
  session/                  # session.json (cookies + token — SECRET, not in git)
  mail-export/              # Exported .eml files (not in git)
```

## Flow (One-Way Dependency Chain)

```
session → mailapi → layout → config/TUI → journal → exporter
```

- **session** — File parsing for session.json (cookies, access_token).
- **mailapi** — HTTP client for Mail.ru/VK API (folders, threads, thread expansion, .eml download). Read-only.
- **layout** — Generates output paths: `mail-export/<email>/<folder>/<date>_<subject>_<uidl>.eml`.
- **config/TUI** — Loads `mexporter.json`, shows folder selector, validates input.
- **journal** — JSONL source of truth + JSON snapshot for fast reads (fast_read package).
- **exporter** — Orchestrates: enumerate (date asc), worker pool, download, resume logic.

## Key Packages

| Package | Responsibility |
|---------|---------------|
| `internal/mailapi` | `Folders()`, `Threads()`, `ThreadExpand()`, `DownloadThread()`. All GET/read only. |
| `internal/exporter` | `ExportFolder(folderID, ...)` — enumerate, download, write .eml, append to journal. |
| `internal/journal` | `AppendJSONL(entry)`, `Snapshot()` — fast snapshot for reads. |
| `internal/layout` | `FileName(date, subject, uidl)` — sanitized filename. `OutputDir(email, folderID)`. |
| `internal/tui` | Raw Windows console line-based folder selector. |
| `internal/config` | `Load(path)` — reads `mexporter.json` (email, out_dir, workers, delay, folders). |
| `session/` | `Load(path)` — reads `session/session.json` (cookies, token). Secret. |

## Data Formats

### Session File (`session/session.json`) — SECRET
```json
{
  "cookies": { "name": "value", ... },
  "access_token": "eyJ..."
}
```
Treat as secret. Never commit, never log.

### Config File (`mexporter.json`)
```json
{
  "email": "user@mail_ru_hosted_domain",
  "out_dir": "mail-export/user@mail_ru_hosted_domain",
  "workers": 4,
  "delay_ms": 100,
  "folders": [0, 500000]
}
```

### Journal (`export-log.jsonl`) — Append-only
```json
{"ts":"2026-11-19T10:00:00Z","thread_id":"abc","date":"2026-11-19","uidl":"xyz","action":"downloaded","file":"Inbox/2026-11-19_Subject_xyz.eml","size":12345}
{"ts":"2026-11-19T10:00:01Z","thread_id":"abc","date":"2026-11-19","uidl":"xyz","action":"done"}
```

### Snapshot (`export-log.json`) — Fast Reads
```json
{
  "version": 1,
  "updated": "2026-11-19T10:00:00Z",
  "entries": [
    {"thread_id":"abc","date":"2026-11-19","uidl":"xyz","action":"done","file":"Inbox/2026-11-19_Subject_xyz.eml","size":12345}
  ]
}
```

## Invariants (must always hold)

1. **`date asc` only.** Never `date desc` + offset — loses messages when new mail arrives.
2. **Identity = uidl.** Never subject or date.
3. **Read-only.** Only GET/read endpoints. No send/delete/move/settings changes.
4. **Trust:** `downloaded` journal entries are trusted only if `.eml` file exists and is non-empty; size re-verified at write.
5. **No inter-process locking.** Don't run two exporter instances on the same output.
6. **`.eml` bytes untouched.** Filename is the only metadata.
7. **Idempotent.** Re-running a folder re-enumerates and skips already-downloaded.

## Testing

```powershell
cd go
gofumpt -l .                  # formatting (fallback: gofmt -l .)
go vet ./...                  # vet
go test ./...                 # unit tests
go test -race ./...           # (unavailable here — CGO disabled)
go test ./internal/exporter -run TestDownload -v   # narrow test
go test -coverprofile=cover.out ./... && go tool cover -func=cover.out
```

## Gotchas

- `-folders` and TUI persist selection to `mexporter.json` **before** export.
- `-limit N` limits threads, not messages — one thread can yield more than N files.
- `-delay 0` means "read `delay_ms` from config", not "no delay".
- `-no-tui` with empty `folders` exits 1.
- PowerShell prints native stderr as `NativeCommandError` — check `$LASTEXITCODE`.
- TUI without real console → line fallback. Don't automate it.
- Set `[Console]::OutputEncoding=[Text.Encoding]::UTF8` before viewing Cyrillic filenames.

## IMAP Migration

The `scripts/migrate_to_imap.py` script migrates exported `.eml` files to an IMAP server (e.g., `imap.spaceweb.ru`):
- Uses Python stdlib only (`imaplib`, `email`, `argparse`, `json`, `pathlib`)
- Supports config file (`scripts/config.json`) or CLI args
- Supports SSL (993) and plain (143) connections
- Recursive `.eml` file discovery
- Progress logging and error handling
- See `scripts/README.md` for full documentation

## Adding New Features (Development Workflow)

1. **Read existing code** before modifying — understand the patterns
2. **Run tests** — `go test ./internal/<package>/...` before changes
3. **Follow Go style** — small functions, comments on exported items, tests for business logic
4. **Update docs** — README.md, CHANGELOG.md, RETROSPECTIVE.md if significant
5. **Commit** — only on explicit request from user

## Performance Notes

- Default worker pool: 4 workers
- Network delay: 100ms between thread downloads
- Memory: ~200 MB baseline + ~10 MB per concurrent thread
- Large exports (127k files): ~6.4 GB — monitor disk space
- Journal operations: append-only, fast snapshot rebuild on demand
