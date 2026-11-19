# Changelog

## [2026-11-19] — IMAP Migration Script Added

- ✅ Added `scripts/migrate_to_imap.py` — Python stdlib IMAP migration script
  - Migrates `.eml` files from local folders to IMAP servers (`imap.example.com`)
  - Uses only Python 3 standard library (`imaplib`, `email`, `argparse`, `json`, `pathlib`)
  - Config file (`scripts/config.json`) + CLI arguments support
  - SSL (port 993) and plain (port 143) connections
  - Recursive `.eml` file discovery
  - Progress logging and error handling (skips corrupt files, continues)
  - Updated `scripts/README.md` with full usage documentation
- ⚠️ Go IMAP implementation abandoned — `github.com/emersion/go-imap` v1.2.1 incompatible with Go 1.27 (undefined: `imap.Client`, `imap.DialTLS`, `imap.Dial`)
- ❌ Removed stale `go.sum`, cleaned `go.mod`
- 📝 Updated `scripts/README.md` with migration script documentation

## [2026-11-19] — Documentation Update

- 📄 Updated `README.md` with migration script info
- 📄 Created this `CHANGELOG.md`
- 📁 Created `docs/ARCHITECTURE.md` — Developer documentation

## [2026-11-19] — Initial Export (v1.0)

### Full Export — `user@mail_ru_hosted_domain`
- 📊 127,829 `.eml` files, 6.42 GB, 0 errors
- 🔄 `date asc` enumeration (from old to new)
- 🏷️ Identity: Mail.ru uidl

### Full Export — `user@mail_ru_hosted_domain`
- 📊 1,202 `.eml` files, 247.54 MB, 0 errors
- 📄 Journal: `mail-export/user@mail_ru_hosted_domain/export-log.json`

### Core Features (Go 1.27, stdlib only)
- ✅ Web API via Playwright-captured session
- ✅ Folder selector TUI (arrow keys, Space, A/N/Enter)
- ✅ Dry-run enumeration mode
- ✅ Worker pool parallelism (default: 4 workers)
- ✅ Resume-safe journaling (append-only `export-log.jsonl`)
- ✅ Batch size limiting, delay config
- ✅ Read-only (GET/read only — no mutations)
- ✅ Automatic session refresh from browser

### Output
- 📁 `mail-export/<email>/<folder>/<date>_<subject>_<uidl>.eml`
- 📁 `mail-export/<email>/export-log.json` (fast snapshot)
- 📁 `mail-export/<email>/export-log.jsonl` (append-only truth)
- 📁 `mexporter.json` (persisted config)
- 📁 `session/session.json` (cookies + token — secret)

### Exit Codes
- 0 — success / enumeration done
- 1 — error (folder not found, IO error)
- 2 — auth expired (refresh session from browser)
- 130 — canceled (Ctrl+C in TUI)

### Invariants
- 🔒 `date asc` enumeration only (never `date desc + offset`)
- 🔒 Identity is uidl, never subject or date
- 🔒 Only GET/read endpoints
- 🔒 `downloaded` journal entries trusted only if file exists and non-empty
- 🔒 No inter-process locking (don't run two instances)
- 🔒 `.eml` bytes are never modified

### Verification (definition of done)
- ✅ `go build ./...` clean
- ✅ `go vet ./...` clean
- ✅ `go test ./...` green
- ✅ `gofumpt -l .` empty
- ✅ Coverage ≥ 80% on business logic
- ✅ `go mod verify` / `go mod tidy`

## [2026-11-19] — Git Repository
- 📦 Initialized: `e733f0a` (60 files, 9,621 insertions)
- 📦 Added `.gitignore`: `e733f0a` → `1b6f859`
- 📄 Local-only repo (no push destination configured)
- 🔐 Excluded: `mail-export/`, `session/`, `mexporter.exe`, `mexporter.json`, logs, compiled binaries
