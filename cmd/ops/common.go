package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// loadEncryptor builds a cryptox.Encryptor from the same env-var schema
// the server uses (ENCRYPTION_KEY or the versioned shape).
//
// MYR-433 made this mandatory for every repo that touches location data,
// not just AccountRepo. Since the plaintext columns are no longer read,
// an ops command without a key can no longer "degrade to plaintext" — it
// would simply print blanks. Failing at construction says so plainly.
func loadEncryptor() (cryptox.Encryptor, error) {
	ks, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		return nil, fmt.Errorf("load encryption key: %w", err)
	}
	enc, err := cryptox.NewEncryptor(ks)
	if err != nil {
		return nil, fmt.Errorf("new encryptor: %w", err)
	}
	return enc, nil
}

// newAccountRepo is the canonical constructor used by every ops
// subcommand that needs to read or write Account tokens. Centralizing it
// here keeps the encryption foundation in one place across auth, fleet,
// and link subcommands.
func newAccountRepo(db *store.DB) (*store.AccountRepo, error) {
	enc, err := loadEncryptor()
	if err != nil {
		return nil, err
	}
	return store.NewAccountRepo(db.Pool(), enc), nil
}

// newVehicleRepo is the canonical constructor for ops subcommands that
// read Vehicle rows. It always wires the encryptor: since MYR-433 the
// GPS and nav-route columns are ciphertext-only, so a keyless
// VehicleRepo silently reports every car at (0, 0) with no destination.
func newVehicleRepo(db *store.DB, logger *slog.Logger) (*store.VehicleRepo, error) {
	enc, err := loadEncryptor()
	if err != nil {
		return nil, err
	}
	return store.NewVehicleRepoWithEncryption(db.Pool(), store.NoopMetrics{}, enc, logger), nil
}

// newDriveRepo is the canonical constructor for ops subcommands that read
// Drive rows, on the same MYR-433 reasoning as newVehicleRepo: a keyless
// DriveRepo reads every GPS trail as empty.
func newDriveRepo(db *store.DB, logger *slog.Logger) (*store.DriveRepo, error) {
	enc, err := loadEncryptor()
	if err != nil {
		return nil, err
	}
	return store.NewDriveRepoWithEncryption(db.Pool(), store.NoopMetrics{}, enc, logger), nil
}

// opsOperatorEnv names the environment variable carrying the operator's
// handle. It is REQUIRED by every subcommand that decrypts user data.
const opsOperatorEnv = "OPS_OPERATOR"

// Field-name vocabularies for the MYR-447 operator-access audit. These are
// NAMES ONLY — store.OperatorAuditor rejects anything value-shaped. Keep
// them in sync with what each subcommand actually decrypts; over-reporting
// is harmless, under-reporting defeats the audit.
var (
	// teslaTokenAuditFields covers store.AccountRepo.GetTeslaToken, which
	// decrypts the Tesla fleet-control credentials.
	teslaTokenAuditFields = []string{"accessToken", "refreshToken"}

	// vehicleRowAuditFields covers store.VehicleRepo.GetByVIN, which
	// decrypts the whole location/navigation surface of the row — every
	// P1 field in data-classification.md §1.3 that MYR-433 moved to
	// ciphertext-only.
	vehicleRowAuditFields = []string{
		"latitude", "longitude", "locationName", "locationAddress",
		"destinationName", "destinationAddress",
		"destinationLatitude", "destinationLongitude",
		"originLatitude", "originLongitude",
		"navRouteCoordinates",
	}
)

// requireOperator resolves the operator's handle from OPS_OPERATOR.
//
// MYR-447: an unattributable decrypt is exactly what the operator audit
// exists to prevent, so there is NO default and NO fallback to the OS
// username — a fallback would silently produce rows attributed to "root"
// or to whoever's laptop the tool happened to run on. Call this first,
// before opening the database, so the operator learns what is missing
// without a connection attempt in between.
func requireOperator() (string, error) {
	handle := strings.TrimSpace(os.Getenv(opsOperatorEnv))
	if err := store.ValidateOperatorHandle(handle); err != nil {
		return "", fmt.Errorf(
			"%s must be set to your operator handle: this command decrypts user data "+
				"and every decrypt is audited (MYR-447); there is no default. "+
				"Example: %s=jdoe ops ... (%w)",
			opsOperatorEnv, opsOperatorEnv, err)
	}
	return handle, nil
}

// newOperatorAuditor builds the audit writer used by every decrypting
// subcommand. Shared here so the three call sites cannot drift on which
// table or which action they write.
func newOperatorAuditor(db *store.DB) *store.OperatorAuditor {
	return store.NewOperatorAuditor(db.Pool())
}

// newLogger returns a text-handler slog logger writing to stderr so
// normal JSON output on stdout remains machine-parseable.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// openDB builds a DatabaseConfig from environment variables and opens a
// connection pool. The caller is responsible for closing the returned DB.
// DATABASE_URL is required. PgBouncer transaction pooling (Supabase port
// 6543) is auto-detected by the presence of :6543 in the URL, matching
// the server's handling of DATABASE_DISABLE_PREPARED_STATEMENTS.
func openDB(ctx context.Context, logger *slog.Logger) (*store.DB, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	cfg := config.DatabaseConfig{
		URL:      url,
		MaxConns: 2,
		MinConns: 1,
		DisablePreparedStatements: strings.Contains(url, ":6543") ||
			os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS") == "true",
	}

	db, err := store.NewDB(ctx, cfg, logger, store.NoopMetrics{})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

// writeJSON marshals v as indented JSON and writes it to w followed by a
// newline. Returns an error if marshalling or writing fails.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

// requireFlag returns the value of flag if non-empty; otherwise returns a
// descriptive usage error.
func requireFlag(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("--%s is required", name)
	}
	return nil
}
