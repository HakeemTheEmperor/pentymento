# Security policy

## Reporting a vulnerability

Send reports to **security@pentymento.dev**. Do not open a public issue for a suspected vulnerability.

Include, where you can:

- the Pentymento version (`pentym --version`) and platform;
- what you found and its impact;
- steps to reproduce it.

Use demo hostnames and fake credentials in any example. Never send real secrets or row data.

## Scope

Pentymento's design goal is that a compromised database host can neither read nor delete backup history. Reports that break any part of that goal, or that expose a secret on a command line, in logs, journal entries or notifications, are in scope.
