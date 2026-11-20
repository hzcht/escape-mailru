# mexporter

`mexporter` exports mailbox messages to local `.eml` files using a web API workflow. It is designed for one-way, read-only mailbox export and resumable downloads.

## Features

- Read-only export (no send/delete/move operations)
- Folder listing and selective export
- Interactive folder selection (with non-interactive fallback)
- Dry-run enumeration mode
- Parallel downloads with configurable workers and delay
- Append-only journal and snapshot for resume/retry safety
- Deterministic local file layout per account/folder

## Requirements

- Go 1.27 or newer
- A valid session file with authentication data for the target mailbox

## Build

```powershell
cd go
go build -o mexporter.exe ./cmd/mexporter
cd ..
```

## Usage

Run commands from the project root.

```powershell
# First run: create/import session (mode 0), then continue
.\go\mexporter.exe -init-session

# Create/import session and immediately list folders
.\go\mexporter.exe -init-session -list

# Show folder tree (requires valid session)
.\go\mexporter.exe -list

# Interactive folder selection, then export
.\go\mexporter.exe

# Export folders already saved in config (no interactive UI)
.\go\mexporter.exe -no-tui

# Enumerate messages in selected folders without downloading
.\go\mexporter.exe -folders 0 -dry-run -v

# Export specific folders
.\go\mexporter.exe -folders 0,1

# Use custom paths
.\go\mexporter.exe -session <session-file> -config <config-file> -out <output-dir>
```

### Common flags

| Flag | Description |
| --- | --- |
| `-init-session` | Guided session setup: open browser for manual sign-in, then import and save session data |
| `-login-url` | URL to open during `-init-session` (default: `https://e.mail.ru/`) |
| `-list` | Print folder tree and exit |
| `-folders` | Comma-separated folder IDs to export |
| `-dry-run` | Enumerate only, do not download message files |
| `-limit` | Limit number of threads to process |
| `-workers` | Number of download workers |
| `-delay` | Delay between requests (ms), subject to config behavior |
| `-no-tui` | Skip interactive selector and use folders from config |
| `-session` | Path to session file (default: `session/session.json`) |
| `-config` | Path to config file (default: `mexporter.json`) |
| `-out` | Output root directory |
| `-v` | Verbose output |

## Session

Authentication data is stored in a session file (for example `session/session.json`). Treat this file as secret and do not share or commit it.

### First run / session setup (mode 0)

If you do not have a session file yet, run:

```powershell
.\go\mexporter.exe -init-session
```

This mode:
1. Opens the login page in your default browser (or prints the URL if automatic open fails).
2. Lets you sign in manually in the browser.
3. Prompts you to import session data as JSON (from a file path or pasted input) and saves it to the session file.

Required session JSON fields: `email`, `access_token`, `cookies` (array). Optional: `csrf_token`.

After saving, the app continues with the requested action in the same run (for example `-list`, folder selection, or export). If the session is invalid, the command will fail when it tries to access the API.

If the tool exits with code `2` during normal operation, the session is no longer valid. Recreate/update it with `-init-session` or replace the session file.

## Typical workflows

### First export of a mailbox
1. Run `-init-session` and sign in manually in the browser.
2. Import the session JSON when prompted.
3. Let the app list folders and select the ones you need.
4. Start the export; rerun the same command later to resume.

### Resume after interruption
- Re-run the export with the same session/config/output paths.
- Completed messages are skipped using the journal; failed ones are retried.

### Multiple mailboxes
- Use a separate `-session`, `-config`, and `-out` set per mailbox to keep state isolated.

### Safe trial run
- Use `-dry-run -v` first to verify enumeration and folder mapping without downloading files.
- Use `-limit N` to cap the number of processed threads for a quick test.

## Output layout

```text
<output-dir>/
  <account>/
    <folder-path>/
      <date>_<subject>_<uidl>.eml
    export-log.json
    export-log.jsonl
```

- `export-log.jsonl` is the append-only source of truth.
- `export-log.json` is a snapshot for faster reads.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | Runtime or usage error |
| `2` | Authentication required or expired |
| `130` | Canceled by user |

## Notes

- Enumeration uses ascending date order to avoid missing messages during long runs.
- Message identity is based on provider uidl.
- Do not run multiple instances against the same output directory.
- Export directories may become very large; inspect logs/snapshots instead of scanning all files.
