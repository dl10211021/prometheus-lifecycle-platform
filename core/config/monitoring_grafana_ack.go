package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/promlifecycle/platform/models"
)

var errDashboardAwaitingApply = errors.New("Grafana has not acknowledged the desired dashboard snapshot yet")

// File writes are not Grafana acknowledgements: its provisioning watcher may
// reject a dashboard or may not have scanned it yet. Keep such work retriable.
func (ac *AutoConfig) verifyGrafanaDashboards(component *models.Component, dashboards []models.Dashboard, files map[string][]byte) error {
	if len(dashboards) == 0 {
		return nil
	}
	endpoint, err := ac.GetGrafanaEndpoint(component.ClusterID)
	if err != nil {
		return err
	}
	credentials := map[string]string{}
	if component.CustomEnv != "" {
		if err := json.Unmarshal([]byte(component.CustomEnv), &credentials); err != nil {
			return fmt.Errorf("invalid Grafana credentials configuration")
		}
	}
	user := credentials["GF_SECURITY_ADMIN_USER"]
	if user == "" {
		user = "admin"
	}
	password := credentials["GF_SECURITY_ADMIN_PASSWORD"]
	if password == "" {
		return fmt.Errorf("Grafana publication cannot be verified without configured administrator credentials")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, dashboard := range dashboards {
		uid := dashboard.UID
		if uid == "" {
			uid = fmt.Sprintf("promlifecycle-%d", dashboard.ID)
		}
		req, err := http.NewRequest(http.MethodGet, endpoint+"/api/dashboards/uid/"+url.PathEscape(uid), nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(user, password)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("verify Grafana dashboard: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		wanted, enabled := files[fmt.Sprintf("dashboard-%d.json", dashboard.ID)]
		if !enabled && resp.StatusCode == http.StatusNotFound {
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("Grafana dashboard verification authorization failed")
		}
		if resp.StatusCode != http.StatusOK || !enabled {
			return errDashboardAwaitingApply
		}
		var actual struct {
			Dashboard map[string]interface{} `json:"dashboard"`
		}
		if err := json.Unmarshal(data, &actual); err != nil {
			return fmt.Errorf("invalid Grafana dashboard acknowledgement")
		}
		var desired map[string]interface{}
		if err := json.Unmarshal(wanted, &desired); err != nil {
			return err
		}
		delete(desired, "version")
		if !dashboardContains(actual.Dashboard, desired) {
			return errDashboardAwaitingApply
		}
	}
	return nil
}

func dashboardContains(actual, desired interface{}) bool {
	wanted, object := desired.(map[string]interface{})
	if !object {
		return reflect.DeepEqual(actual, desired)
	}
	got, ok := actual.(map[string]interface{})
	if !ok {
		return false
	}
	for key, value := range wanted {
		if !dashboardContains(got[key], value) {
			return false
		}
	}
	return true
}
