package models_test

import (
	"errors"
	"testing"

	"github.com/promlifecycle/platform/models"
)

func TestPrometheusParameterVersionSyntax(t *testing.T) {
	tests := []struct {
		version string
		valid   bool
	}{
		{"2.0.0", true},
		{"3.1.0", true},
		{"v3.1.0+build.7", true},
		{"2.55.0+linux-amd64.007", true},
		{" 3.1.0 ", true},
		{"", false},
		{"3", false},
		{"3.1", false},
		{"3.1.0.0", false},
		{"V3.1.0", false},
		{"03.1.0", false},
		{"3.01.0", false},
		{"3.1.00", false},
		{"3.-1.0", false},
		{"3.1.0+", false},
		{"3.1.0+build.", false},
		{"3.1.0+.build", false},
		{"3.1.0+build..7", false},
		{"3.1.0+build_7", false},
		{"3.1.0+build+7", false},
		{"3.1.0-rc.1", false},
		{"v3.1.0-rc.1+build.7", false},
		{"999999999999999999999999999999.1.0", false},
		{"3.999999999999999999999999999999.0", false},
		{"3.1.999999999999999999999999999999", false},
		{"0.1.0", false},
		{"1.0.0", false},
		{"4.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			err := models.ValidatePrometheusArgumentVersion(tt.version, nil)
			if tt.valid {
				if err != nil {
					t.Fatalf("valid stable version %q rejected: %v", tt.version, err)
				}
			} else if !errors.Is(err, models.ErrPrometheusParameterVersion) {
				t.Fatalf("unsupported version %q: error = %v, want parameter admission rejection", tt.version, err)
			}
		})
	}
}
