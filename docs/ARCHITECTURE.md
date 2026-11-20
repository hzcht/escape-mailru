# Architecture

## Overview

`mexporter` is a Go CLI application that exports mailbox messages to local `.eml` files using a read-only web API workflow. It emphasizes deterministic enumeration, resumability, and safe file/journal handling.

## High-level flow

```text
session -> mailapi -> layout -> config/TUI -> journal -> exporter
```

1. **Session**: load and validate authentication material. With `-init-session` (mode 0), the CLI opens a debuggable browser for sign-in, captures cookies and the web-session token from the browser's own traffic (manual session JSON import as a fallback), and validates the result against the API before saving. `-refresh-session` renews the web-session token from stored cookies without a browser.
2. **Mail API**: list folders, enumerate threads/messages, expand threads, download raw `.eml` data.
3. **Layout**: map provider folders to local paths and generate safe filenames.
4. **Config/TUI**: resolve runtime settings and folder selection.
5. **Journal**: persist per-message state in append-only logs and snapshots.
6. **Exporter**: coordinate enumeration, filtering/resume checks, parallel downloads, retries, and reporting.

## Directory structure (key parts)

```text
go/
  cmd/mexporter/      CLI entrypoint and wiring
  internal/
    session/          Session loading/validation
    mailapi/          API client and response types
    layout/           Folder tree and output path helpers
    config/           Config load/save/defaults
    journal/          JSONL log + JSON snapshot
    exporter/         Export orchestration and workers
    tui/              Folder selection UI/fallback
    cdp/              Chrome DevTools Protocol client (browser session capture)
scripts/              Helper scripts (e.g. IMAP migration)
docs/                 Project documentation
```

## Core components

### Session
- Reads session/auth data from a local file (`cookies` plus a `token` or legacy `access_token`).
- Supports guided bootstrap via `-init-session`: open a debuggable browser for sign-in, capture cookies and the web-session token from the browser traffic (manual session JSON import as a fallback), check the candidate against the API, then save.
- Supports token renewal via `-refresh-session` using stored cookies only (no browser).
- Supplies credentials/tokens required by API requests.
- Authentication failures are surfaced to the caller (exit code signaling handled at CLI level).

### Mail API client
- Performs read-only requests for folder and message/thread data.
- Downloads message content without mutating mailbox state.
- Fetches the web-session token from the mail UI inbox bootstrap (`FetchWebToken`) and probes the API (`Probe`) to confirm a session before it is saved or refreshed.
- Owns transport retries: exponential backoff, `Retry-After` handling, and no retry for permanent 4xx responses.
- Captures bounded error bodies for diagnostics and tunes the HTTP transport for multiple workers.

### Layout
- Builds a local folder structure mirroring mailbox hierarchy.
- Produces stable, filesystem-safe filenames using message identity and metadata.
- Never truncates the identity portion; long or unsafe identities get a deterministic hash suffix.

### Config and selection
- Loads runtime configuration and defaults.
- Supports interactive folder selection and non-interactive modes.
- Persists selected folders/settings when required by CLI behavior.
- Validates configured/selected folder IDs against the live folder tree.

### Journal
- `export-log.jsonl`: append-only event/state log (source of truth).
- `export-log.json`: snapshot for fast status reads.
- Snapshot is loaded first, then JSONL is replayed; an empty or corrupt JSONL does not erase a valid snapshot.
- Supports resume by distinguishing completed, skipped, and failed work; recorded size is compared with the file.
- Optional SHA-256 fields enable offline integrity verification (`-verify-hash`).

### Exporter
- Enumerates messages in ascending date order and streams them into the worker pool.
- Uses a worker pool for downloads with bounded concurrency.
- Retries transient failures at the API client level and records outcomes in the journal.
- Writes files safely and validates downloaded artifacts before marking complete.
- Reports failed folders/threads and truncation as a partial run.

### CLI
- Maps errors to documented exit codes (`0`, `1`, `2`, `130`).
- Provides offline verification, JSON summaries, a per-account export lock, and `-allow-partial`.

## Invariants

- Enumeration order must be ascending by date for long-running exports.
- Message identity is provider uidl.
- Only read operations are permitted against the mailbox.
- Journal/download completion must reflect actual, valid local files.
- A single account output target must not be written by concurrent exporter processes; the CLI enforces this with a lock file.

## Testing and verification

Typical checks from `go/`:

```powershell
gofmt -l .
go vet ./...
go test ./...
```

Targeted tests can be used during development for specific packages or scenarios.

Fuzz targets cover the two places where untrusted strings become file-system
paths or credentials: `layout.SanitizeName`/`SafeFilename` (unsafe characters,
reserved device names, identity preservation) and session JSON parsing plus the
cookie domain allow-list. Seeds run as ordinary tests; `.github/workflows/ci.yml`
runs gofmt, vet, build, tests, `-race`, and a short fuzz pass on every push.
