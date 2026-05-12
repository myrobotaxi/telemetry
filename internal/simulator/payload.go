package simulator

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// BuildPayload creates a Tesla protobuf Payload from a scenario state.
// The payload uses the same encoding the receiver expects: string values
// for numeric fields, location values for GPS, and shift state values for
// gear.
func BuildPayload(vin string, state ScenarioState) *tpb.Payload {
	return &tpb.Payload{
		Vin:       vin,
		CreatedAt: timestamppb.New(time.Now()),
		Data:      buildData(state),
	}
}

// MarshalPayload builds and marshals a protobuf payload to bytes.
func MarshalPayload(vin string, state ScenarioState) ([]byte, error) {
	payload := BuildPayload(vin, state)

	raw, err := proto.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("simulator.MarshalPayload: %w", err)
	}
	return raw, nil
}

// buildData converts scenario state fields into Tesla Datum entries.
func buildData(state ScenarioState) []*tpb.Datum {
	data := []*tpb.Datum{
		stringDatum(tpb.Field_VehicleSpeed, state.Speed),
		locationDatum(state.Latitude, state.Longitude),
		stringDatum(tpb.Field_GpsHeading, state.Heading),
		gearDatum(state.GearPosition),
		stringDatum(tpb.Field_Soc, float64(state.ChargeLevel)),
		stringDatum(tpb.Field_EstBatteryRange, float64(state.EstimatedRange)),
		stringDatum(tpb.Field_InsideTemp, float64(state.InteriorTemp)),
		stringDatum(tpb.Field_OutsideTemp, float64(state.ExteriorTemp)),
		stringDatum(tpb.Field_Odometer, state.OdometerMiles),
	}

	// Include MinutesToArrival only when a nav route is active (ETA > 0).
	if state.ETA > 0 {
		data = append(data, stringDatum(tpb.Field_MinutesToArrival, state.ETA))
	}

	// Navigation fields — emitted when a route is active.
	if state.Nav.TripDistanceRemain > 0 {
		data = append(data, stringDatum(tpb.Field_MilesToArrival, state.Nav.TripDistanceRemain))
	}
	if state.Nav.RouteLine != "" {
		data = append(data, textDatum(tpb.Field_RouteLine, state.Nav.RouteLine))
	}
	if state.Nav.DestinationName != "" {
		data = append(data, textDatum(tpb.Field_DestinationName, state.Nav.DestinationName))
	}
	if state.Nav.DestinationLat != 0 || state.Nav.DestinationLng != 0 {
		data = append(data, destLocationDatum(state.Nav.DestinationLat, state.Nav.DestinationLng))
	}

	return data
}

// stringDatum creates a Datum with a string-encoded numeric value.
// Tesla frequently sends numeric fields as string_value.
func stringDatum(field tpb.Field, val float64) *tpb.Datum {
	return &tpb.Datum{
		Key: field,
		Value: &tpb.Value{
			Value: &tpb.Value_StringValue{
				StringValue: fmt.Sprintf("%.2f", val),
			},
		},
	}
}

// locationDatum creates a Datum with a LocationValue for the GPS field.
func locationDatum(lat, lng float64) *tpb.Datum {
	return &tpb.Datum{
		Key: tpb.Field_Location,
		Value: &tpb.Value{
			Value: &tpb.Value_LocationValue{
				LocationValue: &tpb.LocationValue{
					Latitude:  lat,
					Longitude: lng,
				},
			},
		},
	}
}

// textDatum creates a Datum with a raw string value (non-numeric).
// Used for fields like RouteLine and DestinationName.
func textDatum(field tpb.Field, val string) *tpb.Datum {
	return &tpb.Datum{
		Key: field,
		Value: &tpb.Value{
			Value: &tpb.Value_StringValue{
				StringValue: val,
			},
		},
	}
}

// destLocationDatum creates a Datum with a LocationValue for the
// DestinationLocation field.
func destLocationDatum(lat, lng float64) *tpb.Datum {
	return &tpb.Datum{
		Key: tpb.Field_DestinationLocation,
		Value: &tpb.Value{
			Value: &tpb.Value_LocationValue{
				LocationValue: &tpb.LocationValue{
					Latitude:  lat,
					Longitude: lng,
				},
			},
		},
	}
}

// gearDatum creates a Datum with a ShiftState enum value.
func gearDatum(gear string) *tpb.Datum {
	var ss tpb.ShiftState
	switch gear {
	case "P":
		ss = tpb.ShiftState_ShiftStateP
	case "D":
		ss = tpb.ShiftState_ShiftStateD
	case "R":
		ss = tpb.ShiftState_ShiftStateR
	case "N":
		ss = tpb.ShiftState_ShiftStateN
	default:
		ss = tpb.ShiftState_ShiftStateInvalid
	}

	return &tpb.Datum{
		Key: tpb.Field_Gear,
		Value: &tpb.Value{
			Value: &tpb.Value_ShiftStateValue{
				ShiftStateValue: ss,
			},
		},
	}
}
