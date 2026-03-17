# CI/CD Architecture

## Overview

The pipeline has two phases: **CI** (quality gates that must all pass) and **CD** (deployment triggered on merge to main or tag push). An AI-powered code review runs as a separate workflow after CI completes.

```
PR Opened/Updated
  │
  ├──► CI Pipeline (parallel jobs)
  │    ├── Lint (golangci-lint)
  │    ├── Test (unit + integration, race detector, coverage)
  │    ├── Security (govulncheck + gosec)
  │    └── Build (go build + Docker image)
  │
  ├──► Claude Code Review (parallel with CI)
  │    ├── Read PR labels → determine domain agents
  │    ├── Domain-specific review (security, telemetry, concurrency, etc.)
  │    ├── CLAUDE.md rule enforcement
  │    └── Approve / Request Changes
  │
  └──► Merge Gate
       ├── All CI jobs green ✓
       ├── Claude review approved ✓ (or no changes requested)
       └── Merge allowed

Merge to main
  └──► CD: Deploy to Railway (wait for CI green)

Tag push (v*)
  └──► Release: GoReleaser → binaries + Docker image → GitHub Release
```

## CI Jobs

### 1. Lint (`golangci-lint`)

Runs the full linter suite defined in `.golangci.yml`. Catches:
- Bug patterns (bodyclose, nilerr, sqlclosecheck, rowserrcheck)
- Error handling (errorlint, wrapcheck)
- Security (gosec rules via golangci-lint)
- Complexity (cyclop, funlen, nestif, gocognit)
- Style (revive, gocritic, misspell, errname)
- Performance (prealloc, perfsprint)

### 2. Test (unit + integration)

- **Unit tests:** `go test -race -coverprofile=coverage.out -count=1 ./...`
- **Integration tests:** Same but with `DATABASE_URL` pointing to PostgreSQL service container
- **Coverage threshold:** 80% minimum — job fails if below
- **Race detector:** Always on — catches data races at CI time

### 3. Security

Three complementary scanners:
- **govulncheck:** Official Go vulnerability scanner. Call-graph analysis means near-zero false positives. Only reports vulnerabilities in functions your code actually calls.
- **gosec:** AST-based security checker for code patterns (injection, hardcoded creds, weak crypto, etc.)
- **Trivy:** Container image scanner (runs after Docker build). Catches OS-level and dependency vulnerabilities.

### 4. Build

- `go build ./cmd/...` — confirms compilation
- Docker image build (no push on PRs)
- Image size validation (should be < 30MB with distroless base)

## Claude Code Review

### How Domain Routing Works

Every PR inherits `agent:*` labels from the issue it closes. The review workflow reads these labels and constructs a domain-aware prompt:

```
PR labels: [agent:tesla-telemetry, agent:security]
                    │
                    ▼
Claude reads CLAUDE.md (always) + constructs domain-specific review focus:
  - Tesla protocol correctness (protobuf, mTLS, field formats)
  - Security review (cert handling, input validation, VIN redaction)
```

The `.claude/agents/pr-reviewer.md` agent has a mapping of every `agent:*` label to its review focus area. Claude checks ALL of these that apply:

| Label | Review Focus |
|-------|-------------|
| `agent:architect` | Interface design, dependency direction, module boundaries |
| `agent:go-engineer` | Idiomatic Go, error handling, naming, file size limits |
| `agent:tesla-telemetry` | Tesla protocol correctness, protobuf decoding, mTLS config |
| `agent:event-system` | Concurrency safety, channel usage, backpressure, goroutine leaks |
| `agent:websocket-sdk` | Protocol compatibility with frontend, message format, connection handling |
| `agent:security` | OWASP Top 10, secrets exposure, VIN redaction, input validation |
| `agent:testing` | Test quality, coverage, race conditions, test isolation |
| `agent:infra` | Docker best practices, CI config correctness, deployment safety |
| `agent:frontend-integration` | Field name mapping, message format compatibility |

### Review Outcomes

Claude posts a review with one of three verdicts:
- **APPROVE** — Code is clean, all domain checks pass
- **REQUEST_CHANGES** — Issues found that must be fixed before merge
- **COMMENT** — Suggestions and observations, but not blocking

### Interactive Mode

Developers can `@claude` in PR comments for follow-up questions, explanations, or asking Claude to fix issues.

## CD Pipeline

### Railway Deployment (merge to main)

1. CI must pass (Railway "Wait for CI" feature)
2. Railway auto-deploys from main branch
3. Health checks confirm deployment (`/healthz`, `/readyz`)

### Release (tag push v*)

1. GoReleaser builds binaries (linux/amd64, linux/arm64)
2. Builds and pushes Docker image to GHCR
3. Creates GitHub Release with changelog
4. Semantic versioning from git tags

## Known Gotchas (Read Before Modifying Workflows)

1. **`go test -count=1`** is critical — without it, Go caches test results and can mask flaky tests
2. **`-race` makes tests 2-10x slower** — account for this in timeout settings
3. **PostgreSQL health checks are required** — without them, tests start before DB is ready
4. **golangci-lint cache conflicts with setup-go cache** — we use `skip-save-cache` on non-main branches
5. **Forked PRs get read-only GITHUB_TOKEN** — security scan SARIF uploads may fail on forks (handled gracefully)
6. **`only-new-issues` on golangci-lint fails on push to main** — we only use it on PR events
7. **Quote Go versions in YAML** — `1.20` without quotes becomes float `1.2`
8. **`fetch-depth: 0`** is required for GoReleaser changelog generation
9. **`actions/checkout@v4`** is used (not v6) for maximum runner compatibility
