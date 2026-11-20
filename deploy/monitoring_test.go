//go:build integration

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestMonitoringCollectorPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	manifest, err := Monitoring()
	if err != nil {
		t.Fatal(err)
	}
	var config, image string
	decoder := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `yaml:"image"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if doc.Kind == "ConfigMap" {
			config = doc.Data["config.yaml"]
		}
		if doc.Kind == "Deployment" {
			image = doc.Spec.Template.Spec.Containers[0].Image
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.yaml")
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return out
	}
	run("run", "--rm", "-v", path+":/etc/otel.yaml:ro", image, "validate", "--config=/etc/otel.yaml")
	// Only the test adds an HTTP receiver, allowing standard OTLP/JSON injection
	// without adding SDK dependencies to the installer.
	config = strings.Replace(config, "protocols:\n", "protocols:\n      http:\n        endpoint: 0.0.0.0:4318\n", 1)
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("substrate-aks-collector-test-%d", time.Now().UnixNano())
	var logs bytes.Buffer
	process := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name,
		"--user", "10001:10001", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"-p", "127.0.0.1::4318", "-p", "127.0.0.1::8889", "-p", "127.0.0.1::8888", "-p", "127.0.0.1::13133",
		"-v", path+":/etc/otel.yaml:ro", image, "--config=/etc/otel.yaml")
	process.Stdout, process.Stderr = &logs, &logs
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		out, err := exec.CommandContext(stopCtx, "docker", "stop", "--time=5", name).CombinedOutput()
		if err != nil {
			t.Errorf("stopping test collector: %v\n%s", err, out)
		}
		if err := process.Wait(); err != nil && !t.Failed() {
			t.Errorf("collector exited unexpectedly: %v", err)
		}
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	var ports map[string][]struct{ HostPort string }
	for {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", name).CombinedOutput()
		if err == nil && json.Unmarshal(out, &ports) == nil && len(ports["8889/tcp"]) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("collector did not start:", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	endpoint := func(port string) string { return "http://127.0.0.1:" + ports[port+"/tcp"][0].HostPort }
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		response, err := client.Get(endpoint("13133"))
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("collector health endpoint did not become ready:", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	post := func(body string) {
		t.Helper()
		response, err := client.Post(endpoint("4318")+"/v1/metrics", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || strings.Contains(string(data), "rejectedDataPoints") {
			t.Fatalf("OTLP metrics rejected: %s: %v", data, err)
		}
	}
	for _, instance := range []string{"node-a", "node-b"} {
		post(fmt.Sprintf(`{"resourceMetrics":[{"resource":{"attributes":[
			{"key":"service.name","value":{"stringValue":"atelet"}},
			{"key":"service.instance.id","value":{"stringValue":%q}},
			{"key":"ate.actor.uid","value":{"stringValue":"must-not-be-a-resource-label"}}]},
			"scopeMetrics":[{"metrics":[
			{"name":"ate.workerpool.workers","unit":"{worker}","sum":{"aggregationTemporality":2,"isMonotonic":false,"dataPoints":[{"asInt":"2","timeUnixNano":%q}]}},
			{"name":"rpc.server.call.duration","unit":"s","histogram":{"aggregationTemporality":2,"dataPoints":[{"count":"2","sum":0.15,"bucketCounts":["1","1"],"explicitBounds":[0.1],"timeUnixNano":%q}]}},
			{"name":"workqueue_queue_duration_seconds","unit":"s","exponentialHistogram":{"aggregationTemporality":2,"dataPoints":[{"count":"1","sum":1.5,"scale":0,"positive":{"offset":0,"bucketCounts":["1"]},"timeUnixNano":%q}]}}
			]}]}]}`, instance, fmt.Sprint(time.Now().UnixNano()), fmt.Sprint(time.Now().UnixNano()), fmt.Sprint(time.Now().UnixNano())))
	}
	var metrics string
	for {
		response, err := client.Get(endpoint("8889") + "/metrics")
		if err == nil {
			data, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			metrics = string(data)
			if strings.Contains(metrics, `service_instance_id="node-a"`) && strings.Contains(metrics, `service_instance_id="node-b"`) {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("collector did not expose both source identities:", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, want := range []string{"ate_workerpool_workers", "rpc_server_call_duration_seconds_bucket", "workqueue_queue_duration_seconds_bucket"} {
		if !strings.Contains(metrics, "\n"+want+"{") {
			t.Fatalf("dashboard metric %s is absent from the actual collector output:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "must-not-be-a-resource-label") {
		t.Fatal("collector promoted unreviewed resource attributes into metric labels")
	}
	sourceFound := false
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "ate_workerpool_workers{") && strings.Contains(line, `service_name="atelet"`) &&
			strings.Contains(line, `service_instance_id="node-a"`) {
			sourceFound = true
			break
		}
	}
	if !sourceFound {
		t.Fatal("collector application metrics did not preserve service/process identity labels")
	}
	response, err := client.Get(endpoint("8888") + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(data), "\notelcol_receiver_accepted_metric_points{") {
		t.Fatalf("collector self-monitoring is missing: %s: %v", data, err)
	}
}
