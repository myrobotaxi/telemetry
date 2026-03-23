// Package sdk defines shared interfaces, types, and sentinel errors used
// across internal packages. It contains NO implementation — only contracts.
package sdk

import "errors"

// ErrNotFound is a sentinel error for "entity not found" lookups.
// Internal packages should wrap this with context:
//
//	fmt.Errorf("VehicleRepo.GetByVIN(%s): %w", vin, sdk.ErrNotFound)
//
// Callers use errors.Is(err, sdk.ErrNotFound) to detect not-found cases
// without depending on the package that originated the error.
var ErrNotFound = errors.New("not found")
