package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/coder/websocket"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

const defaultDebugFieldsServer = "ws://localhost:8080"

func runFields(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("fields requires a subcommand (watch | snapshot)")
	}
	switch args[0] {
	case "watch":
		return runFieldsWatch(ctx, args[1:])
	case "snapshot":
		return runFieldsSnapshot(ctx, args[1:])
	default:
		return fmt.Errorf("unknown fields subcommand %q", args[0])
	}
}

// runFieldsWatch opens a WebSocket to the server's /api/debug/fields
// endpoint and streams raw field frames to stdout. Each line is one JSON
// frame produced by the server so the output can be piped through jq or
// parsed by later tooling.
func runFieldsWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fields watch", flag.ContinueOnError)
	vin := fs.String("vin", "", "17-character Tesla VIN to filter on (empty = all)")
	server := fs.String("server", defaultDebugFieldsServer, "telemetry server ws:// or wss:// base URL")
	tokenFlag := fs.String("token", "", "debug token (overrides DEBUG_FIELDS_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	endpoint, err := buildDebugURL(*server, *vin)
	if err != nil {
		return err
	}

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("DEBUG_FIELDS_TOKEN")
	}

	header := http.Header{}
	if token != "" {
		header.Set("X-Debug-Token", token)
	}

	conn, resp, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}
	// coder/websocket returns the upgrade response; callers must drain and
	// close its Body or bodyclose flags it. The connection itself is closed
	// separately.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer conn.CloseNow() //nolint:errcheck // nothing useful to do on close failure

	fmt.Fprintf(os.Stderr, "ops fields watch: connected to %s\n", endpoint)
	return streamFrames(ctx, conn, os.Stdout)
}

// streamFrames reads text frames from the WebSocket and writes each one
// to out followed by a newline. Returns nil when the context is cancelled
// (graceful shutdown); any other read/write error is returned.
func streamFrames(ctx context.Context, conn *websocket.Conn, out io.Writer) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		if _, err := out.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
}

// buildDebugURL converts a ws:// or http:// server base URL into the
// full debug fields endpoint URL. It accepts both protocol forms so the
// operator can reuse the same base URL as the server's --client flag.
func buildDebugURL(server, vin string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parse --server: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported --server scheme %q", u.Scheme)
	}
	u.Path = "/api/debug/fields"
	if vin != "" {
		q := u.Query()
		q.Set("vin", vin)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// vehicleSnapshot is the JSON shape printed by `ops fields snapshot`. We
// stay close to store.Vehicle but flatten the pointer fields for easier
// reading. The field order + JSON tags mirror the canonical VehicleState
// contract at docs/contracts/vehicle-state-schema.md §1.1, so this command
// doubles as a spot-check for the REST /snapshot wire shape.
type vehicleSnapshot struct {
	ID                   string   `json:"id"`
	VIN                  string   `json:"vin"`
	Name                 string   `json:"name"`
	Model                string   `json:"model"`
	Year                 int      `json:"year"`
	Color                string   `json:"color"`
	Status               string   `json:"status"`
	ChargeLevel          int      `json:"chargeLevel"`
	EstimatedRange       int      `json:"estimatedRange"`
	Speed                int      `json:"speed"`
	GearPosition         *string  `json:"gearPosition,omitempty"`
	Heading              int      `json:"heading"`
	Latitude             float64  `json:"latitude"`
	Longitude            float64  `json:"longitude"`
	LocationName         string   `json:"locationName"`
	LocationAddress      string   `json:"locationAddress"`
	InteriorTemp         int      `json:"interiorTemp"`
	ExteriorTemp         int      `json:"exteriorTemp"`
	OdometerMiles        int      `json:"odometerMiles"`
	FsdMilesSinceReset   float64  `json:"fsdMilesSinceReset"`
	DestinationName      *string  `json:"destinationName,omitempty"`
	DestinationAddress   *string  `json:"destinationAddress,omitempty"`
	DestinationLatitude  *float64 `json:"destinationLatitude,omitempty"`
	DestinationLongitude *float64 `json:"destinationLongitude,omitempty"`
	OriginLatitude       *float64 `json:"originLatitude,omitempty"`
	OriginLongitude      *float64 `json:"originLongitude,omitempty"`
	EtaMinutes           *int     `json:"etaMinutes,omitempty"`
	TripDistRemaining    *float64 `json:"tripDistanceRemaining,omitempty"`
	NavRouteCoordinates  json.RawMessage `json:"navRouteCoordinates,omitempty"`
	LastUpdated          string   `json:"lastUpdated"`
}

func runFieldsSnapshot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fields snapshot", flag.ContinueOnError)
	vin := fs.String("vin", "", "17-character Tesla VIN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("vin", *vin); err != nil {
		return err
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	repo := store.NewVehicleRepo(db.Pool(), store.NoopMetrics{})
	v, err := repo.GetByVIN(ctx, *vin)
	if err != nil {
		return fmt.Errorf("lookup vehicle: %w", err)
	}

	return writeJSON(os.Stdout, newVehicleSnapshot(v))
}

// newVehicleSnapshot flattens a store.Vehicle into the vehicleSnapshot
// JSON shape printed by `ops fields snapshot`. Shared with
// fields_snapshot_test.go so the two paths cannot drift on field add/rename.
func newVehicleSnapshot(v store.Vehicle) vehicleSnapshot {
	return vehicleSnapshot{
		ID:                   v.ID,
		VIN:                  v.VIN,
		Name:                 v.Name,
		Model:                v.Model,
		Year:                 v.Year,
		Color:                v.Color,
		Status:               string(v.Status),
		ChargeLevel:          v.ChargeLevel,
		EstimatedRange:       v.EstimatedRange,
		Speed:                v.Speed,
		GearPosition:         v.GearPosition,
		Heading:              v.Heading,
		Latitude:             v.Latitude,
		Longitude:            v.Longitude,
		LocationName:         v.LocationName,
		LocationAddress:      v.LocationAddress,
		InteriorTemp:         v.InteriorTemp,
		ExteriorTemp:         v.ExteriorTemp,
		OdometerMiles:        v.OdometerMiles,
		FsdMilesSinceReset:   v.FsdMilesSinceReset,
		DestinationName:      v.DestinationName,
		DestinationAddress:   v.DestinationAddress,
		DestinationLatitude:  v.DestinationLatitude,
		DestinationLongitude: v.DestinationLongitude,
		OriginLatitude:       v.OriginLatitude,
		OriginLongitude:      v.OriginLongitude,
		EtaMinutes:           v.EtaMinutes,
		TripDistRemaining:    v.TripDistRemaining,
		NavRouteCoordinates:  v.NavRouteCoordinates,
		LastUpdated:          v.LastUpdated.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
