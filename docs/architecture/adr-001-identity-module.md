# ADR-001: Identity as a bounded module in the Go telemetry server

- Status: Accepted
- Date: 2026-07-10
- Deciders: Client (architecture review 2026-07-10), go-engineer, security
- Linear: MYR-193
- Supersedes: the `react-frontend` NextAuth-only login path (now DEPRECATED)

## Context

`react-frontend` (the original Next.js web app) is deprecated. The Go
telemetry server is the production backend for MyRoboTaxi and is the only
service that will be actively developed. The iOS app (native SwiftUI,
MyRoboTaxi) needs a first-class authentication path that does not depend on
a browser OAuth round-trip: native **Sign in with Apple**.

Until now the server only *validated* tokens — it never *minted* them. The
tokens were HS256 JWTs signed with the shared `AUTH_SECRET`, minted by the
Next.js app (and, for the dev bench, by `sdk-testbench`'s `/api/backend-token`
route). The shared-secret model is acceptable for a single trusted minter,
but it does not scale to the server itself becoming an issuer: a symmetric
secret held by both the verifier and every minter has no key-isolation story,
and shipping the same secret to a public JWKS is impossible.

We also must not break the live app: every existing browser client and the
testbench continue to present HS256 tokens signed with `AUTH_SECRET`. Those
MUST keep working unchanged during and after this change.

The existing user records are keyed by Prisma CUIDs in the **shared Supabase
Postgres** database (the same DB the Next.js app's Prisma schema owns). The
client's own user CUID `cmmgr4b1p0005l104ifpctlg8` is referenced by his
vehicles and rides; his Apple sign-in MUST resolve to that exact CUID.

## Decision

### 1. Identity is a strictly-bounded module, not a new service

Identity lives in `internal/identity` as a **module inside the
modular-monolith**, owning auth end-to-end:

- its own tables under the CG-DL-9 `go_` namespace: `go_identity_apple`,
  `go_refresh_tokens`, `go_users`;
- its own signing-key secret class (the ES256 private key — see below),
  never shared with any other module;
- its own endpoints under `/api/auth/*`;
- the rest of the app consumes only the **validated user identity** (a user
  CUID string), exactly as it does today through
  `auth.JWTAuthenticator.ValidateToken`.

We deliberately did **not** split identity into a separate process. At v1
scale (NFR-3.6: ~1,000 users) a separate IAM service would add a network
hop, a second deploy, and a second Postgres connection pool for no benefit.
The module boundary (package + table namespace + secret class) gives us the
isolation that matters (blast radius, ownership, an extraction seam) without
the operational cost. This mirrors the MYR-180 decision to reuse the
existing `tesla-http-proxy` sidecar rather than embed the vehicle-command
library: **isolate the secret, not necessarily the process.**

**Split triggers** (when to extract `internal/identity` into its own
service): (a) a second consumer service needs to validate tokens
independently of this process; (b) auth traffic or blast-radius requires an
independent deploy/scale cadence; (c) a compliance boundary (SOC2/PCI) forces
process isolation of the credential store. None hold at v1.

**First candidate for further isolation is NOT IAM.** The Tesla command
signer — the P-256 key that signs vehicle commands — is already isolated in
the `tesla-http-proxy` sidecar (MYR-180); it is the most security-sensitive
key in the system and the natural first target for hardening (e.g. an HSM /
KMS-backed signer). IAM's ES256 key is lower-blast-radius (a leaked signing
key lets an attacker mint access tokens, but the refresh-token family
reuse-detection and 1h access TTL bound the damage, and rotation via JWKS
`kid` is cheap).

### 2. Asymmetric access tokens (ES256) with a JWKS publication path

New access tokens minted by this server are **ES256** (ECDSA P-256), with the
signing `kid` in the JWT header. The public key(s) are published at
`GET /api/auth/.well-known/jwks.json`. The identity module alone holds the
private key.

Rationale for ES256 over RS256: smaller keys and signatures (P-256 sig ≈ 64
bytes vs RSA-2048 ≈ 256 bytes) — meaningful on watchOS/cellular (NFR-3.36) —
and the same curve family Apple itself uses for its identity tokens, so we
already carry ECDSA verification code.

**Key provenance.** The private key is a config secret
(`AUTH_ES256_PRIVATE_KEY`, PEM PKCS#8, base64-optional via
`AUTH_ES256_PRIVATE_KEY_B64` for Fly). There is **no** generate-if-absent in
production — a missing key is a fail-fast startup error. A DEBUG/dev
fallback (`--dev`) generates an **ephemeral** P-256 key with a loud
`WARN` log; tokens signed with it do not survive a restart, which is the
intended dev behaviour. The `kid` is the RFC 7638 JWK SHA-256 thumbprint of
the public key, so it is stable and derivable from the key alone.

Generation recipe (runbook, also in the PR body and `.env.example`):

```
# P-256 private key, PKCS#8 PEM:
openssl ecparam -genkey -name prime256v1 -noout -out ec-private.pem
openssl pkcs8 -topk8 -nocrypt -in ec-private.pem -out ec-private-pkcs8.pem
# For Fly / container env (single line, no newlines):
base64 -i ec-private-pkcs8.pem | tr -d '\n'   # -> AUTH_ES256_PRIVATE_KEY_B64
```

### 3. One validator, dual-algorithm, algorithm pinned per token

The server's existing JWT **validation** accepts BOTH:

- **legacy HS256** — current shared-secret tokens (unchanged; the whole live
  app depends on them); and
- **new ES256** — verified against the locally-held keypair's public keys,
  resolved by `kid`.

There is exactly one validator (`auth.JWTAuthenticator.ValidateToken`), the
single `Authenticator` used by both the WebSocket handler and every REST
handler. The algorithm is **pinned per token by the header `alg` + `kid`**
with strict allowlists, so the classic alg-confusion attacks are impossible:

- valid methods are allowlisted to exactly `{HS256, ES256}` (`none` and every
  other alg are rejected before the key callback runs);
- the key callback is **type-driven**: an HMAC-typed token gets the
  `[]byte` shared secret and *only* that; an ECDSA-typed token gets an
  `*ecdsa.PublicKey` resolved by `kid` and *only* that. An attacker who takes
  the published ES256 public key and signs an HS256 token with it as an HMAC
  key fails, because the HS256 branch returns the real `AUTH_SECRET`, never
  the public key; an attacker who presents an ES256 token cannot have it
  verified with the HMAC secret, because the ECDSA branch requires an EC
  public key;
- `iss`/`aud`/`exp` are checked identically for both algs. Both the Next.js
  minter and this server mint with `iss=myrobotaxi`, `aud=telemetry` (the
  existing `AuthConfig` values), so the validator's issuer/audience checks are
  uniform across algs.

The ES256 public-key resolver is provided by the identity module's keystore
and injected into the `JWTAuthenticator` at construction. When no ES256
keystore is configured (e.g. legacy deploys), the validator silently remains
HS256-only — ES256 tokens are simply rejected as "unknown kid".

### 4. Linkage: same-DB email-match (topology finding)

**Topology: the Go server and the Next.js app share ONE Supabase Postgres
database.** Evidence: `CLAUDE.md` ("Same Supabase PostgreSQL as MyRoboTaxi
Next.js app"); `internal/auth/jwt_auth.go` reads the Prisma-owned `"User"`
and `"Vehicle"` tables directly; `sdk-testbench`'s `/api/backend-token`
route resolves identity by querying the shared `"Account"` table
(`(provider, providerAccountId) -> "User".id`) over the same `DATABASE_URL`;
the Prisma `User` model (`react-frontend/prisma/schema.prisma`) declares
`email String? @unique` and `emailVerified DateTime?`.

Therefore, on first Apple sign-in for an unknown `apple_sub`, the module
resolves the user in this precedence:

1. **Bootstrap override** (`AUTH_APPLE_BOOTSTRAP` — a
   `email=cuid[,email=cuid...]` map). A defensive, config-seeded safety net
   so the client's first sign-in binds to `cmmgr4b1p0005l104ifpctlg8` even if
   his Apple Relay email differs from his DB email. Documented, no secret.
2. **Verified-email match** against the Prisma `"User"` table
   (read-only `SELECT "id" FROM "User" WHERE "email" = $1`, and only when
   Apple asserts `email_verified`). This is how a returning web user's Apple
   sign-in binds to their existing CUID.
3. **Fresh mint** into the Go-owned `go_users` table (a cuid-shaped id) for a
   genuinely new user with no legacy row. `go_users` never writes to any
   Prisma table (CG-DL-9); the user-existence check used by the validator is
   widened to accept a CUID present in **either** `"User"` or `go_users`.

The chosen `apple_sub -> user_id` binding is persisted once in
`go_identity_apple` and reused on every subsequent sign-in — email is never
re-matched after the first bind, so a later email change cannot silently
re-point an Apple account. **We never write to Prisma-owned tables**
(CG-DL-9); the legacy `"User"` table is read-only from Go.

### 5. Refresh tokens: single-use rotation with family reuse-detection

Refresh tokens are opaque 256-bit random strings. Only their SHA-256 hashes
are stored (`go_refresh_tokens.token_hash` PK). Each token belongs to a
**family** (`family_id`); a refresh is **single-use** and **rotates** to a new
token in the same family. Presenting a token that has already been rotated (or
one that is revoked) is treated as **theft**: the entire family is revoked and
the caller gets `401`. The family has a **sliding 90-day** life — each
rotation issues a token expiring `now + 90d`. `/api/auth/revoke` revokes the
whole family.

### 6. Audit without touching the Prisma AuditLog enum

Auth events (sign-in, refresh, reuse-detection, revoke) are audit-logged via a
dedicated structured `slog` audit logger inside the module — user id + event
only, **no PII** (no email, name, token, or token hash). We deliberately do
**not** write these to the Prisma-owned `"AuditLog"` table: its `action` enum
is contract-owned (data-lifecycle.md §4.2) and restricted to the three
system actions the Go server may insert (`drives_pruned`, `mask_applied`,
`tokens_refreshed`); adding auth actions would be a cross-repo Prisma change
(CG-DL-8) out of scope here, and would couple the identity module to a table
it does not own. The in-module slog audit stream keeps identity's audit trail
inside identity's boundary.

## Consequences

- The iOS app gets a native, browser-free sign-in and a long-lived refresh
  session, consuming only `{accessToken, refreshToken, user}`.
- The server becomes a token **issuer** with a clean key-isolation and
  rotation story (JWKS `kid`), without a second process.
- Legacy HS256 keeps working verbatim; there is no flag day.
- New auth surface = new attack surface: mitigated by per-IP rate limiting on
  the auth endpoints, hash-only refresh storage, family reuse-detection, a 1h
  access TTL, and strict per-token algorithm pinning.
- A future extraction of `internal/identity` into a standalone IAM service is
  a package-move + a shared JWKS URL, not a redesign — the module boundary was
  chosen with that seam in mind.
