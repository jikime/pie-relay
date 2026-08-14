package main

import "testing"

func TestValidateSecret(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		required bool
		wantErr  bool
	}{
		{name: "required missing", required: true, wantErr: true},
		{name: "optional missing", required: false},
		{name: "short", value: "too-short", required: true, wantErr: true},
		{name: "exactly 32 bytes", value: "0123456789abcdef0123456789abcdef", required: true},
		{name: "long", value: "0123456789abcdef0123456789abcdef-extra", required: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSecret("TEST_SECRET", tc.value, tc.required)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSecret() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
