"""Тесты чистых функций мигратора (без сети и IMAP)."""

import importlib.util
import unittest
from email.parser import BytesHeaderParser
from pathlib import Path
from tempfile import TemporaryDirectory

MODULE_PATH = Path(__file__).with_name("migrate_to_imap.py")
SPEC = importlib.util.spec_from_file_location("migrate_to_imap", MODULE_PATH)
migrate = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(migrate)


class FakeMail:
    def __init__(self):
        self.created = []

    def create(self, name):
        self.created.append(name)
        return "OK", [b"CREATE completed"]


class MailboxTests(unittest.TestCase):
    def test_encode_decode_roundtrip(self):
        for name in ("INBOX", "Черновики", "A&B", "Archive/2025", "Отправленные 2025"):
            with self.subTest(name=name):
                self.assertEqual(
                    migrate.decode_mailbox(migrate.encode_mailbox(name)), name
                )

    def test_encode_ascii_unchanged(self):
        self.assertEqual(migrate.encode_mailbox("INBOX"), "INBOX")
        self.assertEqual(migrate.encode_mailbox("A&B"), "A&-B")

    def test_map_folder_name(self):
        cases = {
            "Входящие": "INBOX",
            "inbox": "INBOX",
            "Отправленные": "Sent",
            "Черновики": "Drafts",
            "Корзина": "Trash",
            "Спам": "Junk",
            "  Удалённые  ": "Trash",
            "Проект": "Проект",
        }
        for local, expected in cases.items():
            with self.subTest(local=local):
                self.assertEqual(migrate.map_folder_name(local), expected)

    def test_target_mailbox(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            (root / "Входящие").mkdir()
            (root / "Черновики").mkdir()
            (root / "Проект" / "2025").mkdir(parents=True)
            root_file = root / "root.eml"
            inbox_file = root / "Входящие" / "a.eml"
            draft_file = root / "Черновики" / "b.eml"
            nested_file = root / "Проект" / "2025" / "c.eml"
            for path in (root_file, inbox_file, draft_file, nested_file):
                path.write_bytes(b"From: a@b\r\n\r\nbody\r\n")

            self.assertEqual(
                migrate.target_mailbox(root_file, root, "INBOX", True, "/"), "INBOX"
            )
            self.assertEqual(
                migrate.target_mailbox(inbox_file, root, "INBOX", True, "/"), "INBOX"
            )
            self.assertEqual(
                migrate.target_mailbox(draft_file, root, "INBOX", True, "/"), "Drafts"
            )
            self.assertEqual(
                migrate.target_mailbox(nested_file, root, "INBOX", True, "/"),
                "Проект/2025",
            )
            self.assertEqual(
                migrate.target_mailbox(draft_file, root, "Archive", False, "/"),
                "Archive",
            )

    def test_resolve_mailbox_creates_parents(self):
        mail = FakeMail()
        folders = {}
        result = migrate.resolve_mailbox(mail, "Archive/2025", folders, "/")
        self.assertEqual(result, "Archive/2025")
        self.assertEqual(mail.created, ["Archive", "Archive/2025"])
        self.assertIn("archive/2025", folders)

    def test_resolve_mailbox_reuses_existing(self):
        mail = FakeMail()
        self.assertEqual(
            migrate.resolve_mailbox(mail, "Sent", {"sent": "Sent"}, "/"), "Sent"
        )
        self.assertEqual(
            migrate.resolve_mailbox(mail, "Sent", {"inbox.sent": "INBOX.Sent"}, "."),
            "INBOX.Sent",
        )
        self.assertEqual(mail.created, [])

    def test_parse_list_line(self):
        encoded = migrate.encode_mailbox("Отправленные")
        line = b'(\\HasNoChildren) "/" "' + encoded.encode("ascii") + b'"'
        flags, delimiter, name = migrate._parse_list_line(line)
        self.assertEqual(delimiter, "/")
        self.assertEqual(name, "Отправленные")
        self.assertIn("HasNoChildren", flags)


class DateTests(unittest.TestCase):
    def test_date_header_wins(self):
        raw = (
            b"Date: Sun, 14 Feb 2021 20:16:01 +0300\r\n"
            b"Received: by e.mail.ru with HTTP; Mon, 15 Feb 2021 10:00:00 +0300\r\n"
            b"\r\nbody\r\n"
        )
        msg = BytesHeaderParser().parsebytes(raw)
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "2020-01-01_1.eml"
            path.write_bytes(raw)
            dt = migrate.message_datetime(msg, path)
        self.assertEqual((dt.year, dt.month, dt.day), (2021, 2, 14))

    def test_received_fallback(self):
        raw = (
            b"Received: by e.mail.ru with HTTP; Sun, 14 Feb 2021 20:16:01 +0300\r\n"
            b"\r\nbody\r\n"
        )
        msg = BytesHeaderParser().parsebytes(raw)
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "2020-01-01_1.eml"
            path.write_bytes(raw)
            dt = migrate.message_datetime(msg, path)
        self.assertEqual((dt.year, dt.month, dt.day), (2021, 2, 14))

    def test_filename_fallback(self):
        raw = b"From: a@b\r\n\r\nbody\r\n"
        msg = BytesHeaderParser().parsebytes(raw)
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "2019-12-31_123.eml"
            path.write_bytes(raw)
            dt = migrate.message_datetime(msg, path)
        self.assertEqual((dt.year, dt.month, dt.day), (2019, 12, 31))

    def test_prepare_message_adds_date_only_when_missing(self):
        raw = (
            b"Received: by e.mail.ru with HTTP; Sun, 14 Feb 2021 20:16:01 +0300\r\n"
            b"Subject: draft\r\n\r\nbody\r\n"
        )
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "2021-02-14_1.eml"
            path.write_bytes(raw)
            prepared, internaldate, _dt = migrate.prepare_message(
                path, add_missing_date=True
            )
            kept, _internaldate2, _dt2 = migrate.prepare_message(
                path, add_missing_date=False
            )
        self.assertTrue(prepared.startswith(b"Date: "))
        self.assertIn(b"Subject: draft", prepared)
        self.assertTrue(prepared.endswith(raw))
        self.assertIn("2021", internaldate)
        self.assertEqual(kept, raw)

    def test_prepare_message_keeps_existing_date(self):
        raw = b"Date: Sun, 14 Feb 2021 20:16:01 +0300\r\nSubject: x\r\n\r\nbody\r\n"
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "x.eml"
            path.write_bytes(raw)
            prepared, internaldate, _dt = migrate.prepare_message(
                path, add_missing_date=True
            )
        self.assertEqual(prepared, raw)
        self.assertIn("2021", internaldate)


class JournalTests(unittest.TestCase):
    def test_key_depends_on_target_mailbox(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            path = root / "a.eml"
            path.write_bytes(b"From: a@b\r\n\r\nbody\r\n")
            inbox_key = migrate.message_key(path, root, "imap.test", "u@t", "INBOX")
            sent_key = migrate.message_key(path, root, "imap.test", "u@t", "Sent")
        self.assertNotEqual(inbox_key, sent_key)
        self.assertIn("INBOX", inbox_key)
        self.assertIn("Sent", sent_key)


if __name__ == "__main__":
    unittest.main()
