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

## GitHub Issues and Agent Routing

Every GitHub issue is labeled with the **agent** that should implement it. When picking up an issue, Claude MUST use the specified agent(s) for implementation.

### Agent Labels

Issues carry one or more `agent:<name>` labels that map directly to `.claude/agents/<name>.md`:

| Label | Agent File | When to Use |
|-------|-----------|-------------|
| `agent:architect` | `architect.md` | System design, interface definitions, module boundaries |
| `agent:go-engineer` | `go-engineer.md` | Go implementation, code writing, refactoring |
| `agent:tesla-telemetry` | `tesla-telemetry.md` | Tesla protocol, protobuf, mTLS, cert management |
| `agent:event-system` | `event-system.md` | Event bus, pub/sub, domain events, concurrency |
| `agent:websocket-sdk` | `websocket-sdk.md` | Client WebSocket server, SDK interfaces |
| `agent:security` | `security.md` | Security review, threat modeling, auth, validation |
| `agent:testing` | `testing.md` | Test writing, test infrastructure, coverage |
| `agent:infra` | `infra.md` | Docker, CI/CD, deployment, monitoring |
| `agent:frontend-integration` | `frontend-integration.md` | MyRoboTaxi frontend compatibility |
| `agent:ux-audit` | `ux-audit.md` | Cross-project UX audit — end-user experience quality |

### Workflow (MUST FOLLOW)

When picking up an issue, execute these steps in order:

1. **Read the issue** — title, body, labels, milestone, and acceptance criteria
2. **Create a feature branch** from main (see Branching Strategy below)
3. **Identify ALL `agent:*` labels** on the issue — these are your implementation agents
4. **Spin up the tagged agents** using the Agent tool following the execution order below
5. **Commit in reasonable chunks** with the issue number in every commit message
6. **Open a PR** when the issue is complete, carrying over the issue's labels

### Agent Execution Order (ENFORCED)

When multiple agents are tagged on an issue, you MUST spin them up in this order:

**Phase 1 — Design (if `agent:architect` is tagged):**
Spin up the `architect` agent FIRST to define interfaces, module boundaries, and design decisions before any code is written. Wait for its output before proceeding.

**Phase 2 — Implementation (spin up in parallel where possible):**
Spin up ALL of these tagged agents to do the work. Launch independent agents in parallel using multiple Agent tool calls in a single message:
- `agent:go-engineer` — writes the implementation code
- `agent:tesla-telemetry` — handles Tesla-specific protocol work
- `agent:event-system` — handles event bus and concurrency work
- `agent:websocket-sdk` — handles WebSocket server and SDK work
- `agent:infra` — handles Docker, CI, deployment work

**Phase 3 — Testing (if `agent:testing` is tagged):**
Spin up the `testing` agent AFTER implementation is complete to write/verify tests.

**Phase 4 — Security review (if `agent:security` is tagged):**
Spin up the `security` agent LAST to review the completed code for vulnerabilities.

**Phase 5 — Frontend compatibility (if `agent:frontend-integration` is tagged):**
Spin up the `frontend-integration` agent to verify protocol compatibility with the MyRoboTaxi Next.js app.

**Phase 6 — UX audit (ALWAYS runs, no label required):**
Spin up the `ux-audit` agent as the final step on every issue, regardless of labels. This agent audits the completed changes across both the backend and frontend to catch UX regressions — wrong data on screen, missing loading states, broken real-time flows, or contract mismatches that would degrade the end user's experience.

### Examples

**Issue with labels `agent:go-engineer, agent:testing`:**
1. Spin up `go-engineer` → writes implementation + inline tests
2. Spin up `testing` → reviews test coverage, adds missing tests
3. Spin up `ux-audit` → audit end-user experience impact

**Issue with labels `agent:architect, agent:event-system, agent:go-engineer, agent:testing`:**
1. Spin up `architect` → defines interfaces and design (WAIT for output)
2. Spin up `event-system` + `go-engineer` in parallel → implement
3. Spin up `testing` → verify tests and coverage
4. Spin up `ux-audit` → audit end-user experience impact

**Issue with labels `agent:tesla-telemetry, agent:go-engineer, agent:security`:**
1. Spin up `tesla-telemetry` + `go-engineer` in parallel → implement
2. Spin up `security` → audit the completed code
3. Spin up `ux-audit` → audit end-user experience impact

### Milestones

| Milestone | Phase | Focus |
|-----------|-------|-------|
| `Phase 1: Foundation` | Weeks 1-2 | Project scaffolding, event bus, config, DB layer, CI/CD |
| `Phase 2: Tesla Integration` | Weeks 2-3 | mTLS, protobuf, telemetry receiver, Fleet API |
| `Phase 3: Real-Time Processing` | Weeks 3-4 | Drive detection, geocoding, persistence, batch writes |
| `Phase 4: Client WebSocket` | Weeks 4-5 | Auth, broadcast, SDK interfaces, frontend integration |
| `Phase 5: Hardening` | Weeks 5-6 | Load tests, security audit, monitoring, deployment |
| `Phase 6: Test Bench` | Ongoing | TUI dashboard, simulator, developer tooling |

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

When picking up a GitHub issue, ALWAYS create a feature branch from the latest `main`:

```bash
git checkout main && git pull origin main && git checkout -b <issue-number>-<short-kebab-description>
```

Branch name format: `<issue-number>-<short-kebab-description>` derived from the issue title.

Examples:
- Issue #2 "Implement in-process event bus" → `2-event-bus`
- Issue #9 "Telemetry receiver: mTLS WebSocket server" → `9-telemetry-receiver`
- Issue #17 "Security audit" → `17-security-audit`

## Commit Strategy

### Commit Message Format

Every commit message MUST include the issue number and use imperative mood:

```
#<issue> <Imperative verb> <what changed>
```

Examples:
- `#2 Add Bus interface and channel-based implementation`
- `#2 Add backpressure handling with drop-oldest policy`
- `#2 Add comprehensive tests for concurrent pub/sub`
- `#9 Implement mTLS WebSocket server for Tesla vehicles`
- `#9 Add protobuf decoding with field validation`

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

1. **All CI checks pass** — lint, test, build, security, gosec. No exceptions.
2. **All review comments are addressed** — every "changes requested" review must be resolved and re-approved before merge. Do not dismiss or skip reviews.
3. **No pending change requests** — if a reviewer (human or bot) requested changes, fix them and get the review approved. Never use `--admin` or `--force` to bypass.
<<<<<<< HEAD
4. **All critical and warning-level Claude Review comments are resolved** — Claude Review comments marked as critical, high, or medium severity must be fixed before merge. Low-severity suggestions are optional but encouraged.

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

=======

**NEVER use `gh pr merge --admin`** to bypass branch protection. If merge is blocked, fix the root cause — don't circumvent the safeguard.

>>>>>>> 5f3e323 (Add merge policy: never bypass reviews, failing CI, or change requests)
## What NOT to Do

- Do NOT import from `my-robo-taxi` — communicate only via shared database and documented contracts
- Do NOT use gorilla/websocket (unmaintained) — use `nhooyr.io/websocket`
- Do NOT use ORMs — use raw SQL with pgx
- Do NOT store telemetry credentials in config files
- Do NOT log full VINs in production
- Do NOT use `panic()` except in truly unrecoverable situations (main initialization)
- Do NOT use `any` / `interface{}` when a concrete type or narrower interface works
- Do NOT create God structs — if a struct has more than 7 fields, consider decomposition
