# Changelog

## Unreleased

### Added
- Session bootstrap mode (`-init-session`, mode 0): open browser for manual sign-in, import session JSON, save session file, and continue.

### Changed
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
