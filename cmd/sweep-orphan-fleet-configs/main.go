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
// ── WHAT IT LOOKS AT ────────────────────────────────────────────────────────
//
//	A. go_removed_vehicles tombstones (migration 0006) carrying a VIN that no
//	   "Vehicle" row claims. The tombstone's user_id is the only handle on a
//	   token — if that account is gone, so is the token.
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
//
// Exit codes:
//
//	0  the sweep ran (per-VIN failures are IN the report, not in this code)
//	2  fatal — config, DB, or a sweep that could not start
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
		Apply:         *apply,
		MaxTombstones: *maxTombstones,
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
