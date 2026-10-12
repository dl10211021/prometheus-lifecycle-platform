package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPrometheusParameterMigrationPreservesTokensAndScope(t *testing.T) {
	for _, tc := range []struct {
		name, source, target, status string
		args, want                   []string
		required, current            bool
	}{
		{"required equals", "2.55.0", "3.1.0", "available", []string{"--storage.tsdb.retention=045d", "--opaque=secret"}, []string{"--storage.tsdb.retention.time=045d", "--opaque=secret"}, true, false},
		{"split", "2.55.0", "3.1.0", "available", []string{"--storage.tsdb.retention", "1d2h3ms"}, []string{"--storage.tsdb.retention.time", "1d2h3ms"}, true, false},
		{"optional normalization", "2.55.0", "2.55.0", "available", []string{"--storage.tsdb.retention=0"}, []string{"--storage.tsdb.retention.time=0"}, false, true},
		{"introduced", "2.6.1", "2.7.0", "available", []string{"--storage.tsdb.retention=30d"}, []string{"--storage.tsdb.retention.time=30d"}, false, true},
		{"old target", "2.5.0", "2.6.1", "not_needed", []string{"--storage.tsdb.retention=30d"}, []string{"--storage.tsdb.retention=30d"}, false, true},
		{"no change", "2.55.0", "3.1.0", "not_needed", []string{"--storage.tsdb.retention.time=30d"}, []string{"--storage.tsdb.retention.time=30d"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]string(nil), tc.args...)
			before := append([]string(nil), input...)
			p := PreviewPrometheusArguments(tc.source, tc.target, input)
			if p.Status != tc.status || !p.PreviewOnly || !p.SourceCompatible || p.CurrentCompatible != tc.current || !p.ConvertedCompatible || !reflect.DeepEqual(p.Arguments, tc.want) {
				t.Fatalf("%+v candidate=%v", p, p.Arguments)
			}
			if tc.status == "available" && (len(p.Changes) != 1 || p.Changes[0].Required != tc.required) {
				t.Fatalf("changes=%+v", p.Changes)
			}
			if !reflect.DeepEqual(before, input) {
				t.Fatal("mutated input")
			}
			p.Arguments[0] = "mutated"
			if !reflect.DeepEqual(before, input) {
				t.Fatal("candidate aliases input")
			}
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "opaque") || strings.Contains(string(raw), "\"arguments\"") {
				t.Fatalf("raw arguments leaked: %s", raw)
			}
		})
	}
}

func TestPrometheusParameterMigrationBlocksAmbiguityWithoutGuessing(t *testing.T) {
	for _, tc := range []struct {
		name, source, target, code string
		args                       []string
	}{
		{"duplicate", "2.55.0", "3.1.0", "duplicate_retention", []string{"--storage.tsdb.retention=30d", "--storage.tsdb.retention=30d"}},
		{"aliases", "2.55.0", "3.1.0", "conflicting_retention", []string{"--storage.tsdb.retention=30d", "--storage.tsdb.retention.time=40d"}},
		{"missing", "2.55.0", "3.1.0", "invalid_retention_duration", []string{"--storage.tsdb.retention", "--web.enable-lifecycle"}},
		{"empty", "2.55.0", "3.1.0", "invalid_retention_duration", []string{"--storage.tsdb.retention="}},
		{"invalid secret", "2.55.0", "3.1.0", "invalid_retention_duration", []string{"--storage.tsdb.retention=private-secret"}},
		{"end of flags", "2.55.0", "3.1.0", "ambiguous_arguments", []string{"--", "--storage.tsdb.retention=30d"}},
		{"unknown source", "latest", "3.1.0", "unsupported_version", []string{"--storage.tsdb.retention=30d"}},
		{"prerelease target", "2.55.0", "3.1.0-rc.0", "unsupported_version", []string{"--storage.tsdb.retention=30d"}},
		{"already invalid source", "3.1.0", "3.1.0", "source_incompatible", []string{"--storage.tsdb.retention=30d"}},
		{"do not drop wal", "2.55.0", "2.7.0", "target_incompatible", []string{"--storage.tsdb.retention=30d", "--storage.tsdb.wal-compression"}},
		{"old source compound", "2.7.0", "3.1.0", "unverified_duration_semantics", []string{"--storage.tsdb.retention=1d2h"}},
		{"old target compound", "2.55.0", "2.7.0", "unverified_duration_semantics", []string{"--storage.tsdb.retention=1d2h"}},
		{"old source zero", "2.7.0", "3.1.0", "unverified_duration_semantics", []string{"--storage.tsdb.retention=0s"}},
		{"old target zero", "2.55.0", "2.7.0", "unverified_duration_semantics", []string{"--storage.tsdb.retention=0s"}},

		{"oversize", "2.55.0", "3.1.0", "arguments_too_large", []string{strings.Repeat("x", 1024*1024+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]string(nil), tc.args...)
			p := PreviewPrometheusArguments(tc.source, tc.target, tc.args)
			found := false
			for _, b := range p.Blockers {
				if b.Code == tc.code {
					found = true
				}
			}
			if p.Status != "blocked" || p.Arguments != nil || len(p.Changes) != 0 || !found || p.ConvertedCompatible {
				t.Fatalf("%+v", p)
			}
			if !reflect.DeepEqual(tc.args, before) {
				t.Fatal("mutated blocked input")
			}
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), "private-secret") {
				t.Fatal("invalid duration leaked")
			}
		})
	}
}

func TestPrometheusParameterMigrationDurationBoundaries(t *testing.T) {
	for _, value := range []string{"0", "0s", "045d", "1y2w3d4h5m6s7ms", "1ms", "292y", "9223372036854ms"} {
		if p := PreviewPrometheusArguments("2.55.0", "3.1.0", []string{"--storage.tsdb.retention=" + value}); p.Status != "available" || p.Changes[0].Value != value {
			t.Errorf("valid %q: %+v", value, p)
		}
	}
	for _, value := range []string{"", "00", "-1d", "+1d", "1.5h", "1h1d", "1m1m", "1ms1s", "1D", "1us", "1ns", "1d ", " 1d", "1d\n", "293y", "9223372036855ms", "18446744073709551616ms", "1h0", "0x10d"} {
		if p := PreviewPrometheusArguments("2.55.0", "3.1.0", []string{"--storage.tsdb.retention=" + value}); p.Status != "blocked" {
			t.Errorf("invalid %q: %+v", value, p)
		}
	}
}
