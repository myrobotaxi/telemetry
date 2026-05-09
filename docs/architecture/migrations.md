# Database Migration Strategy

**Status:** Active — v1
**Owner:** `infra` agent
**Last updated:** 2026-05-09 (MYR-74)

## 1. Tool decision: golang-migrate

### Decision

Use **`github.com/golang-migrate/migrate/v4`** with the `iofs` source adapter and the `pgx/v5` database driver.

### Rationale

| Criterion | golang-migrate | tern | atlas |
|-----------|---------------|------|-------|
| Go-native embed.FS support | Yes (iofs adapter) | No | No |
| Active maintenance | Yes (v4, actively maintained) | Moderate | Yes |
| pgx/v5 driver | Yes (official) | pgx/v4 only | Yes |
| Zero external process | Yes | Yes | No (binary required) |
| Up/down migration files | Yes | Yes | Partial |
| Complexity/surface area | Low | Low | High (HCL DSL) |
| Already named in CLAUDE.md | **Yes** | No | No |

`atlas` offers schema diffing and declarative migrations but introduces an external binary dependency and HCL DSL overhead that is disproportionate for this service's migration volume. `tern` lacks a pgx/v5 driver. `golang-migrate` is the natural choice: it is already named in CLAUDE.md as the mandated tool, it integrates cleanly with `embed.FS`, and its operational model (numbered up/down SQL files) is well understood.

### Anchored NFRs

- NFR-3.3 — DB snapshots MUST be self-consistent (partial groups invalid). Fail-fast startup enforces this.
- NFR-3.28 — Raw telemetry NOT persisted. Go-owned tables hold only metadata, never telemetry history.

---

## 2. File layout

Migration SQL files live in `internal/store/migrations/`:

```
internal/store/migrations/
  0001_init.up.sql      — bootstraps _telemetry_server_meta (placeholder)
  0001_init.down.sql    — drops _telemetry_server_meta
  NNNN_<name>.up.sql    — future migrations (sequential, zero-padded)
  NNNN_<name>.down.sql  — rollback for each migration
```

The `migrationFiles` embed.FS in `internal/store/migrations.go` compiles all `*.sql` files into the binary at build time. No external migration files are required at runtime.

---

## 3. Startup wiring

`store.RunMigrations(ctx, dbURL, logger)` is called in `cmd/telemetry-server/main.go`:

- **After** `store.NewDB` — the connection pool is ready.
- **Before** any repository, event bus, or handler is registered.
- **Fail-fast** — any error except `migrate.ErrNoChange` causes the server to exit. There is no safe degraded mode when the schema is wrong.

`migrate.ErrNoChange` (already up-to-date) is treated as success and returns `nil`.

---

## 4. Go-owned vs. Prisma-owned coexistence rule

The telemetry server shares the same Supabase PostgreSQL database as the Next.js app, whose schema is managed by Prisma.

### 4.1 Table namespace separation

| Owner | Convention | Examples |
|-------|------------|---------|
| **Go (telemetry server)** | Prefix `_telemetry_` or `go_` | `_telemetry_server_meta` |
| **Prisma (Next.js app)** | PascalCase, no prefix | `User`, `Vehicle`, `Drive`, `AuditLog` |

The prefix convention ensures that `prisma db pull` output can be filtered without risk of touching Prisma-owned tables. It also makes table ownership immediately visible in `psql \dt` output.

### 4.2 Prisma-owned table list (immutable from Go migrations)

The following tables are owned by the Next.js app's Prisma schema. Go migration SQL MUST NEVER reference these table names (CREATE, ALTER, DROP, or INSERT/UPDATE/DELETE in a migration file):

| Table | Owner |
|-------|-------|
| `User` | Prisma |
| `Account` | Prisma |
| `Session` | Prisma |
| `VerificationToken` | Prisma |
| `Vehicle` | Prisma |
| `Drive` | Prisma |
| `TripStop` | Prisma |
| `Invite` | Prisma |
| `Settings` | Prisma |
| `AuditLog` | Prisma |

> The Go store layer holds read access (FK resolution) and narrow insert-only access (AuditLog) to some Prisma tables via regular application queries — NOT via migration files. Migration SQL is for schema changes only. Application queries against Prisma tables are a separate concern and are governed by `docs/contracts/data-lifecycle.md` §1.4.

### 4.3 CG-DL-9: enforcement (contract-guard)

Contract-guard rule CG-DL-9 (see `docs/contracts/data-lifecycle.md` §7) enforces the above at PR-time:

- CI grep pass: any SQL file in `internal/store/migrations/*.sql` that references a Prisma-owned table name (case-insensitive) causes the `contract-guard` CI step to fail with a non-zero exit.
- Session-time: the `contract-guard` agent checks the same condition before a PR is opened.

### 4.4 Writing a new Go-owned migration

1. Add `NNNN_<name>.up.sql` and `NNNN_<name>.down.sql` to `internal/store/migrations/`.
2. Name the new table with the `_telemetry_` or `go_` prefix.
3. Do NOT reference any Prisma-owned table name in the SQL.
4. Verify locally: `go run ./cmd/telemetry-server --dev` — the server logs `database migrations applied` on startup.
5. Verify idempotency: run a second time and confirm `already up-to-date` is logged (no error).

---

## 5. Rollback strategy

golang-migrate supports `m.Down()` for rolling back one or all migrations. In production:

- Rollback is a manual operator action, not automated.
- Each migration MUST have a corresponding `.down.sql` that cleanly reverses the `.up.sql`.
- For purely additive migrations (new table, new index), the down file drops the object (`DROP TABLE IF EXISTS ...`).
- Rollback of data-modifying migrations (if ever needed) requires a coordinated deploy.

---

## 6. Schema tracking table

golang-migrate maintains its own tracking table (`schema_migrations`) in the database. This table is owned by golang-migrate, not by Prisma. It follows the Prisma table naming convention by coincidence (lowercase, no prefix) but Prisma never manages it. Do not add it to the Prisma schema.
