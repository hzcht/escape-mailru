# mexporter — Mail.ru / VK WorkSpace mailbox exporter

Export your Mail.ru email to local `.eml` files. No IMAP/POP3 needed — uses the public web API (captured via Playwright).

> **Status (19.11.2026):** полный экспорт `user@mail_ru_hosted_domain` завершен: 127 829 `.eml`, 6,42 ГБ, 0 ошибок. Экспорт второго ящика `user@mail_ru_hosted_domain`: 1 202 `.eml`, 247,54 МБ, 0 ошибок. Бинарник `go\mexporter.exe` пересобран, `go vet` чистый, покрытие тестами 86,6 %+. Сессия обновлена из залогиненного Playwright-браузера.

> **IMAP Migration:** После экспорта `.eml` файлов их можно загрузить на IMAP-сервер (`imap.spaceweb.ru`) через [scripts/migrate_to_imap.py](scripts/README.md).

## Quick start (Windows)

1. **Install Go 1.27+** and add `go` to PATH.
2. **Log in** to `app.workspace.vk.ru/mail` in a browser (any account).
3. Run once to build and capture a session:

```powershell
cd C:\path\to\mexporter
powershell -ExecutionPolicy Bypass -File run_mexporter.ps1 -list
```

   This builds `go\mexporter.exe`, prints the folder tree, and saves a `session/session.json` file (cookies + `access_token`).

4. **Start the export** (or use the TUI):

```powershell
# Fast enumeration only (no download)
.\go\mexporter.exe -folders 0 -dry-run -v

# Full export of Inbox only (fast, 4 workers)
.\go\mexporter.exe

# Or skip the selector and use saved config
.\go\mexporter.exe -no-tui
```

## Commands

All paths are relative to `C:\path\to\mexporter` (the project root).

| Command | What it does |
|---|---|
| `.\go\mexporter.exe -list` | Print folder tree (needs valid session). |
| `.\go\mexporter.exe` | Drop into TUI: pick folders with arrow‑keys / Space / A (All) / N (None) / Enter. Selections are saved to `mexporter.json`. |
| `.\go\mexporter.exe -no-tui` | Export folders already saved in `mexporter.json`. |
| `.\go\mexporter.exe -folders 0 -dry-run -v` | Enumerate only folder 0 (Inbox), show every message. |
| `.\go\mexporter.exe -folders 0,500000 -limit 5` | Download up to 5 messages (threads) from Inbox + Sent. |
| `.\go\mexporter.exe -session session\other.json -list` | Use a different session file (path relative to project root). |

## Session — how it works

- `session/session.json` holds **cookies** + **`access_token`**. Treat it as a secret — never post it.
- Exit code **2** means "auth expired". Refresh it:
  1. Open `https://e.mail.ru/` (or `app.workspace.vk.ru/mail`) в **свежем браузере**, уже логином.
  2. Запустите `powershell -ExecutionPolicy Bypass -File run_mexporter.ps1 -list` — скрипт захватывает новый `access_token` через Playwright request listener и перезаписывает `session.json`.
  3. Старый файл можно сохранить как `session/session.expired.json` (опционально).
- Приложение **не имеет автоматического обновления** — токен нужно перехватывать вручную при истечении.

> **Примечание:** `go test -race` недоступен здесь (CGO выключен); `staticcheck` несовместим с Go 1.27. Ничто из этого не считается ошибкой кода.

## Invariants (what the app guarantees)

- Перебор ведется в порядке **`date asc`** (от старых к новым). `date desc + offset` теряет письма при появлении новой почты.
- Идентификатор — **mail.ru uidl**, никогда — subject или date.
- Только **GET/read** эндпоинты — никогда не отправляйте, не удаляйте, не перемещайте и не меняйте настройки почтового ящика.
- Запись `downloaded` в журнале доверяется только если `.eml` файл существует и непуст; размер сверяется заново при записи.
- Интер‑процессной блокировки нет — **не запускайте два экземпляра экспорта** в одну папку.
- Имена файлов: `<date>_<subject>_<uidl>.eml` (subject санитизирован, перелишние руны обрезаются до 120).

## Gotchas

- **`-folders` и TUI** сохраняют выбор папок в `mexporter.json` **до** экспорта. Для тестов передавайте отдельные `-config` и `-out`.
- **`-limit N`** ограничивает **цепочки**, а не сообщения — одна цепочка может дать несколько `.eml` файлов.
- **`-delay 0`** означает «прочитать `delay_ms` из конфига», а не «отключить паузу».
- **`-no-tui`** с пустым `folders` → exit **1**.
- PowerShell показывает `NativeCommandError` в **stderr** — проверьте `$LASTEXITCODE`, а не красный текст. Установите `[Console]::OutputEncoding=[Text.Encoding]::UTF8` перед просмотром кириллических имен файлов.
- `mail-export/` может вырасти до **~6,4 GB** и **127 k файлов**. Не сканируйте его рекурсивно — используйте `export-log.json` / `.jsonl` для статистики.
- TUI без реальной консоли переходит в **line‑fallback** (номированный ввод) — частичный piped‑выбор аннулирует работу. Не автоматизируйте селектор.

## IMAP Migration (scripts/)

После экспорта `.eml` файлов их можно загрузить на IMAP-сервер через `scripts/migrate_to_imap.py`:

```powershell
cd scripts
python migrate_to_imap.py --config config.json
```

См. [scripts/README.md](scripts/README.md) для полной документации.

## Output structure

```
mexporter/
  README.md             # этот README
  CHANGELOG.md          # история изменений
  RETROSPECTIVE.md      # полная история запусков
  run_mexporter.ps1     # автономный запуск
  .gitignore
  go/                   # исходный код (Go 1.27)
    cmd/mexporter/
    internal/
  scripts/
    migrate_to_imap.py  # IMAP migration script (Python stdlib)
    README.md           # документация для скрипта
  mail-export/          # экспорт .eml файлов (не в git)
  session/              # session.json (не в git — секрет)

mail-export/
  user@mail_ru_hosted_domain/
    Inbox/
      2012-07-05_Предложение о сотрудничестве с компанией MBA Strategy_13414968560000007864.eml
      2013-03-05_отчет по кадрам_13624642060000011907.eml
      ...
    Sent/
      ...
    export-log.json     # снимок (быстрые чтения)
    export-log.jsonl    # append‑only источник истины (resume)
  user@mail_ru_hosted_domain/
    Inbox/
      ...
  mexporter.json        # persisted config (email, out_dir, workers, delay, folders)
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Все хорошо — экспортирование завершено или перечисление сделано. |
| `1` | Ошибка (например, папка не найдена, IO‑ошибка). |
| `2` | **Auth expired** — токен в session.json устарел. Обновите как описано выше. |
| `130` | Аннулировано (например, Ctrl+C в TUI). |

## What else?

- `CHANGELOG.md` — история изменений проекта (даты, функции, исправления).
- `RETROSPECTIVE.md` — полная история запусков, метрики, ограничения (для разработчика/аналитика).
- `.opencode/NOTES.md` — Rolling session log (внутренний, сюда статус‑документы не пишите).
- Бинарник **самостоятельный** — после сборки `go build -o mexporter.exe ./cmd/mexporter` его можно скопировать на любую Windows‑машину с Go 1.27+ в PATH и запускать без OpenCode, Playwright или иного runtime. Ему нужно только `session/session.json` и флаги.

---
Built with Go 1.27, стандартная библиотека только. Backup sources in `.opencode/backup/`.

## run_mexporter.ps1 — автономный запуск без OpenCode

Скрипт `run_mexporter.ps1` позволяет запускать экспорт на любой Windows‑машине, где установлен Go 1.27+:
- Автоматически собирает `go\mexporter.exe`, если бинарник отсутствует.
- Запускает его с переданными флагами или заходит в TUI по умолчанию.
- Все пути относительно папки скрипта (`C:\path\to\mexporter`).

Использование:
```powershell
powershell -ExecutionPolicy Bypass -File run_mexporter.ps1 -list
```
или просто `.\run_mexporter.ps1 -list` из PowerShell."# escape-mailru"  git init git add README.md git commit -m "first commit" git branch -M main git remote add origin https://github.com/hzcht/escape-mailru.git git push -u origin main
