// Binary ops is a developer CLI for Tesla Fleet API operations and raw
// telemetry inspection. It is the interim UX for verifying Tesla field
// behavior (MYR-25/28/29 and future issues) and will be superseded by a
// web test bench built against the same /api/debug/fields endpoint.
//
// Subcommands:
//
//	ops auth token        --user-id <id>
//	ops vehicles list     --user-id <id>
//	ops fleet-config show
//	ops fleet-config push --vin <vin> --user-id <id>
//	ops fields watch      --vin <vin>
//	ops fields snapshot   --vin <vin>
//
// The CLI reads DATABASE_URL from the environment (same as the server).
// Fleet API operations additionally require TESLA_PROXY_URL,
// FLEET_TELEMETRY_HOSTNAME/PORT, and AUTH_TESLA_ID/SECRET.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage())
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "ops: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "auth":
		return runAuth(ctx, os.Args[2:])
	case "vehicles":
		return runVehicles(ctx, os.Args[2:])
	case "fleet-config":
		return runFleetConfig(ctx, os.Args[2:])
	case "fields":
		return runFields(ctx, os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

var errUsage = errors.New("usage")

func usage() string {
	return `ops — MyRoboTaxi Tesla operations CLI

Usage:
  ops <command> [flags]

Commands:
  auth token          --user-id <id>                 Print the user's Tesla token (auto-refreshes if expired)
  vehicles list       --user-id <id>                 List vehicles owned by the user
  fleet-config show                                  Print DefaultFieldConfig as JSON
  fleet-config push   --vin <vin> --user-id <id>     Push DefaultFieldConfig to Tesla for this VIN
  fields watch        --vin <vin> [--server <url>]   Stream raw decoded fields from /api/debug/fields
  fields snapshot     --vin <vin>                    Dump the current vehicle row as JSON

Environment:
  DATABASE_URL                  Postgres connection string (required)
  TESLA_PROXY_URL               tesla-http-proxy base URL (for fleet-config push)
  FLEET_TELEMETRY_HOSTNAME      Hostname vehicles connect to after config push
  FLEET_TELEMETRY_PORT          Port vehicles connect to (default 443)
  FLEET_TELEMETRY_CA            PEM CA cert for the telemetry server
  AUTH_TESLA_ID                 Tesla OAuth client id (enables token refresh)
  AUTH_TESLA_SECRET             Tesla OAuth client secret
  DEBUG_FIELDS_TOKEN            Auth token for fields watch (when server requires it)
`
}
