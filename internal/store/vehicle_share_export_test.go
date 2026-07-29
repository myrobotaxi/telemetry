package store

// Test-only export of the invite-code minter.
//
// newShareCode is unexported on purpose — a code is a live bearer credential
// and nothing outside this package has any business minting one. The
// integration tests in package store_test still need to exercise the sampler's
// distribution directly (a rejection-sampling bug that narrows the alphabet is
// invisible through the repository), so the seam is opened here, in a _test.go
// file that never ships in the binary.
func NewShareCodeForTest() (string, error) { return newShareCode() }
