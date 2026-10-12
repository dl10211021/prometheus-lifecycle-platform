package services

import (
	"context"
	"github.com/promlifecycle/platform/models"
	"os"
	"strings"
	"testing"
)

func TestRecoveryPointTypedLaunchBoundaries(t *testing.T) {
	c := &models.Component{BinaryPath: "/opt/prometheus"}
	argv := []string{c.BinaryPath, "--storage.tsdb.path=/var/lib/prom", "--log.level=info"}
	hash := pointDigest([]byte(strings.Join(argv, "\x00") + "\x00"))
	good := `a(sasbttttuii) 1 "/opt/prometheus" 3 "/opt/prometheus" "--storage.tsdb.path=/var/lib/prom" "--log.level=info" false 0 0 0 0 0 0 0`
	for name, out := range map[string]string{
		"good":          good,
		"merged-token":  `a(sasbttttuii) 1 "/opt/prometheus" 2 "/opt/prometheus" "--storage.tsdb.path=/var/lib/prom --log.level=info" false 0 0 0 0 0 0 0`,
		"bad-count":     strings.Replace(good, `" 3 `, `" 2 `, 1),
		"ignore-errors": strings.Replace(good, "false", "true", 1),
		"multiple":      strings.Replace(good, ") 1 ", ") 2 ", 1),
		"bad-escape":    strings.Replace(good, "/opt/prometheus", `/opt/\qprometheus`, 1),
		"trailing":      good + " unexpected",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRecoveryPointBusLaunch(out, c, hash)
			if (err == nil) != (name == "good") {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}
func TestRecoveryPointLegacyUnitSyntax(t *testing.T) {
	c := &models.Component{BinaryPath: "/opt/prometheus"}
	argv := []string{c.BinaryPath, "--storage.tsdb.path=/var/lib/prom", "--log.level=info"}
	hash := pointDigest([]byte(strings.Join(argv, "\x00") + "\x00"))
	good := "[Service]\nExecStart=" + strings.Join(argv, " ") + "\n"
	for name, raw := range map[string]string{
		"good": good, "modifier": strings.Replace(good, "ExecStart=", "ExecStart=+", 1),
		"quoted":       strings.Replace(good, "--storage.tsdb.path=/var/lib/prom --log.level=info", `"--storage.tsdb.path=/var/lib/prom --log.level=info"`, 1),
		"continuation": strings.Replace(good, " --log.level", " \\\n --log.level", 1),
		"new-remap":    good + "MountImages=/srv/image:/var/lib/prom\n",
	} {
		t.Run(name, func(t *testing.T) {
			found, err := validateRecoveryPointUnitText(raw, c, hash)
			if name == "good" {
				if !found || err != nil {
					t.Fatal(found, err)
				}
			} else if err == nil {
				t.Fatal("unsafe legacy unit syntax accepted")
			}
		})
	}
}

func TestRecoveryPointGeneratedQuotedUnitSyntax(t *testing.T) {
	c := &models.Component{BinaryPath: "/opt/prometheus"}
	args := []string{c.BinaryPath, "--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/var/lib/prometheus", "--web.listen-address=:19090"}
	hash := pointDigest([]byte(strings.Join(args, "\x00") + "\x00"))
	good := "[Service]\nPrivateTmp=true\nExecStart=" + c.BinaryPath + " \"" + strings.Join(args[1:], "\" \"") + "\"\n"
	for name, raw := range map[string]string{
		"generated":        good,
		"mixed quoting":    strings.Replace(good, `"--storage.tsdb.path=/var/lib/prometheus"`, `--storage.tsdb.path=/var/lib/prometheus`, 1),
		"merged argument":  strings.Replace(good, `" "--storage`, ` --storage`, 1),
		"double separator": strings.Replace(good, `" "--storage`, `"  "--storage`, 1),
		"unterminated":     strings.TrimSuffix(good, "\"\n") + "\n",
		"argv mismatch":    strings.Replace(good, ":19090", ":19091", 1),
		"expansion":        strings.Replace(good, ":19090", "${PORT}", 1),
		"specifier":        strings.Replace(good, ":19090", "%i", 1),
		"escape":           strings.Replace(good, ":19090", `:\\19090`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			found, err := validateRecoveryPointUnitText(raw, c, hash)
			if name == "generated" {
				if !found || err != nil {
					t.Fatal(found, err)
				}
			} else if err == nil {
				t.Fatal("unsupported generated unit syntax accepted")
			}
		})
	}
}

func TestRecoveryPointUnitIncludeRejected(t *testing.T) {
	c := &models.Component{BinaryPath: "/opt/prometheus"}
	args := "/opt/prometheus --config.file=/opt/p.yml"
	hash := pointDigest([]byte(strings.ReplaceAll(args, " ", "\x00") + "\x00"))
	if _, err := validateRecoveryPointUnitText("[Service]\nExecStart="+args+"\n.include /etc/other-unit.conf\n", c, hash); err == nil {
		t.Fatal("uninventoried include accepted")
	}
}

func TestRecoveryPointBareServiceName(t *testing.T) {
	f := newPointFixture(t)
	c := *f.c
	c.ServiceName = strings.TrimSuffix(c.ServiceName, ".service")
	args := []string{c.BinaryPath, "--config.file=" + c.ConfigPath, "--storage.tsdb.path=" + c.DataPath}
	hash := pointDigest([]byte(strings.Join(args, "\x00") + "\x00"))
	r := pointRunFunc(func(ctx context.Context, cmd string) (string, error) {
		if strings.Contains(cmd, "recovery-point-launch") && !strings.Contains(cmd, "_2eservice") {
			return "", pointReject("bare D-Bus unit was not canonicalized")
		}
		return f.runner.Run(ctx, cmd)
	})
	if err := verifyRecoveryPointLaunch(context.Background(), r, &c, hash); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryPointLaunchAcceptsProductPrivateTmp(t *testing.T) {
	f := newPointFixture(t)
	args := []string{f.c.BinaryPath, "--config.file=" + f.c.ConfigPath, "--storage.tsdb.path=" + f.c.DataPath}
	hash := pointDigest([]byte(strings.Join(args, "\x00") + "\x00"))
	raw := "[Unit]\nDescription=Prometheus prometheus (managed by Prometheus Lifecycle Platform)\nAfter=network.target\n\n" +
		"[Service]\nType=simple\nUser=nobody\nNoNewPrivileges=true\nPrivateTmp=true\n" +
		"ExecStart=" + f.c.BinaryPath + " \"" + strings.Join(args[1:], "\" \"") + "\"\n" +
		"Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(f.runner.fragment, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	namespace := "s \"\"\ns \"\"\na(ssbt) 0\na(ssbt) 0\na(ss) 0\nb false\nb true\ns \"no\"\ns \"\"\nas 0\n"

	for _, tc := range []struct {
		name, output string
		wantErr      bool
	}{
		{name: "product PrivateTmp", output: namespace},
		{name: "malformed PrivateTmp", output: strings.Replace(namespace, "b true\n", "b maybe\n", 1), wantErr: true},
		{name: "dynamic user remains rejected", output: strings.Replace(namespace, "b false\nb true\n", "b true\nb true\n", 1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pointRunFunc(func(ctx context.Context, cmd string) (string, error) {
				if strings.Contains(cmd, "recovery-point-namespace") {
					return tc.output, nil
				}
				return f.runner.Run(ctx, cmd)
			})
			err := verifyRecoveryPointLaunch(context.Background(), r, f.c, hash)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected admission result: %v", err)
			}
		})
	}
}

func TestRecoveryPointDigitLeadingServiceName(t *testing.T) {
	f := newPointFixture(t)
	c := *f.c
	c.ServiceName = "1point.service"
	raw, err := os.ReadFile(f.runner.fragment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.root+"/"+c.ServiceName, raw, 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{c.BinaryPath, "--config.file=" + c.ConfigPath, "--storage.tsdb.path=" + c.DataPath}
	hash := pointDigest([]byte(strings.Join(args, "\x00") + "\x00"))
	r := pointRunFunc(func(ctx context.Context, cmd string) (string, error) {
		if strings.Contains(cmd, "recovery-point-launch") && !strings.Contains(cmd, "/unit/_31point_2eservice") {
			return "", pointReject("digit-leading D-Bus label was not escaped")
		}
		if strings.HasPrefix(cmd, "systemctl show --no-pager") {
			return strings.ReplaceAll(f.runner.properties(), f.c.ServiceName, c.ServiceName), nil
		}
		return f.runner.Run(ctx, cmd)
	})
	if err := verifyRecoveryPointLaunch(context.Background(), r, &c, hash); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryPointUnitCommentContinuationRejected(t *testing.T) {
	c := &models.Component{BinaryPath: "/opt/prometheus"}
	argv := "/opt/prometheus --config.file=/opt/p.yml"
	hash := pointDigest([]byte(strings.ReplaceAll(argv, " ", "\x00") + "\x00"))
	for _, middle := range []string{"# comment \\\n[Other]\n", "[broken\n"} {
		raw := "[Service]\nExecStart=" + argv + "\n" + middle + "ExecStart=\nExecStart=+" + argv + "\n"
		if _, err := validateRecoveryPointUnitText(raw, c, hash); err == nil {
			t.Fatal("unsupported physical unit syntax bypassed modifier guard")
		}
	}
}
