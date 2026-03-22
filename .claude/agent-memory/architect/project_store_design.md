---
name: Store Layer Design (Issue #4)
description: Database layer design for internal/store/ - DB wrapper, VehicleRepo, DriveRepo, consumer-site interfaces, no new tables, Prisma camelCase quoting, JSONB route points. Designed 2026-03-17.
type: project
---

## Store Layer Design Decisions

Designed for Issue #4 on 2026-03-17. Database persistence layer using pgx against shared Supabase PostgreSQL.

### Key Decisions

1. **Thin DB wrapper** around pgxpool.Pool for health check, close, and pool stats -- NOT a full DBTX abstraction
2. **Repos receive `*pgxpool.Pool`** (not DB wrapper) -- keeps repos decoupled from wrapper lifecycle
3. **No new tables** -- telemetry server reads/writes Prisma-owned "Vehicle" and "Drive" tables only
4. **Interfaces at consumer site** per CLAUDE.md: drives/ defines VehicleReader+DriveWriter, ws/ defines VehicleLookup
5. **Store package does NOT import events package** -- wiring happens in a Subscriber struct (future issue) or main.go
6. **Prisma camelCase quoting** -- all SQL uses `"columnName"` syntax with quoted identifiers
7. **No cross-repo transactions yet** -- individual ops are single-statement; eventual consistency is acceptable since next telemetry event corrects state
8. **Dynamic UPDATE builder** for VehicleRepo.UpdateTelemetry -- hardcoded column allowlist, NOT user input
9. **Route points stored as JSONB append** (`||` operator) to match existing Next.js frontend contract
10. **StoreMetrics interface** following same pattern as BusMetrics -- NoopStoreMetrics for tests
11. **Types in types.go**: Vehicle, VehicleUpdate, VehicleStatus, DriveRecord, DriveCompletion, RoutePointRecord

### Files

8 files in internal/store/, all under 130 lines. Plus migrations/.gitkeep placeholder.

### Design Doc

Full design at docs/design/004-database-layer.md
