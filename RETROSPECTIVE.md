# Retrospective

## Summary

This project evolved from manual/UI-driven mailbox export experiments into a CLI exporter that uses direct API calls, deterministic enumeration, and resumable journaling. The main goals were correctness, resumability, safe read-only operation on large mailboxes, and predictable day-to-day usability.

## Key lessons

1. **Direct API downloads scale better than UI automation.** Automating UI actions for each message is too slow and brittle for large exports.
2. **Ascending enumeration is safer for long runs.** Descending pagination with offsets can miss or duplicate work when new mail arrives during export.
3. **Stable message identity matters.** Provider uidl is the reliable identifier for deduplication and file naming decisions.
4. **Journaling enables recovery.** An append-only log plus a snapshot allows resume, retry, and progress inspection after interruptions.
5. **Read-only guarantees reduce risk.** Restricting operations to listing and downloading avoids accidental mailbox mutations.

## Architecture outcomes

- Clear package boundaries: session/loading, API client, layout, config/UI, journaling, and export orchestration.
- Worker-based downloading with retry and backoff behavior.
- Atomic file writes and basic integrity checks before trusting downloaded artifacts.
- Interactive and non-interactive execution modes.

## Usability and usage scenarios

### First run (mode 0)
- Session setup is a guided flow: open the login page, sign in manually, import session JSON, save it, and continue.
- Required session fields are validated before saving; invalid input fails early with a clear error.
- The session file is written atomically and treated as secret.

### Daily operation
- Interactive folder selection is the default; `-folders` and `-no-tui` cover scripted/non-interactive runs.
- Folder selection is persisted before export, so an interrupted run can be restarted with the same config.
- `-list` provides a quick, read-only way to inspect folder IDs and message counts.
- Exit codes distinguish success, usage/runtime errors, expired authentication, and user cancellation.

### Safe experimentation
- `-dry-run -v` allows enumeration checks without downloading files.
- `-limit N` makes short trial runs practical without changing the overall export plan.
- Separate `-config` and `-out` paths allow throwaway tests without touching the main export.

### Resume and recovery
- Re-running an interrupted export resumes from the journal: completed messages are skipped, failed ones are retried.
- A `downloaded` record is only trusted when the corresponding file exists and is non-empty.
- The journal layout (JSONL source of truth plus JSON snapshot) keeps progress inspectable even for large exports.

### Multiple mailboxes
- Separate `-session`, `-config`, and `-out` paths per mailbox keep state isolated.
- A clean start for a new mailbox does not require deleting existing exports, only using fresh paths.

### Troubleshooting
- Expired authentication is surfaced as exit code `2` with guidance to refresh the session.
- PowerShell-specific stderr noise is documented so users check exit codes instead of red output.
- Large export directories are documented as log-inspection targets rather than recursive-scan targets.

## Operational notes

- Session/authentication material expires and must be refreshed (for example via `-init-session`).
- Very large exports require careful log-based inspection rather than full directory scans.
- Running concurrent exporter instances against the same output is unsupported.

## Possible improvements

- Verification mode to recheck existing exported files against journal records.
- Adaptive concurrency/rate handling based on API responses.
- Faster startup/resume strategies for very large journals.
- More ergonomic session refresh (fewer manual steps) while keeping credentials local.
- Richer progress reporting and summary output for long unattended runs.
