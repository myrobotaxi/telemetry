---
name: Proto vendoring approach
description: How Tesla proto files are vendored and code-generated in this project, including paths and go_package rewrite
type: reference
---

Tesla proto files are vendored from `github.com/teslamotors/fleet-telemetry/protos/` into `internal/telemetry/proto/tesla/`. The `go_package` option in each proto file is rewritten from `github.com/teslamotors/fleet-telemetry/protos` to `github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla` so generated Go code belongs to our module.

**Files vendored:**
- vehicle_data.proto (main telemetry payload: Payload, Datum, Value, Field enum)
- vehicle_alert.proto (VehicleAlerts, VehicleAlert)
- vehicle_error.proto (VehicleErrors, VehicleError)
- vehicle_metric.proto (VehicleMetrics, Metric)
- vehicle_connectivity.proto (VehicleConnectivity, ConnectivityEvent)

**Generation:** `make proto` or `./scripts/generate-proto.sh`. Requires `protoc` (brew install protobuf) and `protoc-gen-go` (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest). The script adds `$(go env GOPATH)/bin` to PATH since protoc-gen-go may not be on the default PATH.

**How to apply:** When Tesla updates their proto files, re-fetch from GitHub, rewrite go_package, and run `make proto`. Generated `.pb.go` files are committed to version control.
