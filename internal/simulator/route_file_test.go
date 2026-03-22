package simulator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRouteFile_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-route.json")
	data := `{
		"name": "Test Route",
		"origin": {"name": "Start", "lat": 32.7767, "lng": -96.7970},
		"destination": {"name": "End", "lat": 33.1972, "lng": -96.6153},
		"totalDistanceMiles": 10.5,
		"coordinates": [[-96.7970, 32.7767], [-96.7960, 32.7780], [-96.6153, 33.1972]]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rf, err := LoadRouteFile(path)
	if err != nil {
		t.Fatalf("LoadRouteFile: %v", err)
	}

	if rf.Name != "Test Route" {
		t.Errorf("Name = %q, want %q", rf.Name, "Test Route")
	}
	if rf.TotalDistanceMiles != 10.5 {
		t.Errorf("TotalDistanceMiles = %f, want 10.5", rf.TotalDistanceMiles)
	}
	if len(rf.Coordinates) != 3 {
		t.Errorf("Coordinates length = %d, want 3", len(rf.Coordinates))
	}
	if rf.Origin.Name != "Start" {
		t.Errorf("Origin.Name = %q, want %q", rf.Origin.Name, "Start")
	}
	if rf.Destination.Name != "End" {
		t.Errorf("Destination.Name = %q, want %q", rf.Destination.Name, "End")
	}
}

func TestLoadRouteFile_NotFound(t *testing.T) {
	_, err := LoadRouteFile("/nonexistent/route.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadRouteFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadRouteFile(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadRouteFile_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{
			name: "too few coordinates",
			json: `{"name":"X","totalDistanceMiles":1,"coordinates":[[-96.797,32.776]]}`,
		},
		{
			name: "zero distance",
			json: `{"name":"X","totalDistanceMiles":0,"coordinates":[[-96.797,32.776],[-96.6,33.1]]}`,
		},
		{
			name: "empty name",
			json: `{"name":"","totalDistanceMiles":1,"coordinates":[[-96.797,32.776],[-96.6,33.1]]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "route.json")
			if err := os.WriteFile(path, []byte(tt.json), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := LoadRouteFile(path)
			if err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestLoadRouteForScenario_CustomPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	data := `{
		"name": "Custom",
		"origin": {"name": "A", "lat": 32.0, "lng": -96.0},
		"destination": {"name": "B", "lat": 33.0, "lng": -96.0},
		"totalDistanceMiles": 5.0,
		"coordinates": [[-96.0, 32.0], [-96.0, 33.0]]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rf, err := LoadRouteForScenario("anything", path)
	if err != nil {
		t.Fatalf("LoadRouteForScenario: %v", err)
	}
	if rf.Name != "Custom" {
		t.Errorf("Name = %q, want %q", rf.Name, "Custom")
	}
}

func TestLoadRouteForScenario_NotFound(t *testing.T) {
	_, err := LoadRouteForScenario("nonexistent-scenario", "")
	if err == nil {
		t.Fatal("expected error when no route file exists")
	}
}
