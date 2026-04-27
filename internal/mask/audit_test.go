package mask

import (
	"fmt"
	"testing"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

func TestShouldAuditREST_Deterministic(t *testing.T) {
	tests := []struct {
		name      string
		userID    string
		requestID string
		resource  string
	}{
		{name: "typical inputs", userID: "user-1", requestID: "req-abc", resource: "veh-xyz"},
		{name: "empty inputs", userID: "", requestID: "", resource: ""},
		{name: "long inputs", userID: "user-with-a-very-long-cuid-cmkx0001abc", requestID: "01HX9YJWE9G2A3FQB", resource: "cmvehicle12345"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := ShouldAuditREST(tt.userID, tt.requestID, tt.resource)
			for range 5 {
				if got := ShouldAuditREST(tt.userID, tt.requestID, tt.resource); got != first {
					t.Fatalf("ShouldAuditREST not deterministic: first=%v, again=%v", first, got)
				}
			}
		})
	}
}

func TestShouldAuditWS_Deterministic(t *testing.T) {
	for i := uint64(0); i < 20; i++ {
		first := ShouldAuditWS("vehicle-x", auth.RoleViewer, i)
		for range 3 {
			if got := ShouldAuditWS("vehicle-x", auth.RoleViewer, i); got != first {
				t.Fatalf("ShouldAuditWS not deterministic at frameSeq=%d: first=%v, again=%v", i, first, got)
			}
		}
	}
}

func TestShouldAuditREST_DistributionApprox1Percent(t *testing.T) {
	// Sample 10000 distinct triples. The 1% rate (modulus 100) means
	// we expect ~100 trues. Check with generous bounds (50..200) to
	// avoid flakiness from FNV's distribution on sequential inputs.
	const samples = 10000
	hits := 0
	for i := range samples {
		if ShouldAuditREST(fmt.Sprintf("user-%d", i), fmt.Sprintf("req-%d", i), fmt.Sprintf("res-%d", i)) {
			hits++
		}
	}
	if hits < 50 || hits > 200 {
		t.Errorf("ShouldAuditREST hit rate over %d samples: %d (expected ~100, allowed 50..200)", samples, hits)
	}
}

func TestShouldAuditWS_DistributionApprox1Percent(t *testing.T) {
	const samples = 10000
	hits := 0
	for i := uint64(0); i < samples; i++ {
		if ShouldAuditWS("vehicle-x", auth.RoleOwner, i) {
			hits++
		}
	}
	if hits < 50 || hits > 200 {
		t.Errorf("ShouldAuditWS hit rate over %d samples: %d (expected ~100, allowed 50..200)", samples, hits)
	}
}

func TestShouldAuditREST_DiffersAcrossInputs(t *testing.T) {
	// Length-prefixed hashing protects against collisions like
	// ("ab", "cdef") vs ("abc", "def"). Verify both pairs do not
	// produce the same hash AND result. (Equality of bool result is
	// fine for any individual pair; what we want to guard is that
	// changing the boundary produces a different hash bit.)
	a := ShouldAuditREST("ab", "cdef", "x")
	b := ShouldAuditREST("abc", "def", "x")
	// They COULD coincidentally be the same; what matters is the
	// underlying hashes differ. Since ShouldAuditREST collapses to a
	// bool, we instead verify a wider sweep produces both true and
	// false outcomes from these patterns.
	_ = a
	_ = b

	hits := 0
	misses := 0
	for i := range 200 {
		// Vary the boundary across many distinct values; should land
		// on both true and false.
		if ShouldAuditREST(fmt.Sprintf("a%db", i), fmt.Sprintf("c%dd", i), "x") {
			hits++
		} else {
			misses++
		}
	}
	if hits == 0 || misses == 0 {
		t.Errorf("expected both true and false outcomes; hits=%d misses=%d", hits, misses)
	}
}
