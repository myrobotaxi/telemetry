package auth

import (
	"errors"
	"testing"
)

func TestRole_String(t *testing.T) {
	tests := []struct {
		name string
		role Role
		want string
	}{
		{name: "owner", role: RoleOwner, want: "owner"},
		{name: "viewer", role: RoleViewer, want: "viewer"},
		{name: "empty sentinel", role: Role(""), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.role.String(); got != tt.want {
				t.Errorf("Role(%q).String() = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Role
		wantErr error
	}{
		{name: "owner", input: "owner", want: RoleOwner},
		{name: "viewer", input: "viewer", want: RoleViewer},
		{name: "empty rejected", input: "", wantErr: ErrUnknownRole},
		{name: "uppercase rejected", input: "Owner", wantErr: ErrUnknownRole},
		{name: "limited_viewer (FR-5.5 future) rejected in v1", input: "limited_viewer", wantErr: ErrUnknownRole},
		{name: "garbage", input: "admin", wantErr: ErrUnknownRole},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRole(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error wrapping %v, got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error wrapping %v, got: %v", tt.wantErr, err)
				}
				if got != Role("") {
					t.Errorf("expected zero-value Role on error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseRole(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
