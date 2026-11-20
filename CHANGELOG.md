# Changelog

## Unreleased

### Added
- Session bootstrap mode (`-init-session`, mode 0): open browser for manual sign-in, import session JSON, save session file, and continue. Existing session files require `-force`.
- Automatic session capture through a debuggable Chrome/Edge window during `-init-session`: the app reads `access_token`, e-mail, CSRF token and cookies from the browser's own traffic and falls back to manual JSON import when the browser cannot be driven.
- Offline integrity check (`-verify`, `-verify-hash`) comparing journal records with files, including optional SHA-256.
- Machine-readable output (`-json`) for folder lists, export summaries, and verify reports.
- Exact `-limit` semantics: the run attempts at most N messages.
- Partial-export reporting and `-allow-partial`; partial runs exit 1 by default.
- Per-account export lock with stale-lock handling and `-force`.
- API base URL override (`-base-url`).
- Retry/backoff in the API client with `Retry-After` support, response-size checks, and error-body capture.
- Streaming enumeration into the worker pool, folder summaries, SHA-256 journal fields, and size-aware resume checks.
- Session validation for VK/WorkSpace cookie domains and expiring-cookie warnings.
- Folder-ID validation, JSON output, and guided session replacement safeguards.

### Changed
- `-limit` now counts attempted messages rather than threads; `-delay` accepts Go durations.
- Thread expansion failures and truncation are reported instead of silently falling back.
- Journal loading keeps snapshot entries when JSONL is empty or corrupt.
- Rewrote project documentation for clarity and neutrality.
- Anonymized example commands, paths, and account identifiers in docs and helper scripts.
- Removed environment-specific references from project documentation and notes.

## Earlier milestones (summary)

### Export engine
- Implemented read-only mailbox export to local `.eml` files via web API access.
- Added folder discovery, selective export, dry-run enumeration, and resumable operation.
- Added worker-based downloads, retry/backoff handling, and append-only journaling with snapshots.

### Usability
- Added interactive folder selection with fallback for non-interactive environments.
- Added configuration persistence for selected folders and runtime options.

### Migration utilities
- Added a Python standard-library script for uploading exported `.eml` files to an IMAP server.

> Historical run-specific metrics and account details were intentionally removed from this changelog to keep documentation generic.
