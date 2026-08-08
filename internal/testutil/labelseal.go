// Package testutil holds helpers shared by test suites that live in
// DIFFERENT packages and therefore cannot reach each other's unexported
// test code: the external `store_test` package under `internal/store`
// and the build-tagged `contract_test` harness under `tests/contract`.
//
// Nothing here is imported by production code — it exists only so two
// test packages can agree on how a fixture is built.
package testutil

import (
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// SealLabel seals one geocoded location label (MYR-447) the way the
// production write paths do, returning the *string pgx wants for a
// nullable TEXT parameter slot. A nil result writes NULL.
//
// The empty string seals to nil, not to an empty ciphertext: NULL is the
// absent sentinel the geocode backfill's `IS NULL` discovery predicate
// keys on, and an empty label carries no location worth storing. This is
// the test-side mirror of the unexported store.labelToEncPtr — the two
// MUST agree, because a seed that sealed a label differently from the
// writer would exercise a row shape production never produces.
//
// Every test that plants a sealed label goes through here, so there is
// exactly one way to do it: `internal/store/db_test.go`'s
// sealCatalogLabels and `tests/contract`'s vehicle/drive seeds.
func SealLabel(enc cryptox.Encryptor, plain string) (*string, error) {
	if plain == "" {
		return nil, nil //nolint:nilnil // empty label is absent: write NULL
	}
	ct, err := enc.EncryptString(plain)
	if err != nil {
		return nil, fmt.Errorf("seal label: %w", err)
	}
	return &ct, nil
}
