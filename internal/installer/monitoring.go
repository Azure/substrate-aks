package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/deploy"
)

type MonitoringInfrastructure struct {
	Location        string `json:"location"`
	WorkspaceID     string `json:"workspaceId"`
	QueryEndpoint   string `json:"queryEndpoint"`
	GrafanaID       string `json:"grafanaId"`
	GrafanaEndpoint string `json:"grafanaEndpoint"`
}

const (
	prometheusDataSource = "substrate-aks-prometheus"
	platformDataSource   = "substrate-aks-azure-monitor"
)

func validMonitoringResourceID(c Config, id, kind string) bool {
	prefix := "/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/" + kind + "/"
	remainder, ok := strings.CutPrefix(strings.ToLower(id), strings.ToLower(prefix))
	return ok && namePattern.MatchString(remainder)
}

func validateMonitoringInfrastructure(c Config, m *MonitoringInfrastructure) error {
	if !c.Monitoring.Enabled {
		if m != nil {
			return errors.New("deployment has monitoring enabled; restore monitoring.enabled in the original configuration")
		}
		return nil
	}
	if m == nil {
		return errors.New("monitoring outputs are missing; run provision with monitoring enabled before installation")
	}
	validEndpoint := func(value, suffix string) bool {
		u, err := url.Parse(value)
		return err == nil && u.Scheme == "https" && strings.HasSuffix(u.Hostname(), suffix) &&
			u.Port() == "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
	}
	if m.Location != c.MonitoringLocation() ||
		!validMonitoringResourceID(c, m.WorkspaceID, "Microsoft.Monitor/accounts") ||
		!validMonitoringResourceID(c, m.GrafanaID, "Microsoft.Dashboard/grafana") ||
		!validEndpoint(m.QueryEndpoint, ".prometheus.monitor.azure.com") ||
		!validEndpoint(m.GrafanaEndpoint, ".grafana.azure.com") {
		return errors.New("monitoring deployment outputs are missing or inconsistent with this installation")
	}
	return nil
}

func (a App) installMetricsCollector(ctx context.Context) error {
	if !a.Config.Monitoring.Enabled {
		return nil
	}
	a.progress("Waiting for Azure managed Prometheus monitor APIs (up to 5 minutes)")
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, condition := range []string{"create", "condition=Established"} {
		if _, err := a.kubectl(readyCtx, nil, "wait", "--for="+condition,
			"crd/podmonitors.azmonitoring.coreos.com", "--timeout=300s"); err != nil {
			return fmt.Errorf("managed Prometheus PodMonitor API is not ready: %w", err)
		}
	}
	data, err := deploy.Monitoring()
	if err != nil {
		return err
	}
	a.progress("Deploying the metrics-only OTLP collector and managed scrape targets")
	if _, err := a.kubectl(ctx, data, "apply", "-f", "-"); err != nil {
		return err
	}
	_, err = a.kubectl(ctx, nil, "rollout", "status", "deployment/substrate-aks-otel", "--namespace=ate-system", "--timeout=300s")
	return err
}

func (a App) grafanaRequest(ctx context.Context, m *MonitoringInfrastructure, method, path string, body any) ([]byte, error) {
	var input []byte
	var err error
	if body != nil {
		input, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	args := []string{"az", "rest", "--method", method,
		"--url", strings.TrimSuffix(m.GrafanaEndpoint, "/") + path,
		"--resource", "https://dashboard.azure.com",
		"--subscription", a.Config.Azure.SubscriptionID, "--only-show-errors", "--output", "json"}
	if body != nil {
		args = append(args, "--headers", "Content-Type=application/json", "--body", "@/dev/stdin")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return a.Runner.Run(requestCtx, Command{Args: args, Input: input})
}

func monitoringWait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return nil
	}
}

func transientGrafanaError(err error) bool {
	for _, status := range []string{"Forbidden", "Bad Gateway", "Service Unavailable", "Too Many Requests", "Gateway Timeout"} {
		if strings.Contains(err.Error(), status) {
			return true
		}
	}
	return false
}

func (a App) configureGrafana(ctx context.Context, i Infrastructure) error {
	if i.Monitoring == nil {
		return nil
	}
	m := i.Monitoring
	a.progress("Configuring Grafana using the signed-in identity (allowing up to 5 minutes for its Grafana Admin grant)")
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var data []byte
	for {
		var err error
		data, err = a.grafanaRequest(readyCtx, m, "GET", "/api/datasources", nil)
		if err == nil {
			break
		}
		if !transientGrafanaError(err) {
			return fmt.Errorf("reading Grafana data sources; sign in as azure.assetPublisherObjectId: %w", err)
		}
		a.progress("Grafana access is not ready: %v", err)
		if waitErr := monitoringWait(readyCtx); waitErr != nil {
			return fmt.Errorf("Grafana access did not become ready; sign in as azure.assetPublisherObjectId: %w", errors.Join(err, waitErr))
		}
	}
	var sources []struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &sources); err != nil || sources == nil {
		return fmt.Errorf("Grafana did not return a data-source list: %s", data)
	}
	for _, source := range []object{
		{"uid": prometheusDataSource, "name": "Substrate AKS Prometheus", "type": "prometheus", "access": "proxy",
			"url": strings.TrimSuffix(m.QueryEndpoint, "/"), "jsonData": object{
				"httpMethod": "POST", "azureAuth": true, "azureCredentials": object{"authType": "msi"},
			}},
		{"uid": platformDataSource, "name": "Substrate AKS Azure Monitor", "type": "grafana-azure-monitor-datasource", "access": "proxy",
			"jsonData": object{"azureAuthType": "msi", "cloudName": "azuremonitor", "subscriptionId": a.Config.Azure.SubscriptionID}},
	} {
		method, path := "POST", "/api/datasources"
		for _, existing := range sources {
			if existing.UID == source["uid"] {
				if existing.Type != source["type"] {
					return fmt.Errorf("Grafana data source %s has an unexpected type; refusing to overwrite it", existing.UID)
				}
				method, path = "PUT", "/api/datasources/uid/"+existing.UID
			}
		}
		if _, err := a.grafanaRequest(ctx, m, method, path, source); err != nil {
			return fmt.Errorf("reconciling Grafana data source %s: %w", source["uid"], err)
		}
	}
	for _, dashboard := range a.monitoringDashboards(i) {
		if _, err := a.grafanaRequest(ctx, m, "POST", "/api/dashboards/db", object{
			"dashboard": dashboard, "overwrite": true,
			"message": "Reconcile substrate-aks managed monitoring dashboard",
		}); err != nil {
			return fmt.Errorf("reconciling Grafana dashboard %s: %w", dashboard["uid"], err)
		}
	}
	return a.verifyMetrics(ctx, m)
}

func (a App) verifyMetrics(ctx context.Context, m *MonitoringInfrastructure) error {
	a.progress("Waiting up to 10 minutes for managed Prometheus to report the collector and Substrate metrics")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	query := `count by (service_name) (workqueue_depth{service_name="atecontroller"}) and on() (min(up{namespace="ate-system",pod=~"substrate-aks-otel-.*"}) == 1) and on() (min(up{namespace="ate-system",pod=~"atenet-router-.*"}) == 1)`
	request := object{
		"from": "now-5m", "to": "now",
		"queries": []object{{
			"refId": "A", "datasource": object{"uid": prometheusDataSource, "type": "prometheus"},
			"expr": query, "instant": true, "range": false, "intervalMs": 30000, "maxDataPoints": 1000,
		}},
	}
	for {
		data, err := a.grafanaRequest(ctx, m, "POST", "/api/ds/query", request)
		if err != nil {
			if !transientGrafanaError(err) {
				return fmt.Errorf("querying managed Prometheus through Grafana: %w", err)
			}
			a.progress("Managed Prometheus query access is not ready: %v", err)
			if waitErr := monitoringWait(ctx); waitErr != nil {
				return fmt.Errorf("managed Prometheus query access did not become ready: %w", errors.Join(err, waitErr))
			}
			continue
		}
		var response struct {
			Results map[string]struct {
				Status int    `json:"status"`
				Error  string `json:"error"`
				Frames []struct {
					Schema struct {
						Fields []struct {
							Type   string            `json:"type"`
							Labels map[string]string `json:"labels"`
						} `json:"fields"`
					} `json:"schema"`
					Data struct {
						Values [][]*float64 `json:"values"`
					} `json:"data"`
				} `json:"frames"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return fmt.Errorf("managed Prometheus query returned invalid JSON: %w", err)
		}
		result, ok := response.Results["A"]
		if !ok || result.Status != 200 || result.Error != "" {
			return fmt.Errorf("managed Prometheus query failed or returned an invalid response: %s", data)
		}
		found := map[string]bool{}
		for _, frame := range result.Frames {
			if len(frame.Schema.Fields) != len(frame.Data.Values) {
				return fmt.Errorf("managed Prometheus query returned inconsistent frame fields and values: %s", data)
			}
			for n, field := range frame.Schema.Fields {
				if field.Type != "number" || field.Labels["service_name"] == "" {
					continue
				}
				for _, value := range frame.Data.Values[n] {
					if value != nil && *value > 0 {
						found[field.Labels["service_name"]] = true
					}
				}
			}
		}
		if found["atecontroller"] {
			a.progress("Managed Prometheus reports controller metrics and healthy collector/Envoy scrapes; request metrics appear with traffic. Grafana: %s/d/substrate-aks-overview", strings.TrimSuffix(m.GrafanaEndpoint, "/"))
			return nil
		}
		if err := monitoringWait(ctx); err != nil {
			return fmt.Errorf("Substrate metrics are incomplete (services observed: %v); inspect substrate-aks-otel and ama-metrics pods, DCR association and Grafana managed identity permissions: %w", found, err)
		}
	}
}

func (a App) MonitoringDashboardURL(ctx context.Context) (string, error) {
	i, err := a.infrastructure(ctx)
	if err != nil {
		return "", err
	}
	if i.Monitoring == nil {
		return "", errors.New("monitoring is disabled")
	}
	return strings.TrimSuffix(i.Monitoring.GrafanaEndpoint, "/") + "/d/substrate-aks-overview", nil
}
