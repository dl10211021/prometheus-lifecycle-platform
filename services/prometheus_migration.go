package services

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/promlifecycle/platform/core/deployer"
	"github.com/promlifecycle/platform/models"
)

// ErrPrometheusMigrationAdmission is a before-mutation rejection, never a
// reason to compensate an untouched runtime.
var ErrPrometheusMigrationAdmission = errors.New("Prometheus migration admission rejected")

// PrometheusMigrationDecision describes version-path admission only. It does
// not certify configuration, scrape protocols, PromQL or storage semantics.
type PrometheusMigrationDecision struct {
	Source     string   `json:"source"`
	Target     string   `json:"target"`
	Direction  string   `json:"direction"`
	Decision   string   `json:"decision"`
	Reason     string   `json:"reason"`
	Scope      string   `json:"scope"`
	Rule       string   `json:"rule"`
	Provenance []string `json:"provenance"`
}

// Use the dependency rule's strict syntax without changing its established
// compatibility behavior. Overflow is unknown, never a truncated version.
func stablePrometheusVersion(raw string) ([3]int, bool) {
	var v [3]int
	match := stableDependencyVersion.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return v, false
	}
	for i := range v {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, v[0] == 2 || v[0] == 3
}

func canonicalPrometheusVersion(raw string) (string, bool) {
	v, ok := stablePrometheusVersion(raw)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), true
}

// prometheusArtifactVersion preserves build metadata used to select and bind
// an exact package. Numerical migration decisions use canonicalPrometheusVersion.
func prometheusArtifactVersion(raw string) (string, bool) {
	if _, ok := stablePrometheusVersion(raw); !ok {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimSpace(raw), "v"), true
}

func samePrometheusArtifactVersion(a, b string) bool {
	av, aok := prometheusArtifactVersion(a)
	bv, bok := prometheusArtifactVersion(b)
	return aok && bok && av == bv
}

func samePrometheusVersion(a, b string) bool {
	av, aok := stablePrometheusVersion(a)
	bv, bok := stablePrometheusVersion(b)
	return aok && bok && av == bv
}

func managedPrometheus(c *models.Component) bool {
	return c != nil && c.Type == models.ComponentPrometheus && c.DeployType != "target"
}

func prometheusMigrationDecision(c *models.Component, target string) PrometheusMigrationDecision {
	e := PrometheusMigrationDecision{Source: c.Version, Target: target, Direction: "unknown", Decision: "unknown", Scope: "managed_prometheus_version_path_only", Rule: "prometheus-staged-upgrade-v1", Provenance: []string{
		"https://prometheus.io/blog/2024/11/14/prometheus-3-0/",
		"https://prometheus.io/docs/prometheus/latest/stability/",
		"product_policy: all numerical downgrades require paired historical recovery; not a universal upstream prohibition",
	}}
	if !managedPrometheus(c) {
		e.Decision = "not_applicable"
		e.Reason = "metadata-only target or non-Prometheus; existing dependency checks still apply"
		return e
	}
	source, sok := stablePrometheusVersion(c.Version)
	dest, tok := stablePrometheusVersion(target)
	if !sok || !tok {
		e.Reason = "use known stable Prometheus 2.x/3.x source and target versions; unknown, malformed, prerelease, overflow and future majors fail closed"
		return e
	}
	e.Source, _ = canonicalPrometheusVersion(c.Version)
	e.Target, _ = canonicalPrometheusVersion(target)
	cmp := 0
	for i := range source {
		if source[i] < dest[i] {
			cmp = 1
			break
		}
		if source[i] > dest[i] {
			cmp = -1
			break
		}
	}
	switch {
	case cmp < 0:
		e.Direction = "downgrade"
		e.Decision = "requires_restore"
		e.Reason = "direct numerical downgrade requires_restore: use rollback of the historical change plan with its matching stopped-application full config/data backup"
	case cmp == 0:
		e.Direction = "equal"
		e.Decision = "no_upgrade"
		e.Reason = "normalized versions are equal; there is no numerical upgrade"
	case source[0] == 2 && source[1] < 55 && dest[0] == 3:
		e.Direction = "upgrade"
		e.Decision = "requires_intermediate"
		e.Reason = "register and execute an intermediate Prometheus 2.55.x upgrade, then create a fresh plan for 3.x"
	default:
		e.Direction = "upgrade"
		e.Decision = "eligible"
		e.Reason = "version path is eligible for remaining dependency, health, package and paired recovery checks; configuration/scrape/PromQL migration is not certified"
	}
	return e
}

func checkPrometheusMigration(c *models.Component, target string) (PrometheusMigrationDecision, error) {
	e := prometheusMigrationDecision(c, target)
	if e.Decision != "eligible" && e.Decision != "not_applicable" {
		return e, fmt.Errorf("%w: %s", ErrPrometheusMigrationAdmission, e.Reason)
	}
	if managedPrometheus(c) {
		if err := deployer.ValidatePrometheusParameterVersion(c, target); err != nil {
			return e, errors.Join(ErrPrometheusMigrationAdmission, err)
		}
		if err := prometheusFullRecoveryScope(c); err != nil {
			return e, fmt.Errorf("%w: %v", ErrPrometheusMigrationAdmission, err)
		}
	}
	return e, nil
}

func admitPrometheusPlan(plan *models.ChangePlan, c *models.Component, target string) error {
	if _, err := checkPrometheusMigration(c, target); err != nil {
		return err
	}
	if !managedPrometheus(c) {
		return nil
	}
	before, err := prometheusUpgradeSnapshot(plan, c)
	if err != nil {
		return errors.Join(ErrPrometheusMigrationAdmission, err)
	}
	if !samePrometheusArtifactVersion(before.Version, c.Version) || before.Image != c.Image {
		return fmt.Errorf("%w: historical before version/image no longer matches the source; create a fresh plan", ErrPrometheusMigrationAdmission)
	}
	return nil
}
