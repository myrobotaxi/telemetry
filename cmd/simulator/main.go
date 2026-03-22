// Binary simulator is a mock Tesla vehicle that sends fake protobuf telemetry
// to the server for pipeline testing. No real car needed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tnando/my-robo-taxi-telemetry/internal/simulator"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "simulator: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		serverURL    = flag.String("server", "", "WebSocket URL of the telemetry server (required)")
		scenarioName = flag.String("scenario", "highway-drive", "scenario: "+strings.Join(simulator.ScenarioNames(), ", "))
		vehicles     = flag.Int("vehicles", 1, "number of simultaneous vehicles")
		vinPrefix    = flag.String("vin", "5YJ3SIM", "VIN prefix for simulated vehicles")
		interval     = flag.Duration("interval", time.Second, "telemetry send interval")
		routePath    = flag.String("route", "", "path to custom route JSON file (default: configs/routes/<scenario>.json)")
		certPath     = flag.String("cert", "certs/client.crt", "path to client TLS certificate")
		keyPath      = flag.String("key", "certs/client.key", "path to client TLS key")
		caPath       = flag.String("ca", "certs/ca.crt", "path to CA certificate")
	)
	flag.Parse()

	if *serverURL == "" {
		return fmt.Errorf("--server flag is required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Validate cert files exist before attempting TLS setup.
	for _, f := range []struct{ flag, path string }{
		{"--cert", *certPath},
		{"--key", *keyPath},
		{"--ca", *caPath},
	} {
		if f.path == "" {
			continue
		}
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("%s %q not found: run scripts/generate-certs.sh first: %w", f.flag, f.path, err)
		}
	}

	tlsConfig, err := buildTLSConfig(*certPath, *keyPath, *caPath)
	if err != nil {
		return fmt.Errorf("configuring TLS: %w", err)
	}

	// Load route file for scenarios that support it.
	scenarioOpts := loadRouteOpts(*scenarioName, *routePath, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting simulator",
		slog.String("server", *serverURL),
		slog.String("scenario", *scenarioName),
		slog.Int("vehicles", *vehicles),
		slog.String("vin_prefix", *vinPrefix),
		slog.Duration("interval", *interval),
	)

	g, gctx := errgroup.WithContext(ctx)
	for i := range *vehicles {
		vin := fmt.Sprintf("%s%05d", *vinPrefix, i+1)
		scenario := simulator.NewScenario(*scenarioName, scenarioOpts...)
		if scenario == nil {
			return fmt.Errorf("unknown scenario %q (valid: %s)", *scenarioName, strings.Join(simulator.ScenarioNames(), ", "))
		}
		v := simulator.NewVehicle(vin, scenario, logger, *interval)
		g.Go(func() error {
			return v.Run(gctx, *serverURL, tlsConfig)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("simulation: %w", err)
	}

	logger.Info("all vehicles finished")
	return nil
}

// loadRouteOpts attempts to load a route file for the given scenario.
// Returns scenario options that include the route if found, or empty
// options if no route is available (the scenario will fall back to
// random-walk positioning).
func loadRouteOpts(scenarioName, routePath string, logger *slog.Logger) []simulator.ScenarioOption {
	// Only highway-drive and city-drive support routes.
	if scenarioName != "highway-drive" && scenarioName != "city-drive" {
		return nil
	}

	rf, err := simulator.LoadRouteForScenario(scenarioName, routePath)
	if err != nil {
		logger.Warn("route file not found, using random-walk positioning",
			slog.String("scenario", scenarioName),
			slog.Any("error", err),
		)
		return nil
	}

	logger.Info("loaded route file",
		slog.String("route", rf.Name),
		slog.Float64("distance_miles", rf.TotalDistanceMiles),
		slog.Int("waypoints", len(rf.Coordinates)),
	)

	return []simulator.ScenarioOption{
		simulator.WithRouteFile(rf),
		simulator.WithLogger(logger),
	}
}

func buildTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load client certificate if provided (needed for mTLS).
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate if provided (for server verification).
	if caPath != "" {
		caCert, err := os.ReadFile(caPath) // #nosec G304 -- operator-configured cert path
		if err != nil {
			return nil, fmt.Errorf("reading CA certificate: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate from %s", caPath)
		}
		cfg.RootCAs = caPool
	}

	return cfg, nil
}
