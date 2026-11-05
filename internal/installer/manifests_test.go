package installer

import (
	"fmt"
	"os"
	"path/filepath"
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

// testPlugin fills the vendored copy of the pinned plugin's components;
// the contract tests keep that copy identical to the pin.
func testPlugin(t *testing.T, c Config, i Infrastructure) []deploy.External {
	t.Helper()
	plugin, err := pluginComponents("testdata/plugin", c, i)
	if err != nil {
		t.Fatal(err)
	}
	return plugin
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
	wantArgs := []any{mode, "--socket=/run/objectstore-plugin/" + mode + ".sock"}
	if node {
		wantArgs = append(wantArgs, "--root=/var/lib/ate")
	}
	wantArgs = append(wantArgs, "--account-url="+i.AccountURL, "--containers="+i.SnapshotsContainer, "--prefix=substrate")
	if node {
		wantArgs = append(wantArgs, "--asset-staging-dir=/var/lib/objectstore-plugin/asset-staging", "--asset-staging-capacity=8Gi")
	}
	if !reflect.DeepEqual(plugin["args"], wantArgs) {
		t.Fatalf("sidecar arguments are not the Azure plugin's: %v", plugin["args"])
	}
	if _, err := named(plugin["env"], "ATE_STORAGE_BACKEND"); err == nil {
		t.Fatal("in-tree storage backend setting remains")
	}
	if !node {
		if _, err := named(plugin["env"], "AZURE_STORAGE_ASSET_CONTAINERS"); err == nil {
			t.Fatal("control sidecar serves sandbox assets")
		}
		return
	}
	assets, err := named(plugin["env"], "AZURE_STORAGE_ASSET_CONTAINERS")
	if err != nil || assets["value"] != i.AssetsContainer {
		t.Fatalf("node sidecar does not serve assets from the asset container: %v: %v", assets, err)
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
  labels:
    app: %s
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
`, name, kind, name, name, name))
}

func TestAzureWorkloadAdaptation(t *testing.T) {
	for _, node := range []bool{false, true} {
		t.Run(fmt.Sprintf("node=%t", node), func(t *testing.T) {
			c, i := testConfig(t), testInfrastructure()
			got, err := adaptWorkload(workloadFixture(node), c, testPlugin(t, c, i), node)
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
				if strings.Contains(s, "/gke/") || !strings.Contains(s, "--grpc-server-cred-bundle=") {
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
		{"        - name: ATE_STORAGE_BACKEND", "        - name: AZURE_CLIENT_ID\n          value: upstream\n        - name: ATE_STORAGE_BACKEND"},
		{"      - name: host-dev\n        hostPath:", "      - name: azure-identity-token\n        emptyDir: {}\n      - name: host-dev\n        hostPath:"},
	} {
		data := strings.ReplaceAll(string(workloadFixture(true)), tc.old, tc.new)
		c := testConfig(t)
		if _, err := adaptWorkload([]byte(data), c, testPlugin(t, c, testInfrastructure()), true); err == nil {
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
			_, err := deploy.Render([]byte(strings.Replace(source, tc.old, tc.replacement, 1)), deploy.Values{Component: "actor-images"})
			if err == nil {
				t.Fatal("declarative positional patch accepted a changed upstream target")
			}
		})
	}
}

func TestAzureWorkloadsRenderFromEmbeddedAssets(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	plugin := testPlugin(t, c, i)
	t.Chdir(t.TempDir())
	t.Setenv("PATH", "")
	for _, node := range []bool{false, true} {
		got, err := adaptWorkload(workloadFixture(node), c, plugin, node)
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

func TestPluginComponentValuesCannotInjectYAML(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	i.AccountURL = "https://example.invalid/#fragment\n---\nkind: Secret"
	if _, err := pluginComponents("testdata/plugin", c, i); err == nil {
		t.Fatal("placed a value that is not a plain YAML scalar into the plugin component")
	}
}

func TestPluginComponentsRequireReviewedPlaceholders(t *testing.T) {
	c, i := testConfig(t), testInfrastructure()
	for _, tc := range []struct{ name, file, old, new string }{
		{"missing image", "config/kustomization.yaml", "example.invalid/replace-with-azure-blob-objectstore-plugin:latest", "registry.example/fixed:latest"},
		{"new placeholder", "config/kustomization.yaml", "--prefix=substrate", "--prefix=REPLACE_WITH_PREFIX"},
		{"missing asset containers", "config/assets/kustomization.yaml", "REPLACE_WITH_ASSET_CONTAINERS", "assets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, rel := range []string{pluginComponent, pluginAssetsComponent} {
				data, err := os.ReadFile(filepath.Join("testdata/plugin", rel))
				if err != nil {
					t.Fatal(err)
				}
				if rel == tc.file {
					data = []byte(strings.Replace(string(data), tc.old, tc.new, 1))
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, rel), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pluginComponents(dir, c, i); err == nil {
				t.Fatal("accepted a plugin component outside the reviewed placeholder contract")
			}
		})
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/plugin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config/patch.yaml"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginComponents(dir, c, i); err == nil {
		t.Fatal("accepted a plugin component with files outside the reviewed layout")
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
