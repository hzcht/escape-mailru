# Mail migration helpers

## `migrate_to_imap.py`

Uploads local `.eml` files to an IMAP mailbox using only the Python standard library.

### Requirements

- Python 3
- Access to an IMAP server (SSL or plain connection)
- Valid mailbox credentials

### Configuration

You can provide settings via a JSON config file, command-line arguments, or a mix of both (CLI arguments override config values).

Example `config.json`:

```json
{
  "server": "imap.example.com",
  "port": 993,
  "login": "user@example.com",
  "password": "<password>",
  "eml_folder": "C:\\path\\to\\exported-mail"
}
```

Do not store real credentials in version-controlled files.

### Usage

From the `scripts` directory:

```powershell
# Using a config file
python migrate_to_imap.py --config config.json

# Using CLI arguments
python migrate_to_imap.py --server imap.example.com --port 993 --login user@example.com --password "<password>" --folder "C:\path\to\exported-mail"
```

### Common arguments

| Argument | Description |
| --- | --- |
| `--config` | Path to JSON configuration file |
| `--server` | IMAP server hostname |
| `--port` | IMAP port (typically `993` for SSL, `143` for plain) |
| `--login` | Mailbox login/username |
| `--password` | Mailbox password (prefer secure input/storage practices) |
| `--folder` | Directory containing `.eml` files (scanned recursively) |
| `--ssl` / `--no-ssl` | Force SSL/TLS on or off |

### Behavior

- Scans the target folder recursively for `.eml` files.
- Parses each file and appends it to the target IMAP mailbox.
- Logs progress and continues when individual files fail.
- Prints a summary of successes, failures, and skipped files.

### Notes

- Message parsing/upload behavior depends on message structure and server capabilities.
- For large migrations, consider testing with a small subset first.
- Keep backups of source `.eml` files before migration.
