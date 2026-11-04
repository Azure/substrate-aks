package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a App) Doctor(ctx context.Context) error {
	var failures []error
	for _, check := range []struct {
		name string
		args []string
		fix  string
	}{
		{"Azure CLI", []string{"az", "version"}, "Install Azure CLI: https://aka.ms/azure-cli"},
		{"Git", []string{"git", "--version"}, "Install Git: https://git-scm.com/downloads"},
		{"AKS kubelogin (Azure CLI authentication)", []string{"kubelogin", "--version"}, "Run az aks install-cli, then make sure kubelogin is on PATH."},
		{"Azure Bicep", []string{"az", "bicep", "version"}, "Run az bicep install."},
		{"Go toolchain", []string{"go", "version"}, "Install the Go version required by go.mod: https://go.dev/dl/"},
		{"kubectl compatibility with AKS 1.37", []string{"kubectl", "version", "--client", "--output=json"}, "Install kubectl 1.36-1.38 and make sure it is on PATH."},
	} {
		if check.args[0] == "kubelogin" && a.Config.Installation.ID != "" && a.Config.AKS.Operator == nil {
			continue
		}
		a.progress("Checking %s", check.name)
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		data, err := a.Runner.Run(probeCtx, Command{Args: check.args})
		cancel()
		if err == nil && check.args[0] == "kubectl" {
			err = checkKubectlVersion(data)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w\nFix: %s", check.name, err, check.fix))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if len(failures) == 0 && a.ContainerEngine != "" {
		if _, _, err := a.imageBuildEngine(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (a App) RuntimePrerequisites(ctx context.Context) error {
	if dir := a.Config.Substrate.Runtime.AssetsDir; dir != "" {
		a.progress("Checking supplied amd64 runtime asset files (pinned checksums are verified during preparation)")
		for _, name := range []string{"cloud-hypervisor", "virtiofsd", "vmlinux", "rootfs.img"} {
			path := filepath.Join(dir, name)
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("checking supplied runtime asset %s: %w", path, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("runtime asset %s must be a nonempty regular file, not a symlink or device", path)
			}
		}
		return nil
	}
	if a.Config.Substrate.Images != (ImagesConfig{}) {
		return errors.New("prebuilt images require substrate.runtime.assetsDir with the four matching amd64 runtime assets")
	}
	for _, args := range [][]string{
		{"bash", "--version"}, {"curl", "--version"}, {"tar", "--version"},
		{"zstd", "--version"}, {"readlink", "-f", "."}, {"sha256sum"},
	} {
		a.progress("Checking runtime asset assembly prerequisite %s", args[0])
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := a.Runner.Run(probeCtx, Command{Args: args})
		cancel()
		if err != nil {
			return fmt.Errorf("runtime asset assembly requires %s; install the tool or supply substrate.runtime.assetsDir: %w", args[0], err)
		}
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "substrate-capability-probe", Mode: 0600}); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	a.progress("Checking tar can read zstd-compressed archives (GNU or BSD tar)")
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	compressed, err := a.Runner.Run(probeCtx, Command{Args: []string{"zstd", "--compress", "--stdout"}, Input: archive.Bytes(), BinaryOutput: true})
	if err != nil {
		return fmt.Errorf("checking zstd archive support: %w", err)
	}
	if _, err := a.Runner.Run(probeCtx, Command{Args: []string{"tar", "--zstd", "-tf", "-"}, Input: compressed}); err != nil {
		return fmt.Errorf("tar must support --zstd archive reading; install a compatible tar or supply substrate.runtime.assetsDir: %w", err)
	}
	return nil
}

func checkKubectlVersion(data []byte) error {
	var version struct {
		ClientVersion struct {
			Major string `json:"major"`
			Minor string `json:"minor"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return fmt.Errorf("reading kubectl client version: %w", err)
	}
	minor, err := strconv.Atoi(strings.TrimSuffix(version.ClientVersion.Minor, "+"))
	if err != nil || version.ClientVersion.Major != "1" || minor < 36 || minor > 38 {
		return errors.New("AKS 1.37 requires a supported kubectl client within one minor version (1.36-1.38)")
	}
	return nil
}

func (a App) Check(ctx context.Context) error {
	if _, err := a.infrastructure(ctx); err != nil {
		return err
	}
	if err := a.credentials(ctx); err != nil {
		return err
	}
	if a.Config.AKS.Operator != nil {
		if err := a.waitOperatorAccess(ctx); err != nil {
			return err
		}
	}
	return a.checkCluster(ctx)
}

func (a App) waitOperatorAccess(ctx context.Context) error {
	a.progress("Waiting up to 5 minutes for the signed-in operator's cluster access; Azure RBAC grants can take time to propagate")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	review := []byte(`{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","spec":{"resourceAttributes":{"group":"apiextensions.k8s.io","resource":"customresourcedefinitions","verb":"create"}}}`)
	for {
		probeCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		data, err := a.kubectl(probeCtx, review, "--request-timeout=15s", "create", "-f", "-", "--output=json")
		stop()
		if err != nil {
			return fmt.Errorf("checking Entra operator access; verify az login, the configured operator, tenant and API allowlist: %w", err)
		}
		var result struct {
			Status struct {
				Allowed         *bool  `json:"allowed"`
				Reason          string `json:"reason"`
				EvaluationError string `json:"evaluationError"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("reading operator authorization review: %w", err)
		}
		if result.Status.Allowed == nil {
			return errors.New("operator authorization review did not return status.allowed")
		}
		if *result.Status.Allowed {
			return nil
		}
		if result.Status.EvaluationError != "" {
			return fmt.Errorf("operator authorization evaluation failed: %s", result.Status.EvaluationError)
		}
		a.progress("Operator access is not ready (%s); waiting for Azure RBAC propagation", result.Status.Reason)
		select {
		case <-ctx.Done():
			return fmt.Errorf("operator access was denied (%s); verify az login matches aks.operator or belongs to its group and check the cluster-scoped RBAC grant: %w", result.Status.Reason, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func (a App) checkCluster(ctx context.Context) error {
	a.progress("Checking kubectl compatibility and AKS version")
	client, err := a.kubectl(ctx, nil, "version", "--client", "--output=json")
	if err != nil {
		return err
	}
	if err := checkKubectlVersion(client); err != nil {
		return err
	}
	data, err := a.kubectl(ctx, nil, "get", "--raw", "/version")
	if err != nil {
		return err
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return err
	}
	if !strings.HasPrefix(version.GitVersion, "v1.37.") {
		return fmt.Errorf("expected AKS 1.37, got %q", version.GitVersion)
	}
	a.progress("Checking required Kubernetes certificate APIs")
	data, err = a.kubectl(ctx, nil, "api-resources", "--api-group=certificates.k8s.io", "--output=name")
	if err != nil {
		return err
	}
	for _, resource := range []string{"podcertificaterequests.certificates.k8s.io", "clustertrustbundles.certificates.k8s.io"} {
		found := false
		for _, line := range strings.Fields(string(data)) {
			found = found || line == resource
		}
		if !found {
			return fmt.Errorf("AKS does not serve required API %s; a Kubernetes version alone is insufficient", resource)
		}
	}
	a.progress("Waiting for cluster nodes to be ready (up to 5 minutes)")
	if _, err := a.kubectl(ctx, nil, "wait", "node", "--all", "--for=condition=Ready", "--timeout=300s"); err != nil {
		return err
	}
	a.progress("Checking node architecture, version and microVM pool size")
	data, err = a.kubectl(ctx, nil, "get", "nodes", "--output=json")
	if err != nil {
		return err
	}
	var nodes struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				NodeInfo struct {
					KubeletVersion  string `json:"kubeletVersion"`
					Architecture    string `json:"architecture"`
					OperatingSystem string `json:"operatingSystem"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &nodes); err != nil {
		return err
	}
	workers := 0
	for _, node := range nodes.Items {
		if node.Status.NodeInfo.Architecture != "amd64" || node.Status.NodeInfo.OperatingSystem != "linux" {
			return errors.New("this iteration requires Linux amd64 nodes and matching images/runtime assets")
		}
		if !strings.HasPrefix(node.Status.NodeInfo.KubeletVersion, "v1.37.") {
			return fmt.Errorf("node kubelet %q is not 1.37", node.Status.NodeInfo.KubeletVersion)
		}
		if node.Metadata.Labels["substrate-aks/runtime"] == "microvm" {
			workers++
		}
	}
	if workers != a.Config.AKS.WorkerPool.Count {
		return fmt.Errorf("expected %d microVM nodes, found %d", a.Config.AKS.WorkerPool.Count, workers)
	}
	return nil
}

const kvmProbe = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: substrate-aks-kvm-probe
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: substrate-aks-kvm-probe
  template:
    metadata:
      labels:
        app: substrate-aks-kvm-probe
    spec:
      automountServiceAccountToken: false
      nodeSelector:
        substrate-aks/runtime: microvm
      tolerations:
      - key: ate.dev/sandboxClass
        operator: Equal
        value: microvm
        effect: NoSchedule
      containers:
      - name: probe
        image: python:3.13-alpine@sha256:bf12ecc0bea3f171d55145dd02c78b5eeb09f30ee2ac9cc2b4c9e4e8f5e30741
        command: [python3, -c]
        args:
        - |
          import fcntl, json, os, time
          assert os.access("/host/credential-provider/acr-credential-provider", os.X_OK), "AKS ACR credential provider missing"
          with open("/host/azure.json") as config:
              assert json.load(config).get("useManagedIdentityExtension"), "AKS node must use managed identity"
          fd = os.open("/dev/kvm", os.O_RDWR)
          assert fcntl.ioctl(fd, 0xAE00, 0) == 12, "KVM_GET_API_VERSION failed"
          vm = fcntl.ioctl(fd, 0xAE01, 0)
          os.close(vm)
          os.close(fd)
          open("/tmp/kvm-ready", "w").close()
          time.sleep(3600)
        securityContext:
          privileged: true
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            memory: 64Mi
        readinessProbe:
          exec:
            command: [test, -f, /tmp/kvm-ready]
          periodSeconds: 2
        volumeMounts:
        - name: kvm
          mountPath: /dev/kvm
        - name: acr-provider
          mountPath: /host/credential-provider
          readOnly: true
        - name: azure-cloud-config
          mountPath: /host/azure.json
          readOnly: true
      volumes:
      - name: kvm
        hostPath:
          path: /dev/kvm
          type: CharDevice
      - name: acr-provider
        hostPath:
          path: /var/lib/kubelet/credential-provider
          type: Directory
      - name: azure-cloud-config
        hostPath:
          path: /etc/kubernetes/azure.json
          type: File
`

func (a App) checkKVM(ctx context.Context) (err error) {
	a.progress("Testing microVM virtualization and private-image prerequisites on runtime nodes")
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_, cleanupErr := a.kubectl(cleanupCtx, nil, "delete", "daemonset", "substrate-aks-kvm-probe",
			"--namespace=kube-system", "--ignore-not-found", "--wait=true", "--timeout=45s")
		err = errors.Join(err, cleanupErr)
	}()
	if _, err := a.kubectl(ctx, []byte(kvmProbe), "apply", "-f", "-"); err != nil {
		return err
	}
	_, err = a.kubectl(ctx, nil, "rollout", "status", "daemonset/substrate-aks-kvm-probe", "--namespace=kube-system", "--timeout=180s")
	if err != nil {
		_, diagnosticErr := a.kubectl(ctx, nil, "describe", "pods", "--namespace=kube-system", "--selector=app=substrate-aks-kvm-probe")
		_, logsErr := a.kubectl(ctx, nil, "logs", "--namespace=kube-system", "--selector=app=substrate-aks-kvm-probe", "--all-containers=true", "--tail=40")
		return errors.Join(fmt.Errorf("microVM node preflight failed; check KVM, AKS ACR credential provider and managed identity: %w", err), diagnosticErr, logsErr)
	}
	return nil
}

func (a App) guardInstall(ctx context.Context, fp, operation string) error {
	a.progress("Checking installation ownership and retry compatibility")
	ns, err := a.kubectl(ctx, nil, "get", "namespace", "ate-system", "--ignore-not-found", "--output=json")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(ns)) == "" {
		if operation != "ate-system" {
			return errors.New("install Substrate before deploying components, running the demo or setting up the client")
		}
		return nil
	}
	data, err := a.kubectl(ctx, nil, "get", "configmap", "substrate-aks-install", "--namespace=ate-system", "--ignore-not-found", "--output=json")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		var record struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.Data["fingerprint"] != fp || record.Data["installationId"] != a.Config.Installation.ID {
			return errors.New("installed source/images/configuration differ; upgrades are not implemented, use the recorded config or a fresh installation")
		}
		phase := record.Data["phase"]
		if operation != "ate-system" && phase != "deployed" &&
			!((operation == "atelet" || operation == "apiserver") && phase == "installing:"+operation) {
			return fmt.Errorf("installation phase is %q; retry that install component before running %s", phase, operation)
		}
		return nil
	}
	data, err = a.kubectl(ctx, nil, "get", "deployment", "ate-api-server", "--namespace=ate-system", "--ignore-not-found", "--output=json")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		return errors.New("refusing to adopt an existing Substrate installation without this CLI's install record")
	}
	if operation != "ate-system" {
		return errors.New("no completed installation record; run the full install first")
	}
	return nil
}

func (a App) recordInstall(ctx context.Context, fp, phase string) error {
	data, err := json.Marshal(object{
		"apiVersion": "v1", "kind": "List", "items": []any{
			object{"apiVersion": "v1", "kind": "Namespace", "metadata": object{"name": "ate-system"}},
			object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": "substrate-aks-install", "namespace": "ate-system"},
				"data": object{"installationId": a.Config.Installation.ID, "fingerprint": fp, "phase": phase,
					"repository": a.Config.Substrate.Source.Repository, "commit": a.Config.Substrate.Source.Commit}},
		},
	})
	if err != nil {
		return err
	}
	_, err = a.kubectl(ctx, data, "apply", "-f", "-")
	return err
}

func (a App) Install(ctx context.Context, component string) error {
	a.progress("Validating image and runtime asset configuration")
	if component != "ate-system" && component != "atelet" && component != "apiserver" {
		return errors.New("component must be ate-system, atelet or apiserver")
	}
	if err := a.Config.ValidateInstall(); err != nil {
		return err
	}
	if err := a.CheckImages(ctx); err != nil {
		return err
	}
	i, err := a.infrastructure(ctx)
	if err != nil {
		return err
	}
	root, err := a.prepare(ctx, i)
	if err != nil {
		return err
	}
	sandbox, err := sandboxManifest(root, a.Config.Substrate.Runtime.AssetsDir, i)
	if err != nil {
		return err
	}
	if err := a.credentials(ctx); err != nil {
		return err
	}
	if err := a.checkCluster(ctx); err != nil {
		return err
	}
	fp := fingerprint(a.Config, i)
	if err := a.guardInstall(ctx, fp, component); err != nil {
		return err
	}
	if err := a.checkKVM(ctx); err != nil {
		return err
	}
	if err := a.uploadAssets(ctx, i); err != nil {
		return err
	}
	if component != "atelet" {
		if err := a.bootstrapDatabase(ctx, root, i.Database); err != nil {
			return err
		}
	}
	a.progress("Recording the installation checkpoint")
	if err := a.recordInstall(ctx, fp, "installing:"+component); err != nil {
		return err
	}
	if err := a.installMetricsCollector(ctx); err != nil {
		return err
	}
	if err := a.upstream(ctx, root, "deploy", component); err != nil {
		return err
	}
	a.progress("Configuring the microVM sandbox")
	if _, err := a.kubectl(ctx, sandbox, "apply", "-f", "-"); err != nil {
		return err
	}
	if err := a.configureGrafana(ctx, i); err != nil {
		return err
	}
	a.progress("Recording the completed installation")
	return a.recordInstall(ctx, fp, "deployed")
}

func (a App) Prepare(ctx context.Context) (string, error) {
	i, err := a.infrastructure(ctx)
	if err != nil {
		return "", err
	}
	return a.prepare(ctx, i)
}

func (a App) Demo(ctx context.Context) error {
	i, err := a.infrastructure(ctx)
	if err != nil {
		return err
	}
	root, err := a.prepare(ctx, i)
	if err != nil {
		return err
	}
	if err := a.credentials(ctx); err != nil {
		return err
	}
	if err := a.guardInstall(ctx, fingerprint(a.Config, i), "demo"); err != nil {
		return err
	}
	return a.upstream(ctx, root, "deploy", "demo", "counter-microvm")
}

func (a App) Status(ctx context.Context) error {
	i, err := a.infrastructure(ctx)
	if err != nil {
		return err
	}
	if i.Database != nil {
		a.progress("Managed PostgreSQL: %s, %s, database %s (AKS egress %s)", i.Database.Backend, i.Database.Host, i.Database.Name, i.Database.EgressIP)
	}
	if i.Monitoring != nil {
		a.progress("Grafana: %s/d/substrate-aks-overview", strings.TrimSuffix(i.Monitoring.GrafanaEndpoint, "/"))
	}
	if err := a.credentials(ctx); err != nil {
		return err
	}
	if i.Monitoring != nil {
		if _, err := a.runKubectl(ctx, nil, true, "get", "deployments,podmonitors.azmonitoring.coreos.com", "--namespace=ate-system"); err != nil {
			return err
		}
	}
	a.progress("Fetching Substrate pods and persistent volumes")
	_, err = a.runKubectl(ctx, nil, true, "get", "pods,pvc", "--namespace=ate-system", "--output=wide")
	return err
}

func NewLog(c Config) (*os.File, error) {
	dir := c.workspace() + "/logs"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(dir+"/"+time.Now().UTC().Format("20060102T150405.000000000")+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}
