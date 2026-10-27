package installer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/deploy"
	"go.yaml.in/yaml/v3"
)

func manifestObject(t *testing.T, data []byte, kind, name string) object {
	t.Helper()
	var found object
	if err := inspectYAML(data, func(doc object) error {
		meta, err := child(doc, "metadata")
		if err != nil {
			return err
		}
		if doc["kind"] == kind && meta["name"] == name {
			if found != nil {
				t.Fatalf("duplicate %s/%s", kind, name)
			}
			found = doc
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatalf("missing %s/%s", kind, name)
	}
	return found
}

func assertAzureWorkload(t *testing.T, data []byte, c Config, i Infrastructure, node bool) {
	t.Helper()
	name, kind, mode, client := "ate-api-server", "Deployment", "control", i.ControlClientID
	if node {
		name, kind, mode, client = "atelet", "DaemonSet", "node", i.NodeClientID
	}
	pod, err := nested(manifestObject(t, data, kind, name), "spec", "template", "spec")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := named(pod["initContainers"], "objectstore-plugin")
	if err != nil {
		t.Fatal(err)
	}
	if pod["serviceAccountName"] != name || plugin["restartPolicy"] != "Always" || plugin["image"] != c.Substrate.Images.Plugin {
		t.Fatal("storage sidecar lost its native lifecycle, image, or federated service account")
	}
	for key, value := range map[string]string{
		"AZURE_CLIENT_ID": client, "AZURE_TENANT_ID": i.TenantID,
		"AZURE_FEDERATED_TOKEN_FILE": "/var/run/secrets/azure/tokens/azure-identity-token",
	} {
		env, err := named(plugin["env"], key)
		if err != nil {
			t.Fatal(err)
		}
		if env["value"] != value || env["valueFrom"] != nil {
			t.Fatalf("wrong %s identity source: %v", key, env)
		}
	}
	volume, err := named(pod["volumes"], "azure-identity-token")
	if err != nil {
		t.Fatal(err)
	}
	wantProjection := object{"sources": []any{object{"serviceAccountToken": object{
		"audience": "api://AzureADTokenExchange", "expirationSeconds": 3600, "path": "azure-identity-token",
	}}}}
	if !reflect.DeepEqual(volume["projected"], wantProjection) {
		t.Fatalf("wrong federated token projection: %v", volume)
	}
	mount, err := named(plugin["volumeMounts"], "azure-identity-token")
	if err != nil || mount["mountPath"] != "/var/run/secrets/azure/tokens" || mount["readOnly"] != true {
		t.Fatalf("sidecar cannot read its projected token: %v: %v", mount, err)
	}
	probe, err := nested(plugin, "startupProbe", "exec")
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []any{"/ko-app/azure-blob-objectstore-plugin", "healthcheck", "--socket=/run/objectstore-plugin/" + mode + ".sock"}
	if !reflect.DeepEqual(probe["command"], wantCommand) {
		t.Fatalf("sidecar healthcheck does not use the Azure binary and matching socket: %v", probe)
	}
	if !node {
		return
	}
	for volumeName, wantSource := range map[string]object{
		"image-credential-provider-bin": {"path": "/var/lib/kubelet/credential-provider", "type": "Directory"},
		"azure-cloud-config":            {"path": "/etc/kubernetes/azure.json", "type": "File"},
	} {
		volume, err := named(pod["volumes"], volumeName)
		if err != nil || !reflect.DeepEqual(volume["hostPath"], wantSource) {
			t.Fatalf("wrong node credential provider source: %v: %v", volume, err)
		}
	}
	volume, err = named(pod["volumes"], "image-credential-provider-config")
	if err != nil || !reflect.DeepEqual(volume["configMap"], object{"name": "substrate-aks-acr-credentials"}) || volume["hostPath"] != nil {
		t.Fatalf("credential provider must use the generated ConfigMap, not GKE host config: %v: %v", volume, err)
	}
	atelet, err := named(pod["containers"], "atelet")
	if err != nil {
		t.Fatal(err)
	}
	mount, err = named(atelet["volumeMounts"], "image-credential-provider-config")
	if err != nil || mount["subPath"] != "config.yaml" || mount["readOnly"] != true {
		t.Fatalf("credential provider ConfigMap must be mounted as a single read-only file: %v: %v", mount, err)
	}
	config, err := child(manifestObject(t, data, "ConfigMap", "substrate-aks-acr-credentials"), "data")
	if err != nil {
		t.Fatal(err)
	}
	var provider struct {
		Providers []struct {
			Name        string
			MatchImages []string `yaml:"matchImages"`
			Args        []string
		}
	}
	raw, ok := config["config.yaml"].(string)
	if !ok || yaml.Unmarshal([]byte(raw), &provider) != nil || len(provider.Providers) != 1 {
		t.Fatalf("invalid credential provider configuration: %v", config)
	}
	p := provider.Providers[0]
	if p.Name != "acr-credential-provider" || !reflect.DeepEqual(p.MatchImages, []string{"*.azurecr.io"}) || !reflect.DeepEqual(p.Args, []string{"/etc/kubernetes/azure.json"}) {
		t.Fatalf("ACR provider lost its registry match or node identity configuration: %+v", p)
	}
}

func workloadFixture(node bool) []byte {
	kind, name := "Deployment", "ate-api-server"
	if node {
		kind, name = "DaemonSet", "atelet"
	}
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: ate-system
---
apiVersion: apps/v1
kind: %s
metadata:
  name: %s
  namespace: ate-system
spec:
  template:
    spec:
      serviceAccountName: %s
      initContainers:
      - name: objectstore-plugin
        image: ko://example/objectstore-plugin
        restartPolicy: Always
        env:
        - name: ATE_STORAGE_BACKEND
          value: gcs
        volumeMounts:
        - name: objectstore-plugin
          mountPath: /run/objectstore-plugin
      containers:
      - name: atelet
        image: ko://example/atelet
        args:
        - --image-credential-provider-config=/run/image-credential-provider/config.yaml
        - --image-credential-provider-bin-dir=/run/image-credential-provider/bin
        - --grpc-server-cred-bundle=/run/identity.pem
        volumeMounts:
        - name: image-credential-provider-bin
          mountPath: /run/image-credential-provider/bin
        - name: image-credential-provider-config
          mountPath: /run/image-credential-provider/config.yaml
        - name: host-dev
          mountPath: /host/dev
      volumes:
      - name: objectstore-plugin
        emptyDir: {}
      - name: host-dev
        hostPath:
          path: /dev
      - name: image-credential-provider-config
        hostPath:
          path: /gke/config
      - name: image-credential-provider-bin
        hostPath:
          path: /gke/bin
`, name, kind, name, name))
}

func TestAzureWorkloadAdaptation(t *testing.T) {
	for _, node := range []bool{false, true} {
		t.Run(fmt.Sprintf("node=%t", node), func(t *testing.T) {
			c, i := testConfig(t), testInfrastructure()
			got, err := adaptWorkload(workloadFixture(node), c, i, node)
			if err != nil {
				t.Fatal(err)
			}
			assertAzureWorkload(t, got, c, i, node)
			s := string(got)
			for _, want := range []string{c.Substrate.Images.Plugin, "api://AzureADTokenExchange", "AZURE_FEDERATED_TOKEN_FILE", "restartPolicy: Always", "mountPath: /run/objectstore-plugin"} {
				if !strings.Contains(s, want) {
					t.Fatalf("missing %s:\n%s", want, s)
				}
			}
			if strings.Contains(s, "ATE_STORAGE_BACKEND") {
				t.Fatal("GCS backend settings remain")
			}
			client := i.ControlClientID
			if node {
				client = i.NodeClientID
				if strings.Contains(s, "/gke/") || !strings.Contains(s, "--asset-containers=assets") || !strings.Contains(s, "--grpc-server-cred-bundle=") {
					t.Fatalf("atelet provider isolation, assets or mTLS configuration incorrect:\n%s", s)
				}
				for _, want := range []string{"acr-credential-provider", "/var/lib/kubelet/credential-provider", "/etc/kubernetes/azure.json", "subPath: config.yaml", "--image-credential-provider-bin-dir="} {
					if !strings.Contains(s, want) {
						t.Fatalf("missing private ACR pull configuration %s", want)
					}
				}
			}
			if !strings.Contains(s, client) {
				t.Fatal("wrong federated identity")
			}
			repeated, err := adaptWorkload(got, c, i, node)
			if err != nil || string(repeated) != s {
				t.Fatalf("adaptation is not idempotent: %v", err)
			}
		})
	}
}

func TestAzureAdaptationRejectsChangedUpstream(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{"name: objectstore-plugin", "name: storage-plugin"},
		{"serviceAccountName: atelet", "serviceAccountName: different"},
		{"restartPolicy: Always", "restartPolicy: Never"},
		{"kind: DaemonSet", "kind: Deployment"},
		{"namespace: ate-system", "namespace: elsewhere"},
		{"name: atelet\n        image:", "name: renamed\n        image:"},
		{"--image-credential-provider-config=/run/image-credential-provider/config.yaml", "--unrelated-flag=true"},
		{"--grpc-server-cred-bundle=", "--image-credential-provider-extra="},
		{"mountPath: /run/image-credential-provider/config.yaml", "mountPath: /changed/config"},
	} {
		data := strings.ReplaceAll(string(workloadFixture(true)), tc.old, tc.new)
		if _, err := adaptWorkload([]byte(data), testConfig(t), testInfrastructure(), true); err == nil {
			t.Fatalf("silently accepted upstream change %s", tc.new)
		}
	}
}

func TestDeclarativeNodePatchRejectsArgumentDrift(t *testing.T) {
	source := string(workloadFixture(true))
	for _, tc := range []struct{ name, old, replacement string }{
		{"renamed container", "name: atelet\n        image:", "name: other\n        image:"},
		{"changed first flag", "--image-credential-provider-config=/run/image-credential-provider/config.yaml", "--grpc-server-cred-bundle=/run/identity.pem"},
		{"changed second flag", "--image-credential-provider-bin-dir=/run/image-credential-provider/bin", "--unrelated=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Bypass adaptWorkload to exercise the declarative guard itself.
			_, err := deploy.Render([]byte(strings.Replace(source, tc.old, tc.replacement, 1)), deploy.Values{
				Component: "storage", Name: "atelet", Kind: "DaemonSet", Mode: "node",
				PluginImage: testConfig(t).Substrate.Images.Plugin,
			})
			if err == nil {
				t.Fatal("declarative positional patch accepted a changed upstream target")
			}
		})
	}
}

func TestAzureWorkloadsRenderFromEmbeddedAssets(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	t.Chdir(t.TempDir())
	t.Setenv("PATH", "")
	for _, node := range []bool{false, true} {
		got, err := adaptWorkload(workloadFixture(node), c, i, node)
		if err != nil {
			t.Fatal(err)
		}
		assertAzureWorkload(t, got, c, i, node)
	}
}

func TestBundledPostgresUsesPersistentAzureDisk(t *testing.T) {
	data := []byte(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: ate-system
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: postgres
        image: postgres:18
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
`)
	got, err := adaptPostgres(data)
	if err != nil {
		t.Fatal(err)
	}

	var doc object
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"storageClassName: managed-csi", "storage: 32Gi", "cpu: 250m", "replicas: 1", "ReadWriteOnce"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %s:\n%s", want, got)
		}
	}
}

func TestAzurePatchesReplaceStaleIdentityValues(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	data := strings.Replace(string(workloadFixture(true)), "        - name: ATE_STORAGE_BACKEND", `        - name: UNRELATED
          value: retained
        - name: AZURE_CLIENT_ID
          valueFrom:
            secretKeyRef:
              name: stale-identity
              key: client
        - name: ATE_STORAGE_BACKEND`, 1)
	got, err := adaptWorkload([]byte(data), c, i, true)
	if err != nil {
		t.Fatal(err)
	}
	assertAzureWorkload(t, got, c, i, true)
	if strings.Contains(string(got), "stale-identity") || !strings.Contains(string(got), "retained") {
		t.Fatalf("owned environment values were not replaced while preserving unrelated settings:\n%s", got)
	}
}

func TestAzureRuntimeValuesCannotInjectYAML(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	i.AccountURL = "https://example.invalid/#fragment\n---\nkind: Secret"
	got, err := adaptWorkload(workloadFixture(false), c, i, false)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := nested(manifestObject(t, got, "Deployment", "ate-api-server"), "spec", "template", "spec")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := named(pod["initContainers"], "objectstore-plugin")
	if err != nil {
		t.Fatal(err)
	}
	args, ok := plugin["args"].([]any)
	if !ok || len(args) != 4 || args[2] != "--account-url="+i.AccountURL {
		t.Fatalf("runtime value changed YAML structure instead of remaining a scalar: %v", plugin)
	}
}

func TestAzureTelemetryReplacesGKEConfiguration(t *testing.T) {
	source := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: ate-otel-config
  namespace: ate-system
data:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://gke-managed-otel:4317
  UNREVIEWED_EXPORTER: must-not-survive
`)
	got, err := deploy.Render(source, deploy.Values{Component: "telemetry"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := child(manifestObject(t, got, "ConfigMap", "ate-otel-config"), "data")
	if err != nil {
		t.Fatal(err)
	}
	want := object{
		"OTEL_SDK_DISABLED": "true", "OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none",
		"OTEL_LOGS_EXPORTER": "none", "ATE_WORKER_OTEL_LOGS_EXPORTER": "none",
		"ATE_WORKER_OTEL_TRACES_EXPORTER": "none", "ATE_WORKER_OTEL_METRICS_EXPORTER": "none",
	}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("telemetry retained cloud-specific exporters or did not disable worker telemetry: %v", data)
	}
	if _, err := deploy.Render([]byte(strings.Replace(string(source), "name: ate-otel-config", "name: changed", 1)), deploy.Values{Component: "telemetry"}); err == nil {
		t.Fatal("accepted renamed upstream telemetry configuration")
	}
}

func TestAzureBaseMonitoringPatchIsGuarded(t *testing.T) {
	const source = `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../ate-api-server.yaml
- ../ate-idp-server.yaml
- ../ate-controller.yaml
- ../atelet.yaml
- ../atenet-router.yaml
- ../atenet-router-monitoring.yaml
- ../ate-otel-config.yaml
`
	got, err := deploy.AdaptBase([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	var doc object
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	want := []any{"../ate-api-server.yaml", "../ate-idp-server.yaml", "../ate-controller.yaml", "../atelet.yaml", "../atenet-router.yaml", "../ate-otel-config.yaml"}
	if !reflect.DeepEqual(doc["resources"], want) {
		t.Fatalf("base resource adaptation changed more than the GKE monitoring reference: %v", doc)
	}
	for _, changed := range []string{
		strings.Replace(source, "../atenet-router-monitoring.yaml", "../changed.yaml", 1),
		"",
	} {
		if _, err := deploy.AdaptBase([]byte(changed)); err == nil {
			t.Fatal("accepted changed or missing GKE monitoring reference")
		}
	}
}

func TestMicroVMDemoTargetsDedicatedPool(t *testing.T) {
	data := []byte(`apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: counter-microvm
  namespace: ate-demo-counter-microvm
spec:
  template:
    nodeSelector:
      existing: retained
    resources:
      requests:
        memory: 1Gi
`)
	got, err := adaptDemo(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"substrate-aks/runtime: microvm", "existing: retained", "memory: 1Gi"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("incorrect demo scheduling: %s", got)
		}
	}

}

func TestMicroVMDemoActorTemplateUsesAzureSnapshots(t *testing.T) {
	const template = `metadata:
  atespace: ate-demo-counter-microvm
  name: counter-microvm
containers:
- name: counter
  image: ko://go.ate.dev/substrate/demos/counter
snapshotConfig:
  preferredFidelity: SNAPSHOT_FIDELITY_MEMORY
  storageLocation: gs://${BUCKET_NAME}/ate-demo-counter-microvm/
volumes:
- name: data
  durableDir: {}
`
	for _, tc := range []struct {
		name    string
		source  string
		wantErr bool
	}{
		{name: "Azure snapshots", source: template},
		{name: "changed location", source: strings.Replace(template, "gs://", "s3://", 1), wantErr: true},
		{name: "changed template", source: strings.Replace(template, "name: counter-microvm", "name: other", 1), wantErr: true},
		{name: "empty template", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := adaptDemoActorTemplate([]byte(tc.source))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted changed upstream contract:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"storageLocation: azblob://snapshots/ate-demo-counter-microvm/",
				"preferredFidelity: SNAPSHOT_FIDELITY_MEMORY",
				"image: ko://go.ate.dev/substrate/demos/counter",
				"durableDir: {}",
			} {
				if !strings.Contains(string(got), want) {
					t.Fatalf("adapted template lost %q:\n%s", want, got)
				}
			}
			if strings.Contains(string(got), "gs://") || strings.Contains(string(got), "${BUCKET_NAME}") {
				t.Fatalf("adapted template retained GCS configuration:\n%s", got)
			}
		})
	}
}
