package deployer

import (
	"context"
	"github.com/promlifecycle/platform/models"
	"reflect"
	"strings"
	"testing"
)

func TestPrometheusParameterVersionRuntimeRejectsBeforeAccess(t *testing.T) {
	for _, operation := range []string{"deploy", "upgrade", "apply-unit"} {
		t.Run(operation, func(t *testing.T) {
			c := models.Component{Type: models.ComponentPrometheus, Name: "parameter-version-reject", DeployType: "binary", Version: "3.1.0", CustomArgs: "--storage.tsdb.retention=45d", Host: models.Host{IP: "127.0.0.2", Port: 1, ConnectionType: "ssh"}}
			before := c
			d := NewBinaryDeployer(nil, t.TempDir())
			var err error
			switch operation {
			case "deploy":
				err = d.Deploy(context.Background(), &c)
			case "upgrade":
				err = d.Upgrade(context.Background(), &c, "3.2.0")
			case "apply-unit":
				err = d.ApplyUnit(context.Background(), &c)
			}
			if err == nil || !strings.Contains(err.Error(), "Prometheus 参数版本") {
				t.Errorf("expected parameter gate before host access, got %v", err)
			}
			if !reflect.DeepEqual(c, before) {
				t.Fatal("rejection mutated component")
			}
		})
	}
}
