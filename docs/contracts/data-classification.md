# Data Classification Contract

**Status:** TODO — placeholder
**Target artifact:** Classification reference table
**Owner:** `sdk-architect` agent + `security` agent

## Purpose

Labels every persisted field with a classification tier — **P0**, **P1**, or **P2** — driving logging redaction rules, encryption-at-rest boundaries, access-log requirements, and role-mask visibility. This contract is consulted by `contract-guard` on every PR that adds or modifies a persisted field.

## Classification tiers (per NFR-3.9)

- **P0 (Public)** — may appear in logs, no encryption required. Examples: VIN last-4, vehicle name, vehicle model.
- **P1 (Sensitive, encrypted at rest)** — AES-256-GCM column-level encryption. Never in logs. Examples: GPS coordinates, destination data, OAuth tokens.
- **P2 (Sensitive + access-logged)** — P1 requirements plus every read/write writes an access-log entry. Reserved for future fields (e.g., payment info, health data).

## Anchored requirements

- **NFR-3.9** — tier definitions
- **NFR-3.22** — TLS in transit for all connections
- **NFR-3.23** — AES-256-GCM column-level encryption for P1 fields (OAuth tokens, GPS coordinates, destination coordinates, route points)
- **NFR-3.24** — encryption key stored as Fly.io secret (`ENCRYPTION_KEY`)
- **NFR-3.25** — encryption transparent to SDK (server store layer only)
- **NFR-3.26** — key rotation strategy (separate contract doc)

## Sections to author (TODO)

- [ ] Per-field classification table (every column in every persisted table)
- [ ] Redaction rules for each tier (log handlers, error messages, crash reports)
- [ ] Encryption scope mapping (which P1 fields ↔ which DB columns)
- [ ] Key rotation procedure reference
- [ ] Rules for classifying new fields (checklist added by `contract-guard`)
