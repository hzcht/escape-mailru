# mexporter

`mexporter` exports mailbox messages to local `.eml` files using a web API workflow. It is designed for one-way, read-only mailbox export and resumable downloads.

## Features

- Read-only export (no send/delete/move operations)
- Folder listing and selective export
- Interactive folder selection (with non-interactive fallback)
- Dry-run enumeration mode
- Parallel downloads with configurable workers and delay
- Append-only journal and snapshot for resume/retry safety
- Offline integrity check (`-verify`, optional SHA-256 with `-verify-hash`)
- Machine-readable summaries (`-json`)
- Single-instance lock per account output directory
- Deterministic local file layout per account/folder

## Быстрый старт (для новичка)

Пошаговая инструкция, если вы запускаете программу впервые. Ничего устанавливать не нужно: в архиве уже лежит готовый бинарник.

### Шаг 1. Скачайте и распакуйте архив

- **Windows**: скачайте `mexporter-windows-x64.zip`, распакуйте в простую папку, например `C:\mexporter`.
- **Linux**: скачайте `mexporter-linux-x64.tar.gz`, распакуйте командой `tar -xzf mexporter-linux-x64.tar.gz` и сделайте бинарник исполняемым: `chmod +x mexporter`.

Внутри архива:

| Элемент | Что это |
| --- | --- |
| `mexporter` / `mexporter.exe` | сама программа |
| `README.md` | эта инструкция |
| `mexporter.json` | настройки (уже заполнены по умолчанию, менять не обязательно) |
| `session/` | сюда сохраняются данные входа в почту — **секрет, никому не отправляйте** |
| `mail-export/` | сюда складываются экспортированные письма |

### Шаг 2. Войдите в почту (только при первом запуске)

Откройте терминал именно в папке с программой:

- **Windows**: откройте папку в проводнике, в строке адреса наберите `cmd` и нажмите Enter;
- **Linux**: выполните `cd /путь/к/папке`.

Введите команду и нажмите Enter:

```text
Windows:  mexporter.exe -init-session
Linux:    ./mexporter -init-session
```

Откроется браузер со страницей входа — войдите в почту как обычно. Когда почта откроется, вернитесь в терминал и нажмите Enter. Программа сама считает данные входа из браузера, проверит их через API и сохранит в `session/session.json`.

### Шаг 3. Посмотрите список папок

```text
mexporter.exe -list
```

### Шаг 4. Начните экспорт

```text
mexporter.exe
```

Откроется интерактивный список папок почты: отметьте нужные пробелом (стрелки — навигация), Enter — старт. Письма появятся в `mail-export\<ваш адрес>\...`.

Если экспорт прервали (закрыли окно, выключили компьютер) — просто запустите ту же команду снова: уже скачанные письма не повторяются, продолжение начнётся с места остановки.

### Шаг 5. Если что-то пошло не так

| Ситуация | Что делать |
| --- | --- |
| `authorization failed` или код выхода `2` | `mexporter.exe -refresh-session` — обновить вход без повторного логина. Не помогло → `mexporter.exe -init-session -force` — войти заново |
| Нужно проверить, что все письма на месте | `mexporter.exe -verify` |
| Хотите посмотреть, что будет скачано, ничего не скачивая | `mexporter.exe -folders 0 -dry-run -v` |

Дальше — справка для остальных сценариев (запуск из исходников, справка по флагам, работа со скриптами).

## Requirements

- A prebuilt binary from the GitHub Releases page (no Go needed), or Go 1.27 or newer to build from source
- A valid session file with authentication data for the target mailbox (create one with `-init-session`, see «Быстрый старт» above)

## Build

```powershell
cd go
go build -o mexporter.exe ./cmd/mexporter
cd ..
```

## Usage

Run commands from the project root. Commands below use the source-tree layout (`.\go\mexporter.exe`); if you unpacked a release archive, run `.\mexporter.exe` (or `./mexporter` on Linux) from the archive folder — the flags are identical.

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

# Check existing files against the journal (add -verify-hash for SHA-256)
.\go\mexporter.exe -verify -json

# Accept a run where some folders or threads failed
.\go\mexporter.exe -no-tui -allow-partial

# Use custom paths or API base
.\go\mexporter.exe -session <session-file> -config <config-file> -out <output-dir> -base-url <api-base>
```

### Common flags

| Flag | Description |
| --- | --- |
| `-init-session` | Guided session setup: open browser for manual sign-in, then capture cookies and the web-session token |
| `-refresh-session` | Refresh the web-session token from the mail UI using the stored cookies (no browser) |
| `-login-url` | URL to open during `-init-session` (default: `https://e.mail.ru/`) |
| `-force` | Replace an existing session (`-init-session`) or remove a stale/active export lock |
| `-list` | Print folder tree and exit |
| `-folders` | Comma-separated folder IDs to export |
| `-dry-run` | Enumerate only, do not download message files |
| `-limit` | Cap the number of messages attempted in this run (0 = no limit) |
| `-workers` | Number of download workers |
| `-delay` | Pause per worker as a Go duration (for example `250ms`); `0` uses config `delay_ms` |
| `-no-tui` | Skip interactive selector and use folders from config |
| `-session` | Path to session file (default: `session/session.json`) |
| `-config` | Path to config file (default: `mexporter.json`) |
| `-out` | Output root directory |
| `-base-url` | Mail API base URL (default: `https://e.mail.ru/api/v1/`) |
| `-verify` | Check journal entries against files and exit |
| `-verify-hash` | Also compare SHA-256 (implies `-verify`) |
| `-json` | Write JSON to stdout for `-list`, export summary, or `-verify` |
| `-allow-partial` | Exit 0 even when folders or threads failed |
| `-v` | Verbose output |

## Session

Authentication data is stored in a session file (for example `session/session.json`). Treat this file as secret and do not share or commit it.

### First run / session setup (mode 0)

If you do not have a session file yet, run:

```powershell
.\go\mexporter.exe -init-session
```

This mode:
1. Refuses to overwrite an existing session file unless `-force` is passed.
2. Tries to capture the session automatically: it opens the login page in a debuggable Chrome/Edge window and you sign in manually. It then reads the session cookies from the browser and fetches the web-session token that the mail UI embeds into its inbox page (`/api/v1/user/short`). The resulting session is checked against the API before the file is written; if nothing is accepted, it falls back to manual import.
3. Falls back to manual import if automatic capture cannot run: you export session JSON (fields `email`, `cookies`, and either `token` or `access_token`; optionally `csrf_token`) from the browser, save it to a file or paste it into the prompt, and the app saves it to the session file.

Required session JSON fields: `email`, `cookies` (array) and either `token` (current web UI) or `access_token` (older sessions). Optional: `csrf_token`.

The web-session `token` expires independently of the cookies. Refresh it without signing in again:

```powershell
.\go\mexporter.exe -refresh-session
```

This fetches a fresh token from the mail UI using the stored cookies and saves it back to the same session file.

After saving, the app continues with the requested action in the same run (for example `-list`, folder selection, or export). If the session is invalid, the command will fail when it tries to access the API.

If the tool exits with code `2` during normal operation, the session is no longer valid. Recreate/update it with `-init-session` or replace the session file.

## Typical workflows

### First export of a mailbox
1. Run `-init-session` and sign in in the browser that opens; the app captures the session automatically (manual session JSON import is the fallback).
2. Let the app list folders and select the ones you need.
3. Start the export; rerun the same command later to resume.

### Resume after interruption
- Re-run the export with the same session/config/output paths.
- Completed messages are skipped using the journal; failed ones are retried.
- If a download stopped mid-write, the size mismatch makes the exporter download it again.

### Verify and repair
- Run `-verify` (or `-verify-hash` when journal entries include SHA-256).
- Missing or size-mismatched files exit with code 1; rerunning the export re-downloads them.

### Multiple mailboxes
- Use a separate `-session`, `-config`, and `-out` set per mailbox to keep state isolated.

### Safe trial run
- Use `-dry-run -v` first to verify enumeration and folder mapping without downloading files.
- Use `-limit N` to cap the number of messages attempted for a quick test.
- If some folders or threads fail, rerun after fixing the cause or pass `-allow-partial` to accept a partial result.

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
| `0` | Success, or accepted partial run with `-allow-partial` |
| `1` | Runtime/usage error, failed integrity check, or partial export |
| `2` | Authentication required or expired |
| `130` | Canceled by user (including interactive selector quit) |

## Notes

- Enumeration uses ascending date order to avoid missing messages during long runs.
- Message identity is based on provider uidl.
- Do not run multiple instances against the same output directory; a lock file in each account directory enforces this (`-force` removes it).
- Export directories may become very large; inspect logs/snapshots instead of scanning all files.
- Session cookies from `mail.ru`, `vk.com`, `vk.ru`, and `userapi.com` domains are accepted; unrelated cookies are ignored.
