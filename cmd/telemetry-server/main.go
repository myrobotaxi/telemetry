// Binary telemetry-server receives real-time vehicle telemetry from Tesla's
// Fleet Telemetry system and broadcasts it to connected browser clients via
// WebSocket. This file is the composition root — it wires dependencies and
// starts the server. No business logic lives here.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/drives"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/server"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// accountTokenGaugeInterval is how often the running server polls the
// Account table for plaintext-without-ciphertext tokens. The rollout
// window is hours/days, not seconds — 1h cadence is plenty for an
// alerting pipeline and a 12× reduction in COUNT(*) scans vs. the
// prior 5m. MYR-131 flagged the 5m cadence as the bigger disk-IO
// offender on Nano compute.
const accountTokenGaugeInterval = 1 * time.Hour

// vehicleGPSGaugeInterval is the MYR-63 sibling of
// accountTokenGaugeInterval — same 1h cadence over the six Vehicle
// GPS *Enc columns. The two loops are independent so a stall in one
// (e.g., a long-running migration) doesn't starve the other.
const vehicleGPSGaugeInterval = 1 * time.Hour

// routeBlobGaugeInterval is the MYR-64 sibling — same 1h cadence over
// Vehicle.navRouteCoordinatesEnc and Drive.routePointsEnc. The
// route-blob queries can be heavier (jsonb columns), which is exactly
// why their old 5m cadence kept timing out under statement_timeout on
// Nano. 1h gives them headroom and matches the other two gauges.
const routeBlobGaugeInterval = 1 * time.Hour

// storePoolStatsInterval is how often the running server samples the
// pgxpool stats (acquired/idle/total) and pushes them into the
// telemetry_store_pool_*_conns gauges. 15s is short enough to catch
// short-lived contention spikes between scrapes (Prometheus default
// 60s) but long enough that the .Stat() call is invisible at the
// process level. MYR-138 wired this in after MYR-132 found the
// testbench had no pool visibility.
const storePoolStatsInterval = 15 * time.Second

// busDrainTimeout bounds step 2 of the shutdown sequence: running the event
// bus's buffered backlog through the handlers (MYR-410).
const busDrainTimeout = 5 * time.Second

// shutdownDrainBudget is the END-TO-END ceiling on the shutdown closure — the
// bus drain plus the push drains that follow it (MYR-410).
//
// A ceiling is required, not merely tidy. The push workers being waited on run
// on fresh Background contexts that SIGTERM cannot shorten: each fan-out is
// bounded only by the notifier's own Timeout, several run at once, and
// bus.Close hands the drain a whole backlog it did not have a moment earlier.
// Unbounded, that wait can outlast fly.toml's kill_timeout and be SIGKILLed
// mid-push — which loses the same pushes as giving up, minus the log line
// saying so. Both constants are sized inside kill_timeout; the arithmetic lives
// in fly.toml next to it.
const shutdownDrainBudget = 15 * time.Second

// Build-time variables set via ldflags (see .goreleaser.yml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "telemetry-server: %s\n", err)
		os.Exit(1)
	}
}

func run() error { //nolint:funlen,cyclop,gocognit // composition root — sequential dependency wiring; helpers extracted to wiring.go
	// --- Flag parsing ---
	var (
		configPath = flag.String("config", "", "path to JSON configuration file")
		logLevel   = flag.String("log-level", "info", "log level: debug, info, warn, error")
		devMode    = flag.Bool("dev", false, "dev mode: skip JWT auth, accept any token")
	)
	flag.Parse()

	// --- Logger setup ---
	logger, err := newLogger(*logLevel)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	slog.SetDefault(logger)

	logger.Info("starting telemetry-server",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("date", date),
		slog.String("config", *configPath),
	)

	// --- Context with signal-based cancellation ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Configuration loading ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	logger.Info("configuration loaded",
		slog.Int("tesla_port", cfg.Server().TeslaPort),
		slog.Int("client_port", cfg.Server().ClientPort),
		slog.Int("metrics_port", cfg.Server().MetricsPort),
	)

	// --- Startup gates (debug-fields + invite-link signing key) ---
	// Either --dev or a non-empty DEBUG_FIELDS_TOKEN turns on the
	// RawVehicleTelemetryEvent pipeline and mounts /api/debug/fields.
	// In non-dev mode the token must be at least 32 chars so `ops fields
	// watch` can stream real-Tesla data against production behind a
	// real secret.
	//
	// The MYR-368 invite-link signing key is resolved in the same call and
	// FAILS FAST outside --dev: a keyless boot would silently stop emitting
	// `shareUrl` on every invite, which is invisible from inside the running
	// system. Both run before any listener or database connection, so a
	// misconfigured deploy costs nothing to refuse.
	gates, err := resolveStartupGates(*devMode, logger)
	if err != nil {
		return err
	}

	// --- Prometheus registry ---
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// --- Column encryption foundation (NFR-3.23, NFR-3.24) ---
	// MYR-62/63/64 wired the Encryptor into AccountRepo, VehicleRepo,
	// and DriveRepo for the dual-write rollouts of OAuth tokens, GPS
	// coordinates, and route-blob polylines respectively. MYR-65 wires
	// `cryptox_decrypt_total{version="N"}` against this registry so
	// operators can monitor v1-decay during a key rotation
	// (key-rotation.md §"Procedure" step 6).
	encryptor, err := setupEncryption(reg, logger)
	if err != nil {
		return err
	}

	// --- Store metrics (MYR-138) ---
	// Single PrometheusMetrics instance shared by NewDB, VehicleRepo,
	// and DriveRepo so all telemetry_store_* series come from one
	// coherent surface. Replaces the three NoopMetrics{} sites that
	// MYR-132's testbench audit found were hiding DB latency / errors
	// / pool stats from prod observability.
	storeMetrics := store.NewPrometheusMetrics(reg)

	// --- Database connection ---
	db, err := store.NewDB(ctx, cfg.Database(), logger.With(slog.String("component", "store")), storeMetrics)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	// --- Pool-stats collector (MYR-138) ---
	// Periodically samples the pgxpool stats and publishes them via
	// storeMetrics.SetPoolStats so the gauges reflect live state
	// between Prometheus scrapes. Goroutine exits when ctx cancels.
	startPoolStatsCollector(ctx, db, storePoolStatsInterval, logger.With(slog.String("component", "store-pool-stats")))

	// --- Database migrations (Go-owned tables) ---
	// Applies all embedded SQL migrations in internal/store/migrations/ to the
	// _telemetry_* namespace. Prisma-owned tables are never touched here.
	// Fail-fast: a migration error indicates a broken schema that will cause
	// runtime failures -- there is no safe degraded mode.
	// See docs/architecture/migrations.md for the coexistence rule.
	if err := store.RunMigrations(ctx, cfg.Database().URL, logger.With(slog.String("component", "migrations"))); err != nil {
		return fmt.Errorf("running database migrations: %w", err)
	}

	// --- Event bus ---
	bus := events.NewChannelBus(events.BusConfig{
		BufferSize: cfg.Telemetry().EventBufferSize,
	}, events.NoopBusMetrics{}, logger.With(slog.String("component", "events")))

	// --- Shutdown order (MYR-410) ---
	//
	// The push drains are useless without the bus Close that precedes them, and
	// that Close is unsafe without the consumer Stops that precede IT. The three
	// are one ordered sequence, stated here rather than left to be inferred from
	// where each component happens to get built.
	//
	// INVARIANTS. Anything that reorders this — including a merge that moves a
	// defer — has to keep all four:
	//
	//   I1. Every consumer that calls bus.Unsubscribe in its Stop runs BEFORE
	//       bus.Close. Close signals each subscriber it still knows about; a
	//       Stop that unsubscribed afterwards would signal the same one twice.
	//       (Close now deregisters as it signals, so this is a clean
	//       ErrSubscriptionNotFound rather than a panic — but the order is
	//       still the contract.)
	//   I2. bus.Close runs BEFORE any drain Wait.
	//   I3. Every drain Wait runs BEFORE db.Close.
	//   I4. The whole closure stays inside shutdownDrainBudget, which stays
	//       inside fly.toml's kill_timeout.
	//
	// What actually runs, once srv.Start returns — SIGTERM has cancelled ctx and
	// all three HTTP servers have completed their own Shutdown, so nothing new
	// is being published:
	//
	//  1. ridePoller.Stop, serviceStatusMonitor.Stop, broadcaster.Stop,
	//     hub.Stop, writer.Stop, detector.Stop — every consumer that
	//     unsubscribes itself. LIFO lands them here because each is deferred at
	//     its construction site, all of which are below this line.
	//
	//     These Stops SIGNAL; they do not join. Unsubscribe deletes the
	//     subscriber, closes its done channel and returns — the backlog drain
	//     then runs asynchronously on that subscriber's own delivery goroutine.
	//     So a handler can still be executing after its consumer's Stop has
	//     returned. That is tolerable because every one of them only touches
	//     things still alive at this point (the DB pool, which closes at step 4;
	//     the hub, whose Broadcast is a no-op once its client set is empty), and
	//     because of what step 2 actually is.
	//  2. bus.Close — two jobs, and the second is the one that is easy to miss.
	//
	//     Subscriber channels are BUFFERED (EventBufferSize, 1000 in prod), so
	//     an event Publish already accepted can be sitting in a buffer with its
	//     handler never entered: nothing has been counted, and a drain over it
	//     returns instantly. Close signals the remaining subscribers and each
	//     delivery goroutine runs its whole backlog through the handler
	//     synchronously, converting accepted-but-unhandled work into STARTED
	//     work — the only kind a drain can see.
	//
	//     Close also waits on the bus's WaitGroup, which tracks EVERY subscriber
	//     goroutine ever started, including the ones step 1 unsubscribed. That
	//     makes Close the single join point for all of them, which is what
	//     makes step 1's signal-and-return safe.
	//  3. The push drains, bounded — now, and only now, covering the workers
	//     step 2's handlers just started. In the other order they read an empty
	//     counter and return before the handler had even run.
	//  4. db.Close — last, because every handler above can touch the pool.
	//
	// ACCEPTED CONSEQUENCE. Steps 2 and 3 are bounded, so a slow enough handler
	// survives them and is still running at step 4, where its writes fail
	// against a closed pool. That is deliberate: the alternative is an unbounded
	// shutdown that gets SIGKILLed at the same point with nothing logged.
	// Whatever is abandoned is counted and logged loudly below — handlers_live
	// and events_undelivered from the bus, in-flight counts from each drain — so
	// a deploy that dropped a push leaves a number behind.
	//
	// The nav dispatcher also survives to step 2 and has a Wait, deliberately
	// not called here: one dispatch may run for OverallTimeout (2 minutes),
	// which does not fit this budget, so it needs a bounded wait of its own.
	// That, and the fact that step 2 now delivers buffered ride.accepted events
	// into it at shutdown, are tracked on MYR-431.
	var (
		pushNotifier         *push.Notifier
		liveActivityNotifier *push.ActivityNotifier
	)
	defer func() {
		// FRESH contexts throughout, deliberately: ctx is already cancelled by
		// the time this runs, and both Close and the drains derive their
		// deadlines from what they are handed. Given the cancelled ctx they
		// would each return immediately having done nothing, quietly
		// reintroducing the bug this block exists to fix.
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownDrainBudget)
		defer cancelShutdown()

		// Step 2, capped by its own sub-budget and by whatever is left overall.
		closeCtx, cancelClose := context.WithTimeout(shutdownCtx, busDrainTimeout)
		err := bus.Close(closeCtx)
		cancelClose()
		var incomplete *events.DrainIncompleteError
		switch {
		case errors.As(err, &incomplete):
			logger.Error("event bus drain incomplete at shutdown — events were dropped",
				slog.Int("handlers_live", incomplete.HandlersLive),
				slog.Int("events_undelivered", incomplete.EventsUndrained),
			)
		case err != nil:
			logger.Error("event bus close failed", slog.String("error", err.Error()))
		}

		// Step 3, sharing the remainder of the end-to-end budget.
		if pushNotifier != nil {
			if inFlight, waitErr := pushNotifier.WaitContext(shutdownCtx); waitErr != nil {
				logger.Error("alert push drain abandoned at deadline — notifications were dropped",
					slog.Int("fanouts_in_flight", inFlight),
					slog.String("reason", waitErr.Error()),
				)
			}
		}
		if liveActivityNotifier != nil {
			if inFlight, waitErr := liveActivityNotifier.WaitContext(shutdownCtx); waitErr != nil {
				logger.Error("live activity drain abandoned at deadline — updates were dropped",
					slog.Int("updates_in_flight", inFlight),
					slog.String("reason", waitErr.Error()),
				)
			}
		}
	}()

	// --- Telemetry receiver ---
	recv := telemetry.NewReceiver(
		telemetry.NewDecoder(),
		bus,
		logger.With(slog.String("component", "receiver")),
		telemetry.NoopReceiverMetrics{},
		telemetry.ReceiverConfig{
			MaxVehicles:       cfg.Telemetry().MaxVehicles,
			MaxMessagesPerSec: 10,
			// Raw field publication feeds /api/debug/fields. Enabled
			// whenever the debug-fields gate is open (dev mode OR
			// DEBUG_FIELDS_TOKEN set) so operators can tail real-Tesla
			// frames against production without extra deploys.
			PublishRawFields: gates.debugFields.Enabled,
		},
	)

	// --- Store repos (needed by the Detector reconciler) ---
	// MYR-63 wires the Encryptor into VehicleRepo so the six GPS
	// columns are dual-written (plaintext + *Enc) and read with
	// ciphertext preference. Half-pair *Enc rows fall back to
	// plaintext per the atomic-pair invariant in
	// vehicle-state-schema.md §3.3.
	vehicleRepo := store.NewVehicleRepoWithEncryption(db.Pool(), storeMetrics, encryptor, logger.With(slog.String("component", "vehicle-repo")))
	driveRepo := store.NewDriveRepoWithEncryption(db.Pool(), storeMetrics, encryptor, logger.With(slog.String("component", "drive-repo")))
	accountRepo := store.NewAccountRepo(db.Pool(), encryptor)

	// MYR-173/174: ride-request repo. go_ride_requests stores pickup/dropoff
	// coordinates encrypt-only, so the Encryptor is mandatory (the
	// constructor panics on nil). Backs the P10 ride-hailing REST surface.
	rideRepo := store.NewRideRequestRepo(db.Pool(), storeMetrics, encryptor)

	// MYR-186: APNs device registry + the lean vehicle-nickname read the push
	// copy needs. Both are plain reads/writes with no encryption dependency.
	pushRepo := store.NewPushDeviceRepo(db.Pool(), logger.With(slog.String("component", "push-device-repo")))
	// MYR-349: the per-person notification preferences the §7.19 endpoints
	// write and the notifier consults before every send.
	pushPrefsRepo := store.NewPushPrefsRepo(db.Pool(), logger.With(slog.String("component", "push-prefs-repo")))
	// MYR-184 vehicle sharing: the go_vehicle_shares repository behind the
	// §7.5 invite/redeem endpoints and behind every viewer access check.
	shareRepo := store.NewVehicleShareRepo(db.Pool(), logger.With(slog.String("component", "vehicle-share-repo")))
	vehicleNameRepo := store.NewVehicleNameRepo(db.Pool())
	// MYR-172: the ActivityKit push-token registry behind the rider's Live
	// Activity, written by the §7.21 endpoints and read by both the lifecycle
	// consumer and the ETA ticker.
	liveActivityRepo := store.NewLiveActivityRepo(db.Pool(), logger.With(slog.String("component", "live-activity-repo")))

	// --- Drive detector ---
	// MYR-146: reconciler reads open Drive rows on Start so a Fly
	// redeploy mid-drive doesn't leave forever-active rows. The
	// adapter lives in adapters.go and translates store.OpenDriveRow
	// into the drives package's local type.
	detector := drives.NewDetector(
		bus,
		cfg.Drives(),
		logger.With(slog.String("component", "drives")),
		drives.NewPrometheusDetectorMetrics(reg),
		&openDriveListerAdapter{repo: driveRepo},
	)
	if err := detector.Start(ctx); err != nil {
		return fmt.Errorf("starting drive detector: %w", err)
	}
	defer func() { _ = detector.Stop() }()

	// MYR-62 + MYR-63 plaintext-zero gauges. Both register against the
	// same Prometheus registry the /metrics handler scrapes; each
	// refreshes on its own goroutine until the rollouts complete. See
	// startPlaintextGauges in wiring.go.
	startPlaintextGauges(ctx, reg, db.Pool(), accountTokenGaugeInterval, vehicleGPSGaugeInterval, routeBlobGaugeInterval, logger)

	// --- TLS endpoint cert monitor (MYR-188) ---
	// Probes the served leaf cert on each configured public endpoint so an
	// impending expiry pages BEFORE it takes down customer traffic —
	// including the Fly-terminated 4443 cert that no file monitor can see.
	startCertEndpointMonitor(ctx, reg, cfg.Monitoring().CertEndpoints, logger)

	// --- Audit sidecar (MYR-77) ---
	// Best-effort S3 mirror of every AuditLog INSERT.
	// No-op when AUDIT_SIDECAR_BUCKET is empty (local dev).
	// Production: set AUDIT_SIDECAR_BUCKET + AUDIT_SIDECAR_REGION; the service
	// IAM role (telemetry-server-audit-sidecar) grants s3:PutObject only —
	// see deployments/terraform/audit-sidecar/iam.tf.
	auditRepo, err := buildAuditRepo(ctx, reg, db.Pool(), logger)
	if err != nil {
		return fmt.Errorf("building audit repo: %w", err)
	}

	// --- Mask-audit emitter (MYR-71, rest-api.md §5.3) ---
	// MaskAuditEmitter adapts AuditRepo to the mask.AuditEmitter
	// interface so the hub and any future REST mask paths can fire
	// non-blocking audit rows. Prometheus counters
	// telemetry_audit_log_writes_total{action,target} and
	// telemetry_audit_log_write_failures_total{action,target} make
	// the 1% sample rate observable in prod.
	auditEmitter := store.NewMaskAuditEmitter(auditRepo)
	auditMetrics := mask.NewPrometheusAuditMetrics(reg)

	// --- Geocoder (optional — requires MAPBOX_TOKEN) ---
	geo := newGeocoder(cfg.MapboxToken(), cfg.Drives().GeocodeTimeout, logger)

	// --- Persistence writer ---
	writer := store.NewWriter(
		vehicleRepo, driveRepo, vehicleRepo, bus, geo,
		logger.With(slog.String("component", "writer")),
		store.WriterConfig{
			FlushInterval: cfg.Telemetry().BatchWriteInterval,
			BatchSize:     cfg.Telemetry().BatchWriteSize,
		},
	)
	if err := writer.Start(ctx); err != nil {
		return fmt.Errorf("starting persistence writer: %w", err)
	}
	defer func() { _ = writer.Stop() }()

	// --- WebSocket hub + broadcaster ---
	// WithMaskAudit wires the per-(vehicleID, role, frame) mask audit
	// emit at the hub layer per rest-api.md §5.3. The hub itself uses
	// a per-vehicle in-process counter as frameSeq until DV-02 ships
	// envelope sequence numbers.
	hub := ws.NewHub(
		logger.With(slog.String("component", "ws")),
		ws.NoopHubMetrics{},
		ws.WithMaskAudit(auditEmitter, auditMetrics),
		// MYR-137: unicast a per-atomic-group snapshot to a client on
		// subscribe so model/year/color/estimatedRange/fsdMilesSinceReset
		// (already persisted per MYR-24) reach the WS wire immediately
		// instead of only via the REST snapshot endpoint.
		ws.WithVehicleSnapshotReader(&wsVehicleSnapshotAdapter{repo: vehicleRepo}),
	)
	defer hub.Stop()

	// Shared VIN → (vehicleID, userID) cache backing the broadcaster and
	// the HTTP handlers below. Both identifiers are immutable for the
	// lifetime of a vehicle row, so the cache lives forever and a single
	// slim two-column query runs per VIN for the lifetime of the process.
	// This replaces ~660k full-row fetches per billing cycle that were
	// pulling the heavy navRouteCoordinates JSON on every telemetry frame.
	vinCache := store.NewVINCache(vehicleRepo, logger.With(slog.String("component", "vin-cache")))
	recv.SetAuthorizer(&vehicleAuthorizerAdapter{cache: vinCache})

	vinResolver := &vinResolverAdapter{cache: vinCache}
	broadcaster := ws.NewBroadcaster(hub, bus, vinResolver, logger.With(slog.String("component", "broadcaster")))
	if err := broadcaster.Start(ctx); err != nil {
		return fmt.Errorf("starting broadcaster: %w", err)
	}
	defer func() { _ = broadcaster.Stop() }()

	go hub.RunHeartbeat(ctx, cfg.WebSocket().HeartbeatInterval)

	// --- In-service status monitor (MYR-259 Leg 2) ---
	// Subscribes to connectivity edges and fires ONE debounced authoritative
	// Tesla REST in_service read per edge, persisting status=in_service.
	// Event-driven — NO timer/poll. Reads never touch the car, so this is
	// wired unconditionally (owners without a Tesla token are simply skipped).
	serviceStatusMonitor, err := setupServiceStatusMonitor(cfg, bus, vinCache, vehicleRepo, accountRepo, logger)
	if err != nil {
		return fmt.Errorf("starting service-status monitor: %w", err)
	}
	defer serviceStatusMonitor.Stop()

	// MYR-320: the edge subscriptions above are blind to a car that sits
	// in_service — typically offline — for days, because it raises no
	// connectivity edge and streams no ServiceMode transition. This loop
	// re-runs the SAME read bundle for every in-service vehicle on a jittered
	// ~15m cadence, with one pass shortly after startup so a deploy takes
	// effect in seconds. No-ops when SERVICE_REPOLL_ENABLED=false.
	go serviceStatusMonitor.RunPeriodicInServicePoll(ctx)

	// --- Identity module keystore (MYR-193, ADR-001) ---
	// The ES256 signing keystore backs both access-token minting and the
	// dual-alg validator's ES256 verification. Nil => module disabled
	// (HS256-only). Built before the authenticator so its public-key resolver
	// can be injected into JWT validation.
	keystore, err := buildKeystore(cfg, *devMode, logger)
	if err != nil {
		return fmt.Errorf("building identity keystore: %w", err)
	}
	var es256Resolver auth.ES256KeyResolver
	if keystore != nil {
		es256Resolver = keystore
	}

	// --- Client authenticator ---
	authenticator := setupAuthenticator(cfg, db.Pool(), *devMode, es256Resolver, logger)

	// --- vehicle_deleted cleanup pipeline (FR-10.1 / data-lifecycle.md §3.5, MYR-73) ---
	// Postgres LISTEN/NOTIFY goroutine + dispatcher that fans the event
	// out to the WS hub, the Tesla receiver, the VIN cache, and the JWT
	// user-existence cache. Production wires a real JWTAuthenticator;
	// dev mode uses NoopAuthenticator (no user cache to invalidate).
	jwtAuth, _ := authenticator.(*auth.JWTAuthenticator)

	// MYR-184: the sharing handlers bust the access-set cache on redeem and
	// revoke. Only the real JWTAuthenticator owns that cache — dev mode runs
	// a NoopAuthenticator with nothing to invalidate — so the interface stays
	// nil there rather than carrying a typed-nil pointer, which the handlers
	// check for and skip.
	var accessInvalidator telemetry.AccessCacheInvalidator
	if jwtAuth != nil {
		accessInvalidator = jwtAuth
	}

	// MYR-355: account deletion drops BOTH caches — the access set AND the
	// user-existence entry that keeps a deleted user's unexpired access token
	// validating. Same typed-nil avoidance as above.
	var sessionInvalidator telemetry.AccountSessionInvalidator
	if jwtAuth != nil {
		sessionInvalidator = jwtAuth
	}

	dispatcher := newVehicleDeletedDispatcher(hub, recv, vinCache, jwtAuth, logger.With(slog.String("component", "vehicle-deleted-dispatcher")))
	if _, err := dispatcher.Subscribe(bus); err != nil {
		return fmt.Errorf("subscribe vehicle_deleted dispatcher: %w", err)
	}
	runNotifyListener(ctx, cfg.Database().URL, bus, logger)

	// --- Push notifications (MYR-186) ---
	// Subscribes to ride.request.created / ride.status.changed / ride.due and
	// alerts the relevant party's registered phones. Gated by PUSH_ENABLED;
	// runs keyless (every send logged as skipped) until the APNs secrets are
	// set. Drained after the bus closes so in-flight sends finish.
	//
	// ONE APNs client serves both consumers below: the provider JWT is cached
	// per client and Apple throttles re-minting the same key, so a second
	// client over one key buys nothing and risks 403 ExpiredProviderToken.
	apnsClient, err := buildAPNsClient(cfg, logger.With(slog.String("component", "push")))
	if err != nil {
		return fmt.Errorf("setting up apns client: %w", err)
	}

	// liveActivityRepo is passed for the MYR-413 duplicate-banner gate: a
	// lifecycle banner is skipped when the recipient already has a running
	// Live Activity for that ride, whose island alert carries the same news.
	notifier, err := setupPushNotifier(
		cfg, bus, apnsClient, pushRepo, pushPrefsRepo, liveActivityRepo, vehicleNameRepo, logger)
	if err != nil {
		return fmt.Errorf("setting up push notifier: %w", err)
	}
	// Drained by the shutdown block above, AFTER bus.Close — not here. A
	// `defer notifier.Wait()` at this line would run before the bus had handed
	// the handler its buffered backlog (MYR-410).
	pushNotifier = notifier

	// --- Live Activity push updates (MYR-172) ---
	// The lifecycle consumer subscribes to ride.status.changed and replaces the
	// rider's Live Activity content-state on every transition, ending it with a
	// dismissal-date on a terminal one. The ticker refreshes the ETA every
	// 60-90s while a leg is active (MYR-194 decision 3) and sweeps registrations
	// whose Activity died on the phone without telling us.
	activityNotifier, err := setupLiveActivityNotifier(cfg, bus, apnsClient, liveActivityRepo, pushPrefsRepo, logger)
	if err != nil {
		return fmt.Errorf("setting up live activity notifier: %w", err)
	}
	// Drained by the shutdown block above, after bus.Close — see MYR-410.
	liveActivityNotifier = activityNotifier
	startLiveActivityTicker(ctx, cfg, activityNotifier, liveActivityRepo, logger)

	// --- Nav-dispatch (MYR-176) ---
	// Subscribes to the ride.accepted seam (published by the owner-accept
	// handler) and pushes the rider's pickup into the vehicle's Tesla
	// navigation via the command Executor. Gated by DISPATCH_ENABLED.
	//
	// Started AFTER the Live Activity notifier because the reservation sweeper
	// it starts needs it: a reservation that runs past its lateness ceiling is
	// recorded as a dispatch failure and leaves the ride row at `accepted`, so
	// that ending is invisible on the event bus and has to be handed to the
	// Activity directly (MYR-172).
	if err := setupNavDispatcher(ctx, cfg, bus, activityNotifier, vehicleRepo, accountRepo, rideRepo, shareRepo, logger); err != nil {
		return fmt.Errorf("setting up nav dispatcher: %w", err)
	}

	// --- Ride-scoped position polling (MYR-394) ---
	// A car that is in service, asleep, or offline streams nothing, so while it
	// carries out a ride the tracking map was rendering the last fix from
	// before it went quiet. This polls Tesla's vehicle_data for position every
	// ~25s for the DURATION OF THE RIDE ONLY, through the existing MYR-260
	// backfill path — so the frame is tagged SourceRESTBackfill, the MYR-300
	// recency gate makes it a no-op for a car that IS streaming, and nothing
	// new appears on the wire. Never wakes a sleeping car. No-ops entirely when
	// RIDE_POSITION_POLL_ENABLED=false.
	//
	// Started AFTER the nav dispatcher so that a reservation the dispatch
	// reconcile has just resolved is already in its final state before the poll
	// reconcile decides which rides are live.
	ridePoller, err := setupRidePositionPoller(
		ctx, cfg, bus, serviceStatusMonitor, vinCache, vehicleRepo, accountRepo, rideRepo, logger)
	if err != nil {
		return fmt.Errorf("setting up ride position poller: %w", err)
	}
	defer ridePoller.Stop()

	// --- HTTP server + route registration ---
	srv := server.New(cfg.Server(), logger, db, reg, cfg.TeslaPublicKey())
	originPatterns := resolveWSOriginPatterns(cfg.WebSocket().AllowedOrigins, *devMode, logger)
	setupHTTPHandlers(httpRouteDeps{
		cfg:           cfg,
		srv:           srv,
		hub:           hub,
		authenticator: authenticator,
		recv:          recv,
		bus:           bus,
		vinCache:      vinCache,
		vehicleRepo:   vehicleRepo,
		driveRepo:     driveRepo,
		rideRepo:      rideRepo,
		accountRepo:   accountRepo,
		pushRepo:      pushRepo,
		pushPrefsRepo: pushPrefsRepo,
		// MYR-172: backs the §7.21 Live Activity token endpoints.
		liveActivityRepo: liveActivityRepo,
		// The same notifier the bus subscription and the ticker share; the
		// teardown endpoint uses it to end Activities before deleting rides.
		activityNotifier: activityNotifier,
		shareRepo:        shareRepo,
		inviteLinks:      gates.inviteLinks,
		// The authenticator owns the access-set cache the sharing handlers
		// bust on redeem and revoke.
		accessInvalidator: accessInvalidator,
		// The same authenticator also owns the user-existence cache the
		// account-deletion endpoint must drop (MYR-355).
		sessionInvalidator: sessionInvalidator,
		pool:               db.Pool(),
		encryptor:          encryptor,
		auditEmitter:       auditEmitter,
		auditMetrics:       auditMetrics,
		debugGate:          gates.debugFields,
		originPatterns:     originPatterns,
		serviceStatus:      serviceStatusMonitor,
		logger:             logger,
	})

	// --- Identity module endpoints (MYR-193, ADR-001) ---
	// Native Sign in with Apple, ES256 access-token minting, refresh
	// rotation, and the public JWKS at /api/auth/.well-known/jwks.json.
	// No-op when the keystore is disabled.
	setupIdentityEndpoints(cfg, srv, keystore, db.Pool(), logger)

	// --- Tesla mTLS ---
	if err := setupTeslaTLS(cfg, srv, logger); err != nil {
		return err
	}

	logger.Info("starting HTTP servers")
	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	logger.Info("telemetry-server stopped cleanly")
	return nil
}
