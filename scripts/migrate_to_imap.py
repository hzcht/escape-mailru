#!/usr/bin/env python3
# migrate_to_imap.py
# Миграция почты с локальных .eml файлов на IMAP-сервер.
#
# Использование:
#   python migrate_to_imap.py --config config.json
#
# Или с передачей параметров командной строки:
#   python migrate_to_imap.py --server imap.spaceweb.ru --port 993 --login user@domain.ru --password --folder C:\mail-export\user@mail_ru_hosted_domain\Inbox
#
# Требуется: Python 3 standard library (imaplib, email, os, argparse).
# Примечание: для SMTP-сервера или другого порта может потребоваться adjustment.

import argparse
import imaplib
import email
import os
import sys
import json
import time
from pathlib import Path


def parse_args():
    parser = argparse.ArgumentParser(
        description="Миграция локальных .eml писем на IMAP-сервер"
    )
    parser.add_argument("--config", type=str, help="Путь к JSON-файлу конфигурации")
    parser.add_argument(
        "--server",
        type=str,
        default="imap.spaceweb.ru",
        help="IMAP сервер (по умолчанию: imap.spaceweb.ru)",
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
        help="Email пароль (если не указан — из config, промптом или env)",
    )
    parser.add_argument(
        "--folder",
        type=str,
        default="",
        help="Лocal папка с .eml файлами (по умолчанию — текущая папка скрипта)",
    )
    parser.add_argument(
        "--ssl", action="store_true", help="Использовать SSL/TLS соединение (порт 993)"
    )
    parser.add_argument(
        "--no-ssl",
        action="store_true",
        help="Использовать нешифрованное соединение (порт 143)",
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


def connect_imap(server, port, ssl):
    """Подключение к IMAP серверу."""
    try:
        if ssl:
            mail = imaplib.IMAP4_SSL(server, port)
        else:
            mail = imaplib.IMAP4(server, port)
        print(f"✅ Успешное подключение к {server}:{port}")
        return mail
    except Exception as e:
        print(f"❌ Не удалось подключиться к {server}:{port}: {e}")
        sys.exit(1)


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
    """Парсинг .eml файла и извлечение заголовков."""
    try:
        with open(filepath, "rb") as f:
            raw = f.read()
        msg = email.message_from_bytes(raw)
        # Извлекаем основные заголовки
        subject = ""
        sender = ""
        date = ""
        message_id = ""

        if msg["Subject"]:
            subject = msg["Subject"]
        if msg["From"]:
            sender = msg["From"]
        if msg["Date"]:
            date = msg["Date"]
        if msg["Message-ID"]:
            message_id = msg["Message-ID"]

        # Конвертируем body в plain текст
        body = ""
        if msg.is_multipart():
            for part in msg.walk():
                content_type = part.get_content_type()
                if content_type == "text/plain":
                    charset = part.get_content_charset()
                    payload = part.get_payload(decode=True)
                    if payload:
                        try:
                            body = payload.decode(charset or "utf-8", errors="replace")
                        except Exception:
                            body = payload.decode("utf-8", errors="replace")
                    break
        else:
            charset = msg.get_content_charset()
            payload = msg.get_payload(decode=True)
            if payload:
                try:
                    body = payload.decode(charset or "utf-8", errors="replace")
                except Exception:
                    body = payload.decode("utf-8", errors="replace")

        return {
            "subject": subject,
            "sender": sender,
            "date": date,
            "message_id": message_id,
            "body": body,
            "file": str(filepath),
        }
    except Exception as e:
        print(f"⚠️ Ошибка парсинга {filepath}: {e}")
        return None


def upload_message(mail, folder_name, message_data):
    """Загрузка одного письма на IMAP сервер."""
    try:
        # Формируем RFC 822 сообщение
        raw_msg = email.message_as_bytes(message_data)

        # Отправка через IMAP APPEND
        # Используем кодировку UTF-7 как требует IMAP, но email.message_as_bytes уже в bytes
        result, data = mail.append(
            folder_name,
            "\\Seen",  # метка прочитанного (можно изменить)
            raw_msg,
            None,
        )

        if result == "OK":
            return True
        else:
            print(f"⚠️ IMAP APPEND вернул: {result} - {data}")
            return False
    except Exception as e:
        print(f"❌ Ошибка upload {message_data.get('subject', 'без темы')}: {e}")
        return False


def main():
    args = parse_args()

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
    mail = connect_imap(args.server, args.port, use_ssl)

    if not authenticate(mail, login, password):
        sys.exit(1)

    # Определяем целевую папку
    target_mailbox = args.folder if args.folder else "INBOX"
    # Нормализация папки для IMAP
    if target_mailbox.upper() == "INBOX":
        mailbox = "INBOX"
    else:
        mailbox = target_mailbox

    print(f"📤 Начинаем миграцию в папку: {mailbox}")
    print(f"🔗 Сервер: {args.server}:{args.port} (SSL={use_ssl})")
    print(f"👤 Пользователь: {login}")
    print(f"📊 Всего писем: {len(eml_files)}")
    print("-" * 60)

    success_count = 0
    fail_count = 0
    skipped_count = 0

    for i, eml_path in enumerate(eml_files, 1):
        # Парсим файл
        msg_data = parse_eml_file(eml_path)
        if msg_data is None:
            skipped_count += 1
            continue

        subject = msg_data["subject"] or "Без темы"
        # Ограничиваем длину subject для лога
        subj_display = (subject[:40] + "...") if len(subject) > 40 else subject

        # Загрузка письма
        ok = upload_message(mail, mailbox, msg_data)

        if ok:
            success_count += 1
        else:
            fail_count += 1

        # Прогресс-бар каждые 10 писем или в конце
        if i % 10 == 0 or i == len(eml_files):
            print(
                f"\r📥 Прогресс: {i}/{len(eml_files)} "
                f"✓{success_count} ⚠{fail_count} ⏺{skipped_count}",
                end="",
            )

        # Небольшая пауза, чтобы не перегружать сервер
        time.sleep(0.1)

    print(f"\n" + "-" * 60)
    print(f"✅ Миграция завершена:")
    print(f"   Успешно: {success_count}")
    print(f"   Ошибок:  {fail_count}")
    print(f"   Пропущено: {skipped_count}")
    print(f"   Всего: {len(eml_files)}")

    # Закрываем соединение
    try:
        mail.close()
        mail.logout()
        print("🔒 Соединение с закрыто.")
    except Exception:
        pass


if __name__ == "__main__":
    args = parse_args()
    main()
