package main

import "testing"

func TestBuildDebugURL(t *testing.T) {
	tests := []struct {
		name    string
		server  string
		vin     string
		want    string
		wantErr bool
	}{
		{
			name:   "ws stays ws",
			server: "ws://localhost:8080",
			vin:    "5YJ3E7EB2NF000001",
			want:   "ws://localhost:8080/api/debug/fields?vin=5YJ3E7EB2NF000001",
		},
		{
			name:   "http upgraded to ws",
			server: "http://localhost:8080",
			want:   "ws://localhost:8080/api/debug/fields",
		},
		{
			name:   "https upgraded to wss",
			server: "https://telemetry.example.com",
			vin:    "5YJ3E7EB2NF000001",
			want:   "wss://telemetry.example.com/api/debug/fields?vin=5YJ3E7EB2NF000001",
		},
		{
			name:    "unsupported scheme rejected",
			server:  "tcp://localhost",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildDebugURL(tt.server, tt.vin)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err: got %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
