package store

import (
	"strings"
	"testing"
)

// MYR-594 — the SQL properties the keepalive's safety rests on, asserted as
// statement text so they survive an edit that "tidies" them.
//
// These are deliberately text assertions rather than behaviour tests against a
// database. Each one names a clause whose ABSENCE would still compile, still
// pass every integration test on an uncontended row, and still be wrong: a
// rotation without the lock races, a lock without NOWAIT queues behind a user's
// own reconnect, and a candidate query missing any one of its refusals rotates
// credentials nobody asked us to touch.

func TestRotationQueryTakesAnAbandoningRowLock(t *testing.T) {
	tests := []struct {
		name   string
		clause string
		reason string
	}{
		{
			name: "the read takes an exclusive row lock", clause: "FOR UPDATE",
			reason: "the read and the write are one transaction; without the lock another " +
				"writer can commit between choosing a single-use refresh token and storing " +
				"its replacement, and Postgres row locks are the only thing that blocks the " +
				"on-demand path, which takes no lock of its own",
		},
		{
			name: "and abandons rather than queues", clause: "NOWAIT",
			reason: "waiting for the lock means rotating a grant the instant a user's own " +
				"reconnect has finished with it; abandoning costs one pass against a 45-day margin",
		},
		{
			name: "it is scoped to the Tesla grant", clause: `"provider" = 'tesla'`,
			reason: "an account may hold other OAuth providers, and none of them are ours to spend",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(queryLockTeslaToken, tc.clause) {
				t.Errorf("queryLockTeslaToken lost %q (%s)", tc.clause, tc.reason)
			}
		})
	}
}

// TestRotationWritesThroughTheOnDemandStatement pins the reuse. The keepalive
// and the on-demand refresh must not grow two different ideas of what a
// rotation writes — the plaintext scrub in particular is a security behaviour
// that a second, parallel UPDATE would silently drop.
func TestRotationWritesThroughTheOnDemandStatement(t *testing.T) {
	for _, want := range []string{`"access_token_enc"`, `"refresh_token_enc"`, `"expires_at"`} {
		if !strings.Contains(queryUpdateTeslaToken, want) {
			t.Errorf("the shared rotation write no longer sets %s", want)
		}
	}
	if !strings.Contains(queryUpdateTeslaToken, `"access_token" = NULL`) {
		t.Error("the shared rotation write no longer scrubs the legacy plaintext column; " +
			"the keepalive rotates dormant accounts, which are exactly the rows most likely " +
			"to still be carrying a pre-MYR-433 plaintext credential")
	}
}

// TestStaleTokenOwnerQueryRefusals pins every predicate that keeps this arm
// away from somebody it should not touch.
func TestStaleTokenOwnerQueryRefusals(t *testing.T) {
	tests := []struct {
		name   string
		clause string
		reason string
	}{
		{
			name: "only suspended vehicles", clause: "s.suspended_at IS NOT NULL",
			reason: "the arm exists for the one population that makes no Tesla calls of its own; " +
				"widening this turns a repair into a rotation of the whole fleet's credentials",
		},
		{
			name: "only grants with something to spend", clause: `a."refresh_token_enc" IS NOT NULL`,
			reason: "there is nothing to rotate without a stored refresh token",
		},
		{
			name: "never on an unknown clock", clause: `a."expires_at" IS NOT NULL`,
			reason: "a NULL expiry is unknown, not infinitely stale, and this arm never acts on unknown",
		},
		{
			name: "the staleness gate", clause: `to_timestamp(a."expires_at") <= $1`,
			reason: "the rotation clock is the OAuth row's own expiry — the one value every " +
				"writer of that row moves, including the sibling app's refresh",
		},
		{
			name: "the anti-race quiet gate", clause: "ua.last_seen_at <= $2",
			reason: "an owner who has just authenticated might be reaching for the reconnect button",
		},
		{
			name: "the rotation-loop brake", clause: "k.last_attempt_at <= $3",
			reason: "a rotation whose store failed leaves the clock unmoved; without this the " +
				"next pass spends another single-use token on the same broken write",
		},
		{
			name: "the failure cooldown", clause: "k.last_failure_at <= $4",
			reason: "a refused grant is lapsed or revoked and no retry cures either",
		},
		{
			name: "released early when the grant changes", clause: `k.failed_token_expiry IS DISTINCT FROM a."expires_at"`,
			reason: "a re-link or reconnect makes the reason for the cooldown obsolete, and " +
				"IS DISTINCT FROM reads a NULL as different so the safe direction is to retry",
		},
		{
			name: "one row per owner", clause: `GROUP BY v."userId"`,
			reason: "a person with three suspended cars holds one grant; rotating it three " +
				"times in a pass would spend the pair the first rotation just minted",
		},
		{
			name: "stalest first", clause: "ORDER BY token_expires_at ASC",
			reason: "with a per-pass cap, the attempts must go to the tokens closest to lapsing",
		},
		{
			name: "capped", clause: "LIMIT $5",
			reason: "the cap has to reach the database, or a backlog arrives in memory first",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(queryStaleTokenOwners, tc.clause) {
				t.Errorf("queryStaleTokenOwners lost %q (%s)", tc.clause, tc.reason)
			}
		})
	}
}

// TestKeepaliveRecordsKeepTheirHistory. A success clears the outstanding
// failure (the account is demonstrably alive again) but a failure must NOT
// clear the last success — an operator asking "did this ever work?" needs both
// answers, and the two statements are the only place that can disagree.
func TestKeepaliveRecordsKeepTheirHistory(t *testing.T) {
	if !strings.Contains(queryRecordKeepaliveSuccess, "last_failure_at     = NULL") {
		t.Error("a successful rotation no longer clears the outstanding failure; the owner " +
			"would keep serving a cooldown for a grant that has just been proven good")
	}
	if strings.Contains(queryRecordKeepaliveFailure, "last_success_at") {
		t.Error("a refusal touches last_success_at; a failure today does not un-happen a " +
			"success last month, and the column is the only durable record that the arm works")
	}
	for _, q := range []string{queryRecordKeepaliveSuccess, queryRecordKeepaliveFailure} {
		if !strings.Contains(q, "last_attempt_at") {
			t.Error("an outcome that does not stamp last_attempt_at leaves the rotation-loop " +
				"brake off for that path")
		}
	}
}
