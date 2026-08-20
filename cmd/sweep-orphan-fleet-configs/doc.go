// Binary sweep-orphan-fleet-configs is MYR-593's one-off operator sweep for
// Tesla fleet-telemetry configs whose local "Vehicle" row no longer exists.
//
// Tesla bills fleet-telemetry PER VEHICLE and a config's `exp` is 350 days, so
// a config left behind by a car we deleted keeps costing money and streams to a
// receiver that rejects it. MYR-593 makes the teardown delete the Tesla-side
// config going forward; this clears the backlog that predates it.
//
// ── READ THIS BEFORE YOU BELIEVE THE OUTPUT ─────────────────────────────────
//
// THERE IS NO PARTNER TOKEN IN THIS CODEBASE. Every FleetAPIClient call
// authenticates with a per-call OAuth2 OWNER bearer token; there is no
// client_credentials grant and no partner_accounts call anywhere in the repo.
// The consequence is blunt:
//
//   - A config belonging to an account we have ALREADY DELETED is NOT reachable
//     by this tool. The tokens that could authenticate the DELETE were
//     destroyed with the account. Those VINs are reported `unreachable_no_token`
//     and they will keep streaming and billing until their 350-day exp lapses.
//     Nothing in this binary can change that. Fixing it would need a Tesla
//     partner token, which we do not have and do not obtain here.
//   - What this tool CAN reach: VINs whose owner still has a live Tesla link —
//     mid-fleet car removals where the Tesla-side delete failed or was skipped,
//     and cars provisioned then removed. Plus the DB-only
//     go_fleet_config_attempts garbage, which needs no token at all.
//
// So read `deleted` / `would_delete` alongside `unreachable_no_token`. The
// second number is the part of the leak that stays open.
//
// SINCE MYR-596 THAT SECOND NUMBER ALSO SHRINKS FOR THE WRONG REASON, and this
// is the paragraph to read before comparing two runs. Step 8e of the account
// deletion sequence now removes a person's go_removed_vehicles tombstones along
// with their account. Source A below is those tombstones. So for every account
// deleted from MYR-596 onward, its VINs never enter this report AT ALL — not as
// `unreachable_no_token`, not as anything. Nothing changed about the leak: a
// config whose owner is deleted was never reachable from here and still is not.
// What changed is that this tool stopped being able to NAME it. A falling
// `unreachable_no_token` is therefore no longer evidence of progress.
//
// Accounts deleted BEFORE MYR-596 shipped still have their tombstones, and that
// backlog is finite and closed — nothing adds to it. `-purge-orphan-tombstones`
// clears it (see the flag section below).
//
// ── WHAT IT LOOKS AT ────────────────────────────────────────────────────────
//
//	A. go_removed_vehicles tombstones (migration 0006) carrying a VIN that no
//	   "Vehicle" row claims. The tombstone's user_id is the only handle on a
//	   token — if that account is gone, so is the token. Since MYR-596 a
//	   DELETED account leaves no tombstone at all, so this source now sees only
//	   live owners' removals plus the pre-MYR-596 backlog.
//	B. For every user with a Tesla "Account" row: GET /api/1/vehicles, diffed
//	   against the VINs we hold for them. The genuinely reachable set.
//	C. go_fleet_config_attempts rows (migration 0031) whose vehicle_id matches
//	   no "Vehicle"."id". No VIN, so no Tesla call — pure DB garbage, deleted
//	   under -apply. This half runs FIRST and completes even if Tesla is
//	   unreachable.
//
// ── PER-VIN LABELS ──────────────────────────────────────────────────────────
//
//	would_delete          a config exists; -apply would remove it
//	deleted               the DELETE succeeded
//	no_config             Tesla reports nothing configured (or 404s)
//	unreachable_no_token  the owner has no Tesla "Account" row — see above
//	failed                Tesla or the token path errored; never aborts the run
//
// An EXPIRED-but-unrefreshable token is `failed`, not `unreachable_no_token`:
// a refresh failure can be transient or Tesla-side and is worth retrying,
// whereas a missing account row is permanent. Don't conflate the two.
//
// ── VIN REDACTION ───────────────────────────────────────────────────────────
//
// The slog lines on stderr carry redacted VINs (last 4, data-classification.md
// §2.1). The JSON report on stdout carries FULL VINs: this is an operator
// artifact run by hand against production, and a redacted tail cannot be acted
// on. Treat the report as P1 and don't paste it into a ticket.
//
// ── DRY RUN BY DEFAULT ──────────────────────────────────────────────────────
//
// The default run writes NOTHING and issues no Tesla DELETE — the reads are the
// point. -apply is the only thing that changes that, and if no config deleter
// could be built the run downgrades itself back to a dry run rather than
// reporting deletions that did not happen.
//
// ── -purge-orphan-tombstones: THE MYR-596 LEGACY BACKLOG ────────────────────
//
// `go_removed_vehicles` rows whose `user_id` resolves to NO identity source —
// no "User", no go_users, no go_identity_apple, no convergence edge — belong to
// accounts deleted before MYR-596's step 8e existed. Each is a (cuid, VIN) pair
// naming a person who is gone. `-purge-orphan-tombstones` counts them; with
// `-apply` it deletes them. It is opt-in and is NOT implied by `-apply`.
//
// IT IS OPT-IN BECAUSE IT COSTS SOMETHING. Those rows are source A. Purging
// them means this tool can never again name the VINs of already-deleted owners,
// so `unreachable_no_token` drops to whatever the LIVE owners contribute. The
// configs are no more and no less unreachable than they were — there is no
// token either way — but the visibility is gone for good.
//
// So the sequence to run is: a plain dry run FIRST, keep the JSON (it carries
// the full VINs), then purge. The purge deliberately runs LAST within a single
// invocation for the same reason — every VIN the run could see is already in
// the report by the time the rows naming them are removed — so
// `-apply -purge-orphan-tombstones` in one go is safe, provided the stdout
// report is kept.
//
// The predicate deliberately spares a CONVERGED person's abandoned cuid, which
// can hold tombstones while carrying no identity row of its own (MYR-452).
// Deleting those would let a live owner's next Tesla sync resurrect a car they
// removed — the MYR-261 bug, reinstated. See
// internal/store/orphan_tombstone_purge.go.
//
// ── WHICH BASE URL THE DELETE USES, AND WHY ─────────────────────────────────
//
// All three Tesla calls (list, config read, config delete) go to the DIRECT
// Fleet API base, not through the tesla-http-proxy. The server sends its
// DELETE through the proxy, but FleetAPIClient.DeleteTelemetryConfig's own doc
// explains why that is incidental: a DELETE carries no config body to sign, so
// the proxy plain-forwards it to Tesla with the bearer token, unmodified.
// Going direct is the same request with one hop fewer. It also means this
// binary needs no TESLA_PROXY_URL and no loopback-TLS exception — which
// matters, because the proxy is a sidecar next to the server and an operator
// running this from a laptop cannot reach it.
//
// Configuration is env-driven, matching the running telemetry-server:
//
//	DATABASE_URL          Postgres connection string (required)
//	DATABASE_DISABLE_PREPARED_STATEMENTS
//	                      "true" for PgBouncer (Supabase 6543); auto-detected
//	                      when the URL contains :6543
//	ENCRYPTION_KEY        base64(32B) — REQUIRED. Tesla tokens are stored as
//	                      AES-256-GCM ciphertext and there is no plaintext
//	                      fallback (MYR-433), so without the key every VIN
//	                      reports unreachable_no_token and the run is a lie.
//	                      ENCRYPTION_KEY_V<N> versioned form also works.
//	FLEET_API_BASE_URL    optional; defaults to the NA prod Fleet API
//	AUTH_TESLA_ID         optional; with AUTH_TESLA_SECRET, enables OAuth
//	AUTH_TESLA_SECRET     refresh so expired-but-refreshable tokens still work
//
// Usage:
//
//	sweep-orphan-fleet-configs                # DRY RUN (default): report only
//	sweep-orphan-fleet-configs -apply         # delete what it can reach
//	sweep-orphan-fleet-configs -max-tombstones 50
//	sweep-orphan-fleet-configs -purge-orphan-tombstones          # count the backlog
//	sweep-orphan-fleet-configs -apply -purge-orphan-tombstones   # and clear it
//
// Exit codes:
//
//	0  the sweep ran (per-VIN failures are IN the report, not in this code)
//	2  fatal — config, DB, or a sweep that could not start
package main
