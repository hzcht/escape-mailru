#!/usr/bin/env python3
# migrate_to_imap.py
# Upload local .eml files to an IMAP mailbox.
#
# Usage:
#   python migrate_to_imap.py --config config.json
#
# Or with CLI arguments:
#   python migrate_to_imap.py --server imap.example.com --port 993 --login user@example.com --password "<password>" --folder "C:\path\to\exported-mail\Inbox"
#
# Requires: Python 3 standard library (imaplib, email, argparse, json, pathlib, etc.).

import argparse
import base64
import hashlib
import imaplib
import os
import re
import sys
import json
import socket
import time
from datetime import datetime, timezone
from email.parser import BytesHeaderParser
from email.utils import format_datetime, parsedate_to_datetime
from pathlib import Path

LOG_FILE = None
VERBOSE = False


# Символы, недопустимые в имени файла (Windows строже остальных ОС).
_INVALID_NAME_CHARS = set('<>:"/\\|?*')

# Зарезервированные имена файлов в Windows (CON, PRN, AUX, NUL, COM1...).
_RESERVED_NAME_BASES = {
    "con",
    "prn",
    "aux",
    "nul",
    *(f"com{i}" for i in range(1, 10)),
    *(f"lpt{i}" for i in range(1, 10)),
}

# Локальные имена папок → стандартные папки IMAP.
# Ключи нормализованы: нижний регистр, «ё» заменена на «е», пробелы схлопнуты.
_FOLDER_ALIASES = {
    "inbox": "INBOX",
    "входящие": "INBOX",
    "sent": "Sent",
    "sent items": "Sent",
    "sent mail": "Sent",
    "отправленные": "Sent",
    "drafts": "Drafts",
    "draft": "Drafts",
    "черновики": "Drafts",
    "trash": "Trash",
    "deleted items": "Trash",
    "deleted": "Trash",
    "корзина": "Trash",
    "удаленные": "Trash",
    "junk": "Junk",
    "spam": "Junk",
    "спам": "Junk",
}


def _safe_name_part(value, max_len=40):
    """Привести часть суффикса к виду, допустимому в имени файла."""
    cleaned = "".join(
        "_" if (ch in _INVALID_NAME_CHARS or ord(ch) < 32 or ord(ch) == 127) else ch
        for ch in value
    )
    # Windows не допускает имена/части, заканчивающиеся точкой или пробелом.
    cleaned = cleaned.strip().rstrip(".")
    # Зарезервированные слова ОС нельзя использовать как имя файла
    # (даже с расширением: "NUL.txt") — помечаем их подчёркиванием.
    if cleaned.split(".", 1)[0].lower() in _RESERVED_NAME_BASES:
        cleaned = "_" + cleaned
    # Ограничиваем длину, чтобы итоговый путь оставался в лимитах Windows.
    if len(cleaned) > max_len:
        cleaned = cleaned[:max_len].rstrip().rstrip(".")
    return cleaned or "default"


def _context_suffix(args):
    """Строковый суффикс для файлов состояния/лога на основе контекста."""
    # Используем server_login_mailbox, чтобы разные ящики писали в разные файлы.
    parts = [args.server, args.login, args.mailbox or "INBOX"]
    # Короткий дайджест исходного контекста: разные контексты не должны
    # сливаться в одно имя после обрезки/замены недопустимых символов.
    digest = hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()[:8]
    return "_" + "_".join(_safe_name_part(p) for p in parts) + "_" + digest


def diag(message):
    """Записать диагностическую строку с таймстампом и сразу сбросить буфер."""
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {message}"
    if LOG_FILE is not None:
        LOG_FILE.write(line + "\n")
        LOG_FILE.flush()
    if VERBOSE:
        print(line, flush=True)


def encode_mailbox(name):
    """Закодировать имя папки в modified UTF-7 (RFC 3501)."""
    out = []
    buf = []

    def flush():
        if not buf:
            return
        raw = "".join(buf).encode("utf-16-be")
        encoded = base64.b64encode(raw).decode("ascii").rstrip("=")
        out.append("&" + encoded.replace("/", ",") + "-")
        buf.clear()

    for ch in name:
        if 0x20 <= ord(ch) <= 0x7E:
            flush()
            out.append("&-" if ch == "&" else ch)
        else:
            buf.append(ch)
    flush()
    return "".join(out)


def decode_mailbox(name):
    """Декодировать modified UTF-7 в обычный Unicode."""

    def replace(match):
        token = match.group(1)
        if not token:
            return "&"
        encoded = token.replace(",", "/")
        encoded += "=" * (-len(encoded) % 4)
        return base64.b64decode(encoded).decode("utf-16-be")

    return re.sub(r"&([^-]*)-", replace, name)


def map_folder_name(name):
    """Сопоставить локальное имя папки стандартному имени IMAP."""
    key = " ".join(name.split()).casefold().replace("ё", "е")
    return _FOLDER_ALIASES.get(key, name.strip())


def target_mailbox(eml_path, folder_root, default_mailbox, preserve, delimiter):
    """Вычислить целевую IMAP-папку для файла по его пути в экспорте."""
    if not preserve:
        return default_mailbox
    try:
        rel = eml_path.resolve().relative_to(folder_root)
    except ValueError:
        return default_mailbox
    parent = rel.parent
    if parent == Path("."):
        return default_mailbox
    parts = list(parent.parts)
    mapped = map_folder_name(parts[0])
    rest = parts[1:]
    # У INBOX не может быть дочерних папок: вложенность под «Входящими»
    # поднимаем на верхний уровень.
    if mapped.upper() == "INBOX" and rest:
        return delimiter.join(rest)
    return delimiter.join([mapped, *rest])


def _parse_list_line(line):
    """Разобрать строку ответа IMAP LIST."""
    match = re.match(
        rb'\((?P<flags>[^)]*)\)\s+"(?P<delim>[^"]*)"\s+(?P<name>.+?)\s*$', line
    )
    if not match:
        return None
    name = match.group("name").strip()
    if name.startswith(b'"') and name.endswith(b'"') and len(name) >= 2:
        name = name[1:-1]
    try:
        name = decode_mailbox(name.decode("ascii"))
    except (UnicodeDecodeError, ValueError):
        name = name.decode("utf-8", "replace")
    return (
        match.group("flags").decode("ascii", "replace"),
        match.group("delim").decode("ascii", "replace"),
        name,
    )


def list_mailboxes(mail):
    """Получить разделитель иерархии и существующие папки сервера."""
    delimiter = "/"
    folders = {}
    try:
        typ, data = mail.list()
    except Exception as exc:
        diag(f"LIST failed: {exc}")
        return delimiter, folders
    if typ != "OK":
        diag(f"LIST returned {typ}: {data}")
        return delimiter, folders
    for line in data or []:
        if not isinstance(line, bytes):
            continue
        parsed = _parse_list_line(line)
        if not parsed:
            continue
        _flags, delim, name = parsed
        if delim:
            delimiter = delim
        folders.setdefault(name.casefold(), name)
    return delimiter, folders


def resolve_mailbox(mail, name, folders, delimiter):
    """Найти существующую папку или создать её (вместе с родителями)."""
    if name.upper() == "INBOX":
        return "INBOX"
    key = name.casefold()
    if key in folders:
        return folders[key]
    # Некоторые серверы держат личные папки внутри INBOX (INBOX.Sent).
    inbox_key = ("INBOX" + delimiter + name).casefold()
    if inbox_key in folders:
        return folders[inbox_key]

    parts = name.split(delimiter) if delimiter and delimiter in name else [name]
    current = []
    for part in parts:
        current.append(part)
        path = delimiter.join(current)
        if path.upper() == "INBOX" or path.casefold() in folders:
            continue
        typ, data = mail.create(encode_mailbox(path))
        message = b" ".join(
            item for item in (data or []) if isinstance(item, bytes)
        ).decode("utf-8", "replace")
        if typ != "OK" and "exist" not in message.casefold():
            raise RuntimeError(f"CREATE {path!r} failed: {typ} {message}")
        folders[path.casefold()] = path
    return name


def _parse_datetime(value):
    """Разобрать дату заголовка; вернуть aware datetime или None."""
    try:
        dt = parsedate_to_datetime(value)
    except (TypeError, ValueError, OverflowError):
        return None
    if dt is None:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    if not (1970 <= dt.year <= 2100):
        return None
    return dt


def message_datetime(msg, eml_path):
    """Дата письма: Date → Received → дата в имени файла → mtime."""
    dt = _parse_datetime(msg.get("Date") or "")
    if dt:
        return dt
    for received in msg.get_all("Received") or []:
        if ";" in received:
            candidate = " ".join(received.rsplit(";", 1)[1].split())
            dt = _parse_datetime(candidate)
            if dt:
                return dt
    match = re.match(r"^(\d{4})-(\d{2})-(\d{2})(?:_|$)", eml_path.name)
    if match:
        try:
            return datetime(
                int(match.group(1)),
                int(match.group(2)),
                int(match.group(3)),
                tzinfo=timezone.utc,
            )
        except ValueError:
            pass
    return datetime.fromtimestamp(eml_path.stat().st_mtime, tz=timezone.utc)


def prepare_message(eml_path, add_missing_date):
    """Прочитать .eml, вычислить INTERNALDATE и при необходимости добавить Date."""
    with open(eml_path, "rb") as f:
        raw = f.read()
    msg = BytesHeaderParser().parsebytes(raw)
    dt = message_datetime(msg, eml_path)
    has_date = bool(msg.get("Date") and msg.get("Date").strip())
    if add_missing_date and not has_date:
        header = format_datetime(dt)
        raw = b"Date: " + header.encode("ascii") + b"\r\n" + raw
        diag(f"DATE added file={eml_path} date={header}")
    return raw, imaplib.Time2Internaldate(dt.timestamp()), dt


def parse_args():
    parser = argparse.ArgumentParser(
        description="Миграция локальных .eml писем на IMAP-сервер"
    )
    parser.add_argument("--config", type=str, help="Путь к JSON-файлу конфигурации")
    parser.add_argument(
        "--server",
        type=str,
        default="imap.example.com",
        help="IMAP сервер (по умолчанию: imap.example.com)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=993,
        help="Порт IMAP (993 для SSL, 143 для plain) (по умолчанию: 993)",
    )
    parser.add_argument(
        "--login", type=str, default="", help="Email логин (если не указан — из config)"
    )
    parser.add_argument(
        "--password",
        type=str,
        default="",
        help="Email пароль (если не указан — из config)",
    )
    parser.add_argument(
        "--folder",
        type=str,
        default="",
        help="Локальная папка с .eml файлами (по умолчанию — текущая папка)",
    )
    parser.add_argument(
        "--mailbox",
        type=str,
        default="",
        help=(
            "Папка для файлов в корне --folder (по умолчанию: INBOX); "
            "при --flatten — для всех писем"
        ),
    )
    parser.add_argument(
        "--flatten",
        action="store_true",
        help="Сложить все письма в одну папку --mailbox (старое поведение)",
    )
    parser.add_argument(
        "--state",
        type=str,
        default="imap_upload_state.jsonl",
        help="Журнал загруженных писем; не перезаписывается при повторном запуске",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=60.0,
        help="Таймаут IMAP-операций в секундах (по умолчанию: 60)",
    )
    parser.add_argument(
        "--log",
        type=str,
        default="imap_migration.log",
        help="Диагностический лог с таймстампами (по умолчанию: imap_migration.log)",
    )
    parser.add_argument(
        "-v",
        "--verbose",
        action="store_true",
        help="Выводить диагностические строки также в консоль",
    )
    parser.add_argument(
        "--ssl", action="store_true", help="Использовать SSL/TLS соединение (порт 993)"
    )
    parser.add_argument(
        "--no-ssl",
        action="store_true",
        help="Использовать нешифрованное соединение (порт 143)",
    )
    parser.add_argument(
        "--keep-bytes",
        action="store_true",
        help=(
            "Не добавлять заголовок Date письмам без него "
            "(дата в клиенте может остаться 1970)"
        ),
    )
    return parser.parse_args()


def load_config(config_path):
    """Загрузка конфигурации из JSON файла."""
    config_path = Path(config_path)
    if not config_path.exists():
        print(f"❌ Файл конфигурации не найден: {config_path}")
        sys.exit(1)
    with open(config_path, "r", encoding="utf-8") as f:
        return json.load(f)


def connect_imap(server, port, ssl, timeout):
    """Подключение к IMAP серверу с таймаутом операций."""
    try:
        if ssl:
            mail = imaplib.IMAP4_SSL(server, port, timeout=timeout)
        else:
            mail = imaplib.IMAP4(server, port, timeout=timeout)
        print(f"✅ Успешное подключение к {server}:{port}")
        return mail
    except Exception as e:
        print(f"❌ Не удалось подключиться к {server}:{port}: {e}")
        return None


def authenticate(mail, login, password):
    """Авторизация на сервере."""
    try:
        mail.login(login, password)
        print(f"✅ Успешная авторизация как {login}")
        return True
    except imaplib.IMAP4.error as e:
        print(f"❌ Ошибка авторизации: {e}")
        return False


def find_eml_files(folder_path):
    """Найти все .eml файлы в указанной папке (рекурсивно)."""
    folder = Path(folder_path)
    if not folder.exists():
        print(f"❌ Папка с .eml файлами не существует: {folder}")
        return []
    eml_files = sorted(folder.rglob("*.eml"))
    print(f"📁 Найдено .eml файлов: {len(eml_files)}")
    return eml_files


def parse_eml_file(filepath):
    """Прочитать заголовки .eml, не разбирая тело и вложения."""
    try:
        with open(filepath, "rb") as f:
            msg = BytesHeaderParser().parse(f)
        return {
            "subject": msg.get("Subject", ""),
            "sender": msg.get("From", ""),
            "date": msg.get("Date", ""),
            "message_id": msg.get("Message-ID", ""),
            "file": str(filepath),
        }
    except Exception as e:
        print(f"⚠️ Ошибка парсинга {filepath}: {e}")
        return None


def upload_message(mail, folder_name, message_data, raw_msg, internaldate):
    """Загрузить готовые байты письма с вычисленным INTERNALDATE."""
    label = message_data.get("subject", "без темы")
    source = message_data.get("file", "")
    started = time.monotonic()
    diag(f"APPEND start mailbox={folder_name} date={internaldate} file={source}")
    try:
        # Пересборка Message меняет переводы строк, порядок заголовков и границы MIME,
        # поэтому отправляем исходные байты файла как есть.
        result, data = mail.append(
            encode_mailbox(folder_name),
            "\\Seen",  # метка прочитанного (можно изменить)
            internaldate,
            raw_msg,
        )
        elapsed = time.monotonic() - started

        if result == "OK":
            diag(f"APPEND ok in {elapsed:.2f}s mailbox={folder_name} file={source}")
            return True
        print(f"⚠️ IMAP APPEND вернул: {result} - {data}")
        diag(f"APPEND fail in {elapsed:.2f}s result={result} data={data} file={source}")
    except (socket.timeout, TimeoutError, imaplib.IMAP4.abort) as e:
        elapsed = time.monotonic() - started
        print(f"❌ Ошибка upload {label}: {e}")
        diag(f"APPEND disconnected in {elapsed:.2f}s file={source}: {e}")
        return "disconnected"
    except Exception as e:
        elapsed = time.monotonic() - started
        print(f"❌ Ошибка upload {label}: {e}")
        diag(f"APPEND error in {elapsed:.2f}s file={source}: {e}")
    return False


def load_uploaded(state_path):
    """Прочитать ключи уже загруженных писем из журнала."""
    uploaded = set()
    path = Path(state_path)
    if not path.exists():
        return uploaded
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                key = json.loads(line).get("key")
            except (json.JSONDecodeError, AttributeError):
                continue
            if key:
                uploaded.add(key)
    return uploaded


def message_key(eml_path, folder_root, server, login, mailbox):
    """Стабильный ключ: назначение + относительный путь + размер файла."""
    try:
        rel = eml_path.resolve().relative_to(folder_root).as_posix()
    except ValueError:
        rel = eml_path.name
    return f"{server}|{login}|{mailbox}|{rel}|{eml_path.stat().st_size}"


def record_upload(state_file, key, eml_path):
    """Дописать одну строку в журнал и сразу сбросить буфер."""
    record = {
        "key": key,
        "file": str(eml_path),
        "size": eml_path.stat().st_size,
        "uploaded_at": time.strftime("%Y-%m-%dT%H:%M:%S"),
    }
    state_file.write(json.dumps(record, ensure_ascii=False) + "\n")
    state_file.flush()


def reconnect_imap(mail, server, port, use_ssl, timeout, login, password):
    """Закрыть зависшее соединение и открыть новое."""
    diag("RECONNECT closing old connection")
    for method in ("close", "logout"):
        try:
            getattr(mail, method)()
        except Exception:
            pass
    new_mail = connect_imap(server, port, use_ssl, timeout)
    if new_mail is None:
        diag("RECONNECT failed: no connection")
        return None
    if not authenticate(new_mail, login, password):
        diag("RECONNECT failed: authentication")
        return None
    diag("RECONNECT ok")
    return new_mail


def main():
    global LOG_FILE, VERBOSE
    args = parse_args()
    VERBOSE = args.verbose

    # Добавляем суффикс контекста (server_login_mailbox) к именам файлов по умолчанию,
    # чтобы параллельные запуски для разных ящиков писали в разные файлы.
    # Пользователь, явно передавший --log/--state, получит свои файлы без суффикса.
    ctx = _context_suffix(args)

    def _suffix_basename(path_str, default):
        p = Path(path_str)
        if path_str != default:
            return p  # явный путь не трогаем
        name = p.name  # просто имя файла (без пути)
        if ctx:
            # Вставляем суффикс перед расширением
            if "." in name:
                stem, ext = name.rsplit(".", 1)
                name = stem + ctx + "." + ext
            else:
                name = name + ctx
        return p.parent / name

    log_path = _suffix_basename(args.log, "imap_migration.log")
    state_path = _suffix_basename(args.state, "imap_upload_state.jsonl")
    log_path.parent.mkdir(parents=True, exist_ok=True)
    state_path.parent.mkdir(parents=True, exist_ok=True)
    LOG_FILE = open(log_path, "a", encoding="utf-8")

    # Загрузка конфига если указан
    config = None
    if args.config:
        config = load_config(args.config)
        # Переопределение аргументами из командной строки
        if not args.login and config.get("login"):
            args.login = config["login"]
        if not args.password and config.get("password"):
            args.password = config["password"]
        if not args.folder and config.get("eml_folder"):
            args.folder = config["eml_folder"]
        if not args.server and config.get("server"):
            args.server = config["server"]
        if not args.port and config.get("port"):
            args.port = config["port"]

    # Определяем SSL
    use_ssl = args.ssl
    if args.no_ssl:
        use_ssl = False
    elif args.port == 143 and not args.ssl and not args.no_ssl:
        use_ssl = False  # default for 143

    # Определяем логин/пароль
    login = args.login
    password = args.password

    if not login:
        print("❌ Логин не указан. Используйте --login или настройте config.json")
        sys.exit(1)

    # Находим .eml файлы
    eml_files = find_eml_files(args.folder)
    if not eml_files:
        print("ℹ️ Файлов для миграции не найдено — выход.")
        sys.exit(0)

    # Подключение к IMAP
    mail = connect_imap(args.server, args.port, use_ssl, args.timeout)
    if mail is None:
        sys.exit(1)

    if not authenticate(mail, login, password):
        sys.exit(1)

    # --folder — локальный путь; --mailbox — отдельный параметр IMAP-папки.
    mailbox = args.mailbox or "INBOX"
    folder_root = Path(args.folder).resolve()
    preserve_folders = not args.flatten
    state_path.parent.mkdir(parents=True, exist_ok=True)
    uploaded = load_uploaded(state_path)

    if preserve_folders:
        delimiter, server_folders = list_mailboxes(mail)
        print(
            "📁 Структура папок сохраняется; стандартные имена маппятся на "
            "INBOX/Sent/Drafts/Trash/Junk"
        )
    else:
        delimiter, server_folders = "/", {}
        print(f"📤 Все письма загружаются в папку: {mailbox}")

    print(
        f"🔗 Сервер: {args.server}:{args.port} (SSL={use_ssl}, таймаут {args.timeout:g}с)"
    )
    print(f"👤 Пользователь: {login}")
    print(f"📊 Всего писем: {len(eml_files)}")
    print(f"📄 В журнале уже загружено: {len(uploaded)}")
    print("-" * 60)
    diag(
        f"START server={args.server}:{args.port} ssl={use_ssl} mailbox={mailbox} "
        f"flatten={args.flatten} timeout={args.timeout:g}s"
    )
    diag(
        f"CONFIG folder={folder_root} state={state_path} log={log_path} "
        f"total={len(eml_files)} journal={len(uploaded)}"
    )
    if preserve_folders:
        diag(f"FOLDERS delimiter={delimiter!r} existing={len(server_folders)}")

    success_count = 0
    fail_count = 0
    skipped_count = 0
    already_count = 0

    state_file = open(state_path, "a", encoding="utf-8")
    try:
        for i, eml_path in enumerate(eml_files, 1):
            target = target_mailbox(
                eml_path, folder_root, mailbox, preserve_folders, delimiter
            )
            key = message_key(eml_path, folder_root, args.server, login, target)
            diag(
                f"[{i}/{len(eml_files)}] process file={eml_path} "
                f"mailbox={target} size={eml_path.stat().st_size}"
            )
            if key in uploaded:
                diag(f"[{i}/{len(eml_files)}] skip journal file={eml_path}")
                already_count += 1
                if i % 10 == 0 or i == len(eml_files):
                    print(
                        f"\r📥 Прогресс: {i}/{len(eml_files)} "
                        f"✓{success_count} ⚠{fail_count} ↩{already_count} ⏺{skipped_count}",
                        end="",
                        flush=True,
                    )
                continue

            # Парсим заголовки
            msg_data = parse_eml_file(eml_path)
            if msg_data is None:
                diag(f"[{i}/{len(eml_files)}] parse failed file={eml_path}")
                skipped_count += 1
                continue

            # Готовим байты и INTERNALDATE (Date добавляется только при отсутствии)
            try:
                raw_msg, internaldate, _dt = prepare_message(
                    eml_path, add_missing_date=not args.keep_bytes
                )
            except Exception as exc:
                fail_count += 1
                print(f"❌ Не удалось прочитать {eml_path}: {exc}")
                diag(f"READ error file={eml_path}: {exc}")
                continue

            # Папку создаём только для писем, которые реально будем загружать.
            if preserve_folders:
                try:
                    target = resolve_mailbox(mail, target, server_folders, delimiter)
                except Exception as exc:
                    fail_count += 1
                    print(f"❌ Не удалось создать папку {target}: {exc}")
                    diag(f"MAILBOX error target={target} file={eml_path}: {exc}")
                    continue

            # Загрузка письма
            result = upload_message(mail, target, msg_data, raw_msg, internaldate)
            if result is True:
                success_count += 1
                uploaded.add(key)
                record_upload(state_file, key, eml_path)
                diag(f"[{i}/{len(eml_files)}] journal written file={eml_path}")
            else:
                fail_count += 1
                if result == "disconnected":
                    print("🔄 Переподключение после обрыва соединения...", flush=True)
                    mail = reconnect_imap(
                        mail,
                        args.server,
                        args.port,
                        use_ssl,
                        args.timeout,
                        login,
                        password,
                    )
                    if mail is None:
                        print(
                            "❌ Не удалось переподключиться — выход. Журнал сохранён."
                        )
                        break

            # Прогресс-бар каждые 10 писем или в конце
            if i % 10 == 0 or i == len(eml_files):
                print(
                    f"\r📥 Прогресс: {i}/{len(eml_files)} "
                    f"✓{success_count} ⚠{fail_count} ↩{already_count} ⏺{skipped_count}",
                    end="",
                    flush=True,
                )

            # Небольшая пауза, чтобы не перегружать сервер
            time.sleep(0.1)
    except KeyboardInterrupt:
        diag("INTERRUPTED by user; journal is safe")
        print(
            "\n⏹ Прервано пользователем; журнал сохранён, повторный запуск продолжит с места."
        )
    finally:
        state_file.close()
        diag(
            f"FINISH loaded={success_count} skipped_journal={already_count} "
            f"failed={fail_count} parse_failed={skipped_count} total={len(eml_files)}"
        )
        if LOG_FILE is not None:
            diag("STOP closing diagnostic log")
            LOG_FILE.close()
            LOG_FILE = None
        try:
            mail.close()
            mail.logout()
            print("🔒 Соединение закрыто.")
        except Exception:
            pass

    diag(
        f"FINISH loaded={success_count} skipped_journal={already_count} "
        f"failed={fail_count} parse_failed={skipped_count} total={len(eml_files)}"
    )
    print(f"\n" + "-" * 60)
    print(f"✅ Миграция завершена:")
    print(f"   Загружено сейчас: {success_count}")
    print(f"   Уже было в журнале: {already_count}")
    print(f"   Ошибок:  {fail_count}")
    print(f"   Пропущено (парсинг): {skipped_count}")
    print(f"   Всего: {len(eml_files)}")


if __name__ == "__main__":
    args = parse_args()
    main()
