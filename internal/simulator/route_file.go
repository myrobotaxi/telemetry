package simulator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RouteFile represents a pre-baked route loaded from a JSON file.
// Coordinates are [lng, lat] pairs following GeoJSON convention.
type RouteFile struct {
	Name               string       `json:"name"`
	Origin             RoutePoint   `json:"origin"`
	Destination        RoutePoint   `json:"destination"`
	TotalDistanceMiles float64      `json:"totalDistanceMiles"`
	Coordinates        [][2]float64 `json:"coordinates"` // [lng, lat]
}

// RoutePoint is a named geographic point (origin or destination).
type RoutePoint struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
}

// LoadRouteFile reads and parses a route JSON file from disk.
func LoadRouteFile(path string) (*RouteFile, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-configured route path
	if err != nil {
		return nil, fmt.Errorf("simulator.LoadRouteFile(%s): %w", path, err)
	}

	var rf RouteFile
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("simulator.LoadRouteFile(%s): %w", path, err)
	}

	if err := rf.validate(); err != nil {
		return nil, fmt.Errorf("simulator.LoadRouteFile(%s): %w", path, err)
	}

	return &rf, nil
}

// LoadRouteForScenario loads a route file from the configs/routes/ directory
// using the scenario name to derive the filename. Falls back to a custom
// path if routePath is non-empty.
func LoadRouteForScenario(scenarioName, routePath string) (*RouteFile, error) {
	if routePath != "" {
		return LoadRouteFile(routePath)
	}

	// Walk up from the binary to find configs/routes/.
	candidates := []string{
		filepath.Join("configs", "routes", scenarioName+".json"),
		filepath.Join("..", "configs", "routes", scenarioName+".json"),
		filepath.Join("..", "..", "configs", "routes", scenarioName+".json"),
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return LoadRouteFile(path)
		}
	}

	return nil, fmt.Errorf("simulator.LoadRouteForScenario: no route file found for %q (tried configs/routes/%s.json)", scenarioName, scenarioName)
}

// validate checks that the route file has valid data.
func (rf *RouteFile) validate() error {
	if len(rf.Coordinates) < 2 {
		return fmt.Errorf("route must have at least 2 coordinates, got %d", len(rf.Coordinates))
	}
	if rf.TotalDistanceMiles <= 0 {
		return fmt.Errorf("totalDistanceMiles must be positive, got %f", rf.TotalDistanceMiles)
	}
	if rf.Name == "" {
		return fmt.Errorf("route name is required")
	}
	return nil
}
