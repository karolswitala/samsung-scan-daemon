# TODO

_No open items._

Resolved 2026-07-24:
- Re-registration after printer power-cycle — the SNMP poll loop now detects the
  printer no longer answering for our OID and re-registers on recovery
  (`internal/snmp`, `cmd/samsung-scan/main.go`).
- H2 log path — moved off `/tmp` to per-user `~/Library/Logs/samsung-scan.log` as
  part of the per-user LaunchAgent migration (see `SECURITY_AUDIT.md`).
