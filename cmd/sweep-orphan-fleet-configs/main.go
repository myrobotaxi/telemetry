// The sweep binary itself. Its full operator documentation — the honest
// limits, the per-VIN labels, the flags and the environment — is the package
// comment in doc.go, split out for the 300-line file cap. READ IT before
// acting on this tool's output.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/fleetorphan"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

const (
	exitOK    = 0
	exitFatal = 2
)

// Pool bounds for a binary that issues a handful of short reads. Typed int32 to
// match pgxpool directly. See cmd/backfill-name-confirmations for the argument.
const (
	poolMaxConns int32 = 2
	poolMinConns int32 = 1
)

func main() { os.Exit(run()) }

// run is the testable seam — separated so a test can drive it without os.Exit.
func run() int {
	apply := flag.Bool("apply", false,
		"actually delete. Default is a dry run that writes nothing and issues no Tesla DELETE.")
	maxTombstones := flag.Int("max-tombstones", 0,
		"cap the removed-vehicle tombstone read (0 = package default).")
	purgeOrphanTombstones := flag.Bool("purge-orphan-tombstones", false,
		"also clear the MYR-596 legacy backlog: go_removed_vehicles rows whose owner no longer "+
			"exists in any identity source. Counted on a dry run, deleted under -apply. "+
			"READ THE HEADER: this destroys source A, so those VINs stop being reported at all.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sweep-orphan-fleet-configs: %s\n", err)
		return exitFatal
	}
	defer pool.Close()

	deps, err := buildDeps(pool, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sweep-orphan-fleet-configs: %s\n", err)
		return exitFatal
	}

	rep, runErr := fleetorphan.New(deps, fleetorphan.Config{
		Apply:                 *apply,
		MaxTombstones:         *maxTombstones,
		PurgeOrphanTombstones: *purgeOrphanTombstones,
	}, logger).Run(ctx)
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "sweep-orphan-fleet-configs: %s\n", runErr)
		return exitFatal
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintf(os.Stderr, "sweep-orphan-fleet-configs: write report: %s\n", err)
		return exitFatal
	}
	return exitOK
}

// buildDeps wires the sweep's collaborators over the pool.
func buildDeps(pool *pgxpool.Pool, logger *slog.Logger) (fleetorphan.Deps, error) {
	keySet, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		return fleetorphan.Deps{}, fmt.Errorf("load encryption key: %w", err)
	}
	encryptor, err := cryptox.NewEncryptor(keySet)
	if err != nil {
		return fleetorphan.Deps{}, fmt.Errorf("build encryptor: %w", err)
	}

	accountRepo := store.NewAccountRepo(pool, encryptor)

	// ONE client, on the direct Fleet API base, for all three calls. See the
	// base-URL section of the header for why the DELETE does not need the proxy.
	client := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL:    os.Getenv("FLEET_API_BASE_URL"), // empty => default NA Fleet API
		HTTPClient: &http.Client{Timeout: fleetCallTimeout},
	}, logger.With(slog.String("subcomponent", "fleet")))
	fleet := &fleetAdapter{client: client}

	var opts []telemetry.TeslaTokenResolverOption
	if id := os.Getenv("AUTH_TESLA_ID"); id != "" {
		refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
			ClientID:     id,
			ClientSecret: os.Getenv("AUTH_TESLA_SECRET"),
		}, logger.With(slog.String("subcomponent", "token-refresh")))
		opts = append(opts,
			telemetry.WithResolverRefresher(refresher, &tokenUpdater{repo: accountRepo}),
			telemetry.WithResolverRotator(&tokenRotator{repo: accountRepo}),
		)
	} else {
		logger.Warn("AUTH_TESLA_ID is unset: expired tokens cannot be refreshed, " +
			"so some reachable VINs will report failed rather than being cleaned")
	}
	resolver := telemetry.NewTeslaTokenResolver(&tokenProvider{repo: accountRepo},
		logger.With(slog.String("subcomponent", "token")), opts...)

	return fleetorphan.Deps{
		Store:   orphanStore{FleetConfigOrphanRepo: store.NewFleetConfigOrphanRepo(pool)},
		Fleet:   fleet,
		Reader:  fleet,
		Deleter: fleet,
		Tokens:  &tokenSource{resolver: resolver},
	}, nil
}

const fleetCallTimeout = 30 * time.Second

// openPool builds a pgxpool from DATABASE_URL using the same PgBouncer-aware
// logic as the server's openDB helper.
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	cfg := config.DatabaseConfig{
		URL: url,
		DisablePreparedStatements: strings.Contains(url, ":6543") ||
			os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS") == "true",
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = poolMaxConns
	poolCfg.MinConns = poolMinConns
	if cfg.DisablePreparedStatements {
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
