package utils

import (
	"strings"
	"testing"
)

func TestValidVersionBuildMetadata(t *testing.T) {
	for _, version := range []string{
		"3.13.4+promlifecycle.1", "v2.55.0+linux-amd64.007", "0.0.0+0",
		"1.2.3-rc.1+build.007", "1.2.3-alpha-1+SHA.Abc123", "1.2.3+-",
		"1.2.3", "v2.53.0", "1.0.0-rc.1", "2025_legacy", "1.2",
	} {
		t.Run(version, func(t *testing.T) {
			if !ValidVersion(version) {
				t.Fatalf("valid version rejected: %q", version)
			}
		})
	}
	for _, version := range []string{
		"1.2.3+", "1.2.3++build", "1.2.3+build+more", "1.2.3+.build", "1.2.3+build.",
		"1.2.3+build..1", "1.2.3+build_1", "1.2.3+build/../escape", "1.2.3+build\\escape",
		"1.2.3+build 1", "1.2.3+build\n", "1.2.3+build\x00", "1.2.3+build;id", "1.2.3+$(id)",
		"1.2.3+构建", "1.2.3+%2fescape", "1.2+build", "01.2.3+build", "1.02.3+build",
		"1.2.03+build", "1.2.3-01+build", "1.2.3-rc..1+build", "1.2.3-+build", "2025_legacy+build",
		"1.2.3+" + strings.Repeat("a", 123),
	} {
		t.Run(version, func(t *testing.T) {
			if ValidVersion(version) {
				t.Fatalf("unsafe or malformed metadata accepted: %q", version)
			}
		})
	}
}
