# Robo-Taxi Telemetry — Project Rules

## Project Overview

A Go service that receives real-time vehicle telemetry from Tesla's Fleet Telemetry system and broadcasts it to connected clients via WebSocket. This is the real-time backend for the MyRoboTaxi web app.

### System Context

```
Tesla Vehicle ──mTLS/WSS──► Telemetry Server ──WSS──► Browser (MyRoboTaxi Next.js app)
                                    │
                                    ├──► PostgreSQL (Supabase — shared with Next.js app)
                                    └──► Prometheus metrics
```

- **Upstream:** Tesla vehicles push protobuf telemetry over mTLS WebSocket
- **Downstream:** Browser clients connect via authenticated WebSocket
- **Persistence:** Shared Supabase PostgreSQL database (same as MyRoboTaxi Next.js app)
- **Partner app:** `my-robo-taxi` Next.js app at `../my-robo-taxi/`

## Architecture Rules (Enforced)

### Project Structure

```
cmd/
  telemetry-server/       → Binary entrypoint only. No business logic.
  testbench/              → TUI dashboard for live telemetry inspection (dev tool)
  simulator/              → Mock Tesla vehicle that sends fake telemetry (dev tool)
internal/
  telemetry/              → Tesla Fleet Telemetry receiver (mTLS, protobuf decode)
  events/                 → Event bus, domain event types, dispatcher
  drives/                 → Drive detection state machine
  ws/                     → WebSocket server for browser clients
  store/                  → Database persistence (pgx, repository pattern)
  auth/                   → Client authentication, token validation
  simulator/              → Mock vehicle telemetry generator (shared with cmd/simulator)
  testutil/               → Test helpers, fixtures, shared test infrastructure
pkg/
  sdk/                    → Public SDK interfaces (abstract, pluggable for web/mobile)
configs/                  → Configuration files (JSON, YAML)
deployments/              → Docker, Kubernetes manifests, Helm charts
scripts/                  → Cert generation, deployment helpers
tests/
  unit/                   → Unit tests (mirroring internal/ structure)
  integration/            → Integration tests (real DB, real WebSocket)
  load/                   → Load/stress tests
docs/                     → Architecture, data flow, deployment guides
```

### The Dependency Rule

Dependencies flow inward. Outer layers depend on inner layers, never the reverse.

```
cmd/ → internal/* → pkg/sdk (interfaces only)
```

- `internal/telemetry/` receives raw Tesla data, emits domain events
- `internal/events/` defines the event bus — all modules publish/subscribe through it
- `internal/drives/` subscribes to telemetry events, manages drive state machine
- `internal/ws/` subscribes to telemetry events, pushes to browser clients
- `internal/store/` subscribes to events, persists to PostgreSQL
- `internal/auth/` is a dependency of `internal/ws/`, never the reverse
- `pkg/sdk/` contains ONLY interfaces and types — no implementation

### Go Conventions

- **Go 1.23+** — use standard library `log/slog` for structured logging
- **No frameworks.** Standard library `net/http`, `nhooyr.io/websocket`, `pgx` for Postgres
- **Interfaces at consumer site.** Define interfaces where they're used, not where they're implemented
- **Accept interfaces, return structs.** Functions accept interface parameters, return concrete types
- **Error wrapping.** Use `fmt.Errorf("context: %w", err)` — never swallow errors
- **Context propagation.** Every function that does I/O takes `context.Context` as first parameter
- **Table-driven tests.** All test files use table-driven pattern with `t.Run` subtests
- **No globals.** All dependencies injected via constructor. No `init()` functions except for flag parsing in `main`
- **Struct embedding for composition, not inheritance**
- **Channel-based concurrency.** Prefer channels over mutexes where possible. Use `errgroup` for managed goroutines

### File Rules

- **Max 300 lines per file** (excluding tests). Decompose if exceeded
- **One type per file** for major domain types
- **Test files adjacent** to source: `foo.go` → `foo_test.go`
- **No `_test` package suffix** for unit tests (test internal behavior). Use `_test` suffix only for integration tests that test the public API
- **Named returns only when they improve readability** (e.g., multiple return values of same type)

### Error Handling

- Define domain error types in each package: `var ErrVehicleNotFound = errors.New("vehicle not found")`
- Wrap with context at every level: `fmt.Errorf("store.GetVehicle(%s): %w", id, err)`
- Never log AND return an error — do one or the other
- Use `errors.Is()` and `errors.As()` for error checking, never string comparison

### Configuration

- Use a single `Config` struct loaded from environment variables + optional JSON file
- Environment variables for secrets (TLS certs, DB password, auth keys)
- JSON config file for operational settings (intervals, thresholds, field mappings)
- Validate all config at startup — fail fast on invalid config

### Observability

- **Structured logging:** `log/slog` with JSON handler in production, text handler in development
- **Metrics:** Prometheus via `prometheus/client_golang` — every major operation has a counter/histogram
- **Health checks:** `/healthz` (liveness) and `/readyz` (readiness, checks DB + telemetry connection)
- **Tracing:** OpenTelemetry spans on every inbound/outbound request (defer to Phase 2 if needed)

### Security (Non-Negotiable)

- **mTLS termination** at the service level for Tesla vehicle connections
- **All client WebSocket connections authenticated** via JWT or session token
- **No Tesla credentials in logs** — redact VINs in production logs (show last 4 only)
- **Input validation on all protobuf fields** — malformed data must not crash the server
- **Rate limiting** on client WebSocket connections
- **TLS for all external connections** (DB, client WS)
- **Secrets via environment variables only** — never in config files or code

### Database

- **Same Supabase PostgreSQL** as MyRoboTaxi Next.js app
- **pgx driver** (not database/sql) for connection pooling and PostgreSQL-specific features
- **Repository pattern:** one repository struct per domain aggregate (VehicleRepo, DriveRepo)
- **Migrations:** `golang-migrate/migrate` — numbered SQL files in `internal/store/migrations/`
- **Never modify tables owned by the Next.js app's Prisma schema.** Only read from them or add new tables

### Testing

- **Unit tests:** Every package, table-driven, mock external dependencies via interfaces
- **Integration tests:** Real PostgreSQL (testcontainers-go), real WebSocket connections
- **Load tests:** k6 or custom Go benchmarks for WebSocket throughput
- **Test coverage target:** 80%+ on `internal/` packages
- **No test pollution:** Each test creates its own data, cleans up after itself

## Contracts

The SDK v1 contract surface lives in [`docs/contracts/`](docs/contracts/README.md). It is the authoritative source for every WebSocket message, REST endpoint, persisted field, atomic group, classification label, and state transition exposed by the SDK.

- Read [`docs/contracts/README.md`](docs/contracts/README.md) before touching WebSocket messages, DB schema, or public SDK API surface.
- Every contract change follows the PR workflow: `sdk-architect` review + `contract-guard` gate, per the Merge Policy above.
- Contract drift (schema change without doc update, missing P0/P1/P2 label, broken atomic group) blocks merge unconditionally.

## Linear Issues and Agent Routing

**Linear is the source of truth for issues and roadmap.** Team key: `MYR` (e.g., `MYR-42`). GitHub is connected to Linear for automatic status sync. Every Linear issue is labeled with the **agent** that should implement it. When picking up an issue, Claude MUST use the specified agent(s) for implementation.

### Agent Labels

Issues carry one or more `Agent/<name>` labels in Linear that map directly to `.claude/agents/<name>.md`:

| Linear Label | Agent File | When to Use |
|-------|-----------|-------------|
| `Agent/sdk-architect` | `sdk-architect.md` | **Supervisor** — owns requirements + contract docs, reviews every PR for contract adherence, coordinates SDK work. Auto-invoked on contract-relevant paths. |
| `Agent/sdk-typescript` | `sdk-typescript.md` | TypeScript SDK implementation (core, React, Node — web/Next.js consumers only; **no React Native**) |
| `Agent/sdk-swift` | `sdk-swift.md` | Swift SDK implementation (iOS, iPadOS, macOS, watchOS, visionOS) |
| `Agent/contract-tester` | `contract-tester.md` | Contract conformance, FR/NFR, chaos test scenarios |
| `Agent/contract-guard` | `contract-guard.md` | Automated PR gate — blocks contract drift (session + CI) |
| `Agent/go-engineer` | `go-engineer.md` | Go implementation, constrained by SDK contract |
| `Agent/tesla-telemetry` | `tesla-telemetry.md` | Tesla protocol, protobuf, mTLS, cert management |
| `Agent/security` | `security.md` | Security, encryption, data classification (P0/P1/P2), RBAC |
| `Agent/testing` | `testing.md` | Unit tests (contract/FR/NFR/chaos owned by `contract-tester`) |
| `Agent/infra` | `infra.md` | CI/CD, observability stack, deployment, release pipelines |
| `Agent/ux-audit` | `ux-audit.md` | End-user experience quality audit |

### Picking up issues

- Trigger work from Linear via `W` then `O` (Open in Claude Code) — loads the issue into a new session with the configured prompt
- Or say "pick up MYR-42" in an active session — Claude fetches the issue via the Linear MCP
- Always fetch the latest issue body via the Linear MCP before implementing; comments may have newer context than the prompt snapshot

### Workflow (MUST FOLLOW)

When picking up an issue, execute these steps in order:

1. **Read the issue** — title, body, `Agent/*` labels, project, acceptance criteria, anchored FRs/NFRs
2. **Create a feature branch** from main using Linear's `{{issue.branchName}}` (see Branching Strategy below)
3. **Identify ALL `Agent/*` labels** on the issue — these are your implementation agents
4. **Spin up the tagged agents** using the Agent tool following the execution order below
5. **Commit in reasonable chunks** with the Linear identifier in every commit message (`MYR-<num> <verb> <what>`)
6. **Open a PR** when the issue is complete; Linear auto-links it and transitions the issue to "In Review"

### Agent Execution Order (ENFORCED)

**Phase 0 — Architect supervision (AUTO-INVOKED on contract-relevant paths):**
`sdk-architect` is automatically invoked when the session touches any of:
- `docs/architecture/` or `docs/contracts/`
- `internal/ws/`, `internal/store/`, or WebSocket/DB schema changes
- Public SDK API surface
- Any PR labeled `contract` or `sdk`

The architect reviews the plan, references FRs/NFRs in `docs/architecture/requirements.md`, and approves or redirects before implementation starts.

**Phase 1 — Design (if `Agent/sdk-architect` is explicitly tagged):**
For planning sessions, architectural decisions, or new contract docs. Wait for its output before implementation.

**Phase 2 — Implementation (spin up in parallel where possible):**
Launch independent agents in parallel via multiple Agent tool calls in one message:
- `Agent/go-engineer` — server-side Go implementation
- `Agent/sdk-typescript` — TypeScript SDK implementation
- `Agent/sdk-swift` — Swift SDK implementation
- `Agent/tesla-telemetry` — Tesla protocol work
- `Agent/infra` — CI/CD, observability, deployment

**Phase 3 — Contract enforcement (AUTO-INVOKED before PR):**
`contract-guard` runs session-time against the working diff to catch contract drift before the PR is opened. Runs again at CI-time as a required check.

**Phase 4 — Testing:**
- `Agent/testing` for unit tests
- `Agent/contract-tester` for contract conformance, FR/NFR, and chaos scenarios

**Phase 5 — Security review (if `Agent/security` is tagged):**
Reviews for data classification, encryption, RBAC enforcement, audit logging.

**Phase 6 — UX audit (ALWAYS runs):**
`Agent/ux-audit` reviews end-user experience impact.

**Phase 7 — Architect PR review (AUTO-INVOKED on contract-touching PRs):**
`sdk-architect` performs the final contract-adherence review before merge.

### Examples

**Issue with labels `Agent/sdk-architect`, `Agent/go-engineer`, `Agent/testing`:**
1. `sdk-architect` → defines contract + scopes work (WAIT for output)
2. `go-engineer` → implements against contract
3. `contract-guard` → checks diff before PR
4. `testing` → unit test coverage
5. `contract-tester` → contract/FR conformance tests
6. `ux-audit` → UX impact
7. `sdk-architect` → final PR review

**Issue with labels `Agent/sdk-typescript`, `Agent/contract-tester`:**
1. `sdk-architect` (auto) → approves the task scope
2. `sdk-typescript` → implements TS SDK feature
3. `contract-guard` → checks diff
4. `contract-tester` → writes FR/NFR tests
5. `ux-audit` → UX impact
6. `sdk-architect` → final review

**Issue with labels `Agent/tesla-telemetry`, `Agent/go-engineer`, `Agent/security`:**
1. `sdk-architect` (auto) → approves scope
2. `tesla-telemetry` + `go-engineer` in parallel → implement
3. `contract-guard` → checks diff
4. `security` → data classification + encryption review
5. `ux-audit` → UX impact
6. `sdk-architect` → final review

### Projects (SDK v1 roadmap)

Work is organized into Linear Projects anchored to `docs/architecture/requirements.md` FRs/NFRs:

| Project | Focus |
|---------|-------|
| P1 — Contract foundation | AsyncAPI/OpenAPI specs, JSON Schema, fixtures, data classification |
| P2 — Backend SDK v1 | Go server: atomicity, 1s nav intervals, AES-256-GCM encryption, RBAC, audit |
| P3 — TypeScript SDK v1 | Web/Next.js SDK: core (browser + Node) + React, <75KB bundle. **No React Native** — Apple platforms covered by P4. |
| P4 — Swift SDK v1 | iOS 26+ / iPadOS 26+ / macOS 26+ / watchOS 26+ / visionOS 26+. iOS clients consume the Swift SDK directly, not via React Native bridging. |
| P5 — Observability & Scale | OTel, Prometheus/Grafana, 5K-client load tests, SLOs |
| P6 — Web test bench | Contract/FR/NFR/chaos validation UI |
| P7 — Frontend integration | Next.js consumes TS SDK (depends on P3) |
| P8 — Autonomous agent | Linear Agent bot for hands-off issue delegation (post-v1) |

## Dev Tools (cmd/testbench, cmd/simulator)

### Test Bench (`cmd/testbench/`)

A terminal UI (TUI) app built with `charmbracelet/bubbletea` for inspecting live telemetry from your real Tesla during development.

**Features:**
- Connect to the telemetry server as a WebSocket client
- Display real-time vehicle data: speed, location, charge, heading, gear
- Show event bus activity: telemetry events, drive events, connectivity
- Drive detection status: idle/driving, current drive stats, route points
- Raw telemetry inspector: see every protobuf field as it arrives
- Connection health: latency, message rate, drops

**Usage:**
```bash
go run ./cmd/testbench --server ws://localhost:8080 --token <auth-token>
```

### Simulator (`cmd/simulator/`)

A mock Tesla vehicle that sends fake protobuf telemetry to the server. For testing the full pipeline without a real car.

**Features:**
- Simulate one or many vehicles with configurable VINs
- Replay recorded drive routes (from JSON files)
- Generate random drives with realistic speed/location patterns
- Simulate edge cases: sleep/wake, connectivity drops, rapid gear changes
- Configurable telemetry interval and field selection

**Usage:**
```bash
go run ./cmd/simulator --server wss://localhost:443 --vehicles 5 --scenario highway-drive
```

## Branching Strategy (Enforced)

When picking up a Linear issue, ALWAYS create a feature branch from the latest `main` using **Linear's auto-generated branch name**:

```bash
git checkout main && git pull origin main && git checkout -b <linear-branch-name>
```

Branch name format: use the value Linear provides via `{{issue.branchName}}` in the Claude Code prompt (or copy from the Linear issue via `⌘⇧.`). This ensures GitHub ↔ Linear auto-sync (status transitions, PR linking) works correctly.

Examples:
- MYR-42 "Implement in-process event bus" → `thomas/myr-42-implement-in-process-event-bus`
- MYR-9 "Telemetry receiver: mTLS WebSocket server" → `thomas/myr-9-telemetry-receiver-mtls-websocket-server`

Do NOT hand-craft branch names — Linear's format is what the GitHub integration matches against.

## Commit Strategy

### Commit Message Format

Every commit message MUST include the Linear issue identifier and use imperative mood:

```
MYR-<num> <Imperative verb> <what changed>
```

Examples:
- `MYR-42 Add Bus interface and channel-based implementation`
- `MYR-42 Add backpressure handling with drop-oldest policy`
- `MYR-42 Add comprehensive tests for concurrent pub/sub`
- `MYR-9 Implement mTLS WebSocket server for Tesla vehicles`
- `MYR-9 Add protobuf decoding with field validation`

Linear auto-links commits containing `MYR-<num>` to the referenced issue.

### Commit Cadence

Commit in reasonable chunks throughout development — NOT one giant commit at the end. A good commit represents one logical unit:

1. **Interface/type definitions** — commit after defining the types for a module
2. **Core implementation** — commit the main logic (one commit per major function/method is fine)
3. **Tests** — commit tests alongside or immediately after the code they test
4. **Wiring/integration** — commit when connecting a module to the event bus or other components
5. **Configuration** — commit config changes separately from logic changes

A typical issue should have **3-8 commits**, not 1 and not 20.

### Pre-Commit Checks

Run before every commit:
1. `go vet ./...`
2. `golangci-lint run`
3. `go test ./...`
4. `go build ./cmd/...`

### Pre-PR Lint Gate (ENFORCED)

Every agent MUST run `golangci-lint run ./...` and fix all warnings before opening a PR. CI will reject PRs that fail lint. This applies to ALL agents — implementation, testing, infra, etc. No exceptions. If a lint rule seems wrong, suppress it with a targeted `//nolint:rulename // reason` comment, never globally disable the rule.

### Merge Policy (NON-NEGOTIABLE)

A PR MUST NOT be merged until ALL of the following are true:

1. **All CI checks pass** — lint, test, build, security, gosec, contract-guard. No exceptions.
2. **All review comments are addressed** — every "changes requested" review must be resolved and re-approved before merge. Do not dismiss or skip reviews.
3. **No pending change requests** — if a reviewer (human or bot) requested changes, fix them and get the review approved. Never use `--admin` or `--force` to bypass.
4. **All critical and warning-level Claude Review comments are resolved** — Claude Review comments marked as critical, high, or medium severity must be fixed before merge. Low-severity suggestions are optional but encouraged.
5. **`sdk-architect` review required** for any PR touching `docs/architecture/`, `docs/contracts/`, WebSocket messages, DB schema, public SDK API, or any path under `internal/ws/` or `internal/store/`. The architect must approve the PR with a Contract Adherence checklist before merge.
6. **`contract-guard` must pass** for all PRs. Contract drift (missing contract doc updates, missing data classification labels, missing atomic group membership) blocks merge unconditionally.

**NEVER use `gh pr merge --admin`** to bypass branch protection. If merge is blocked, fix the root cause — don't circumvent the safeguard.

### Claude Review Comment Severity (ENFORCED)

When Claude Review (tnando-gh-bot) leaves review comments, they carry severity levels. The following MUST be addressed before merge:

| Severity | Action Required |
|----------|----------------|
| **Critical** | MUST fix — blocks merge |
| **High** | MUST fix — blocks merge |
| **Medium / Warning** | MUST fix — blocks merge |
| **Low / Suggestion** | Optional — fix if reasonable |

If you disagree with a review comment, respond with a justification on the PR — do not silently ignore it.

## What NOT to Do

- Do NOT import from `my-robo-taxi` — communicate only via shared database and documented contracts
- Do NOT use gorilla/websocket (unmaintained) — use `nhooyr.io/websocket`
- Do NOT use ORMs — use raw SQL with pgx
- Do NOT store telemetry credentials in config files
- Do NOT log full VINs in production
- Do NOT use `panic()` except in truly unrecoverable situations (main initialization)
- Do NOT use `any` / `interface{}` when a concrete type or narrower interface works
- Do NOT create God structs — if a struct has more than 7 fields, consider decomposition
