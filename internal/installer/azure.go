package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/infra"
)

const ownershipTag = "substrate-aks-installation"

var errGroupAbsent = errors.New("installation resource group does not exist")

type Infrastructure struct {
	ClusterName        string `json:"clusterName"`
	ResourceGroup      string `json:"resourceGroup"`
	StorageAccount     string `json:"storageAccount"`
	AccountURL         string `json:"accountUrl"`
	SnapshotsContainer string `json:"snapshotsContainer"`
	AssetsContainer    string `json:"assetsContainer"`
	NodeClientID       string `json:"nodeClientId"`
	ControlClientID    string `json:"controlClientId"`
	TenantID           string `json:"tenantId"`
}

type App struct {
	Config             Config
	Runner             Runner
	ContainerEngine    string
	Progress           func(string, ...any)
	Preview            func([]ResourceChange) error
	ImageCheckProgress func(name string, done bool, err error)
}

type ResourceChange struct {
	ChangeType string `json:"changeType"`
	ResourceID string `json:"resourceId"`
}

func (a App) progress(format string, args ...any) {
	if a.Progress != nil {
		a.Progress(format, args...)
	}
}

func (a App) az(ctx context.Context, args ...string) ([]byte, error) {
	return a.Runner.Run(ctx, Command{Args: append([]string{"az"}, append(args, "--subscription", a.Config.Azure.SubscriptionID, "--only-show-errors", "--output", "json")...)})
}

func (a App) account(ctx context.Context) error {
	a.progress("Checking Azure subscription access")
	data, err := a.az(ctx, "account", "show")
	if err != nil {
		return fmt.Errorf("Azure authentication: run az login and verify subscription access: %w", err)
	}
	var account struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &account); err != nil {
		return err
	}
	if !strings.EqualFold(account.ID, a.Config.Azure.SubscriptionID) {
		return errors.New("Azure CLI returned a different subscription")
	}
	return nil
}

func (a App) ownedGroup(ctx context.Context, required bool) error {
	_, err := a.inspectOwnedGroup(ctx, required)
	return err
}

func (a App) inspectOwnedGroup(ctx context.Context, required bool) (bool, error) {
	a.progress("Checking resource group ownership")
	data, err := a.az(ctx, "group", "exists", "--name", a.Config.Azure.ResourceGroup)
	if err != nil {
		return false, err
	}
	var exists *bool
	if err := json.Unmarshal(data, &exists); err != nil {
		return false, fmt.Errorf("reading resource-group existence: %w", err)
	}
	if exists == nil {
		return false, errors.New("resource-group existence check did not return a boolean")
	}
	if !*exists {
		if required {
			return false, errGroupAbsent
		}
		a.progress("Resource group %s is available for this installation", a.Config.Azure.ResourceGroup)
		return false, nil
	}
	data, err = a.az(ctx, "group", "show", "--name", a.Config.Azure.ResourceGroup)
	if err != nil {
		return false, err
	}
	var group struct {
		Tags     map[string]string `json:"tags"`
		Location string            `json:"location"`
	}
	if err := json.Unmarshal(data, &group); err != nil {
		return false, err
	}
	if group.Tags[ownershipTag] != a.Config.Installation.ID {
		return false, fmt.Errorf("resource group %q already exists in %s and does not belong to this installation; choose an unused azure.resourceGroup in your configuration. It has not been modified", a.Config.Azure.ResourceGroup, group.Location)
	}
	if group.Location != a.Config.Azure.Location {
		return false, fmt.Errorf("resource group %q is in %s, but azure.location is %s; restore its existing location for retries or teardown. A different target requires a new installation configuration", a.Config.Azure.ResourceGroup, group.Location, a.Config.Azure.Location)
	}
	return true, nil
}

func (a App) Provision(ctx context.Context, whatIf bool) error {
	return a.provision(ctx, whatIf, false)
}

func (a App) ProvisionRegistry(ctx context.Context, whatIf bool) error {
	return a.provision(ctx, whatIf, true)
}

func (a App) guardTarget(ctx context.Context) error {
	a.progress("Checking that this installation has not been retargeted")
	data, err := a.az(ctx, "group", "list", "--tag", ownershipTag+"="+a.Config.Installation.ID)
	if err != nil {
		return fmt.Errorf("cannot verify the installation's existing resource-group target: %w", err)
	}
	var groups []struct {
		Name      string            `json:"name"`
		Tags      map[string]string `json:"tags"`
		ManagedBy string            `json:"managedBy"`
	}
	if err := json.Unmarshal(data, &groups); err != nil {
		return fmt.Errorf("reading installation resource groups: %w", err)
	}
	if groups == nil {
		return errors.New("Azure did not return a resource-group list; target consistency is unverified")
	}
	targetExists := false
	for _, group := range groups {
		if group.Name == "" || group.Tags[ownershipTag] != a.Config.Installation.ID {
			return errors.New("Azure returned an inconsistent installation resource-group list")
		}
		if strings.EqualFold(group.Name, a.Config.Azure.ResourceGroup) {
			targetExists = true
		}
	}
	clusterID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s",
		a.Config.Azure.SubscriptionID, a.Config.Azure.ResourceGroup, a.Config.AKS.ClusterName)
	for _, group := range groups {
		if !strings.EqualFold(group.Name, a.Config.Azure.ResourceGroup) {
			// AKS propagates the installation tag onto its managed node group.
			if strings.EqualFold(group.ManagedBy, clusterID) {
				if targetExists {
					continue
				}
				return fmt.Errorf("AKS-managed node resource group %q remains for cluster %q, but the tagged installation resource group %q was not found; refusing target reuse or image-reference reset. Wait for any in-progress teardown to finish, then retry. If it persists, inspect Azure for incomplete teardown or changed ownership tags before reusing this configuration", group.Name, clusterID, a.Config.Azure.ResourceGroup)
			}
			if group.ManagedBy != "" {
				return fmt.Errorf("resource group %q is managed by %q, which does not match configured AKS cluster %q; refusing to reuse this installation ID. Restore aks.clusterName and azure.resourceGroup to this installation's original values for management or teardown; use a new configuration for a separate installation", group.Name, group.ManagedBy, clusterID)
			}
			return fmt.Errorf("this installation already owns resource group %q; refusing to use target %q under the same installation ID. Restore azure.resourceGroup for management or teardown; use a new configuration for a separate installation", group.Name, a.Config.Azure.ResourceGroup)
		}
	}
	return nil
}

func (a App) provision(ctx context.Context, whatIf, registryOnly bool) error {
	if err := a.Config.Validate(); err != nil {
		return err
	}
	if err := a.account(ctx); err != nil {
		return err
	}
	if err := a.guardTarget(ctx); err != nil {
		return err
	}
	exists, err := a.inspectOwnedGroup(ctx, false)
	if err != nil {
		return err
	}
	if exists {
		cluster, err := a.guardOperator(ctx)
		if err != nil {
			return err
		}
		if err := a.guardWorkerPool(cluster); err != nil {
			return err
		}
		if err := a.guardSystemPool(cluster); err != nil {
			return err
		}
	}
	a.progress("Preparing Azure infrastructure templates")
	if err := os.MkdirAll(a.Config.workspace(), 0700); err != nil {
		return err
	}
	dir := filepath.Join(a.Config.workspace(), "infra")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"main.bicep", "resources.bicep", "registry.bicep"} {
		data, err := infra.Files.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	c := a.Config
	params := map[string]any{}
	for key, value := range map[string]any{
		"location": c.Azure.Location, "resourceGroupName": c.Azure.ResourceGroup,
		"installationId": c.Installation.ID, "clusterName": c.AKS.ClusterName,
		"kubernetesVersion": c.AKS.KubernetesVersion, "workerVmSize": c.AKS.WorkerPool.VMSize,
		"workerCount": c.AKS.WorkerPool.Count, "apiAuthorizedCidrs": append([]string{}, c.AKS.APIAuthorizedCIDRs...),
		"workerOsDiskType":            c.AKS.WorkerPool.osDiskType(),
		"workerOsDiskSizeGB":          c.AKS.WorkerPool.osDiskSizeGB(),
		"workerMaxPods":               c.AKS.WorkerPool.MaxPods,
		"assetPublisherObjectId":      c.Azure.AssetPublisherObjectID,
		"assetPublisherPrincipalType": c.publisherPrincipalType(),
		"snapshotReaderObjectId":      c.Azure.SnapshotReaderObjectID,
		"snapshotReaderPrincipalType": c.readerPrincipalType(),
		"registryOnly":                registryOnly,
	} {
		params[key] = map[string]any{"value": value}
	}
	if c.AKS.SystemPool.VMSize != DefaultSystemVMSize {
		params["systemVmSize"] = map[string]any{"value": c.AKS.SystemPool.VMSize}
	}
	if c.AKS.SystemPool.Count != 1 {
		params["systemCount"] = map[string]any{"value": c.AKS.SystemPool.Count}
	}
	if operator := c.AKS.Operator; operator != nil {
		params["operatorObjectId"] = map[string]any{"value": operator.ObjectID}
		params["operatorPrincipalType"] = map[string]any{"value": operator.PrincipalType}
	}
	data, err := json.Marshal(map[string]any{
		"$schema":        "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
		"contentVersion": "1.0.0.0", "parameters": params,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "parameters.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	action := "create"
	if whatIf {
		action = "what-if"
	}
	deployment := c.deploymentName()
	if registryOnly {
		deployment += "-registry"
	}
	args := []string{"deployment", "sub", action, "--name", deployment,
		"--location", c.Azure.Location, "--template-file", filepath.Join(dir, "main.bicep"), "--parameters", "@" + path}
	if whatIf {
		a.progress("Asking Azure to preview resource changes; no resources will be deployed")
		args = append(args, "--no-pretty-print")
	} else if registryOnly {
		a.progress("Creating or reconciling the container registry and publisher permissions")
	} else {
		a.progress("Creating or reconciling AKS, registry, storage and identities; this can take several minutes")
	}
	data, err = a.az(ctx, args...)
	if err != nil {
		return err
	}
	if whatIf {
		return a.showPlan(data)
	}
	return nil
}

func (a App) showPlan(data []byte) error {
	var plan struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Changes []ResourceChange `json:"changes"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return fmt.Errorf("reading Azure infrastructure preview: %w", err)
	}
	if plan.Error != nil {
		return fmt.Errorf("Azure infrastructure preview failed: %s", plan.Error.Message)
	}
	if plan.Status != "Succeeded" || plan.Changes == nil {
		return fmt.Errorf("Azure returned an incomplete infrastructure preview (status %q); inspect the diagnostic log", plan.Status)
	}
	if a.Preview != nil {
		return a.Preview(plan.Changes)
	}
	return nil
}

func (a App) infrastructure(ctx context.Context) (Infrastructure, error) {
	if err := a.ownedGroup(ctx, true); err != nil {
		return Infrastructure{}, err
	}
	a.progress("Loading this installation's Azure resource details")
	data, err := a.az(ctx, "deployment", "sub", "show", "--name", a.Config.deploymentName(), "--query", "properties.outputs.infrastructure.value")
	if err != nil {
		return Infrastructure{}, err
	}
	var i Infrastructure
	if err := json.Unmarshal(data, &i); err != nil {
		return i, err
	}
	if i.ClusterName != a.Config.AKS.ClusterName || i.ResourceGroup != a.Config.Azure.ResourceGroup ||
		!regexpStorageAccount(i.StorageAccount) || !uuidPattern.MatchString(i.TenantID) ||
		!uuidPattern.MatchString(i.NodeClientID) || !uuidPattern.MatchString(i.ControlClientID) ||
		i.SnapshotsContainer != "snapshots" || i.AssetsContainer != "assets" ||
		strings.TrimSuffix(i.AccountURL, "/") != "https://"+i.StorageAccount+".blob.core.windows.net" {
		return i, errors.New("deployment outputs are missing or inconsistent with this installation")
	}
	i.AccountURL = strings.TrimSuffix(i.AccountURL, "/")
	return i, nil
}

func regexpStorageAccount(s string) bool {
	if len(s) < 3 || len(s) > 24 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (a App) credentials(ctx context.Context) error {
	a.progress("Connecting to AKS with an installation-specific kubeconfig")
	if err := os.MkdirAll(a.Config.workspace(), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(a.Config.workspace(), ".kubeconfig-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Close(); err != nil {
		return err
	}
	_, err = a.az(ctx, "aks", "get-credentials", "--resource-group", a.Config.Azure.ResourceGroup,
		"--name", a.Config.AKS.ClusterName, "--file", path, "--context", a.Config.AKS.ClusterName, "--overwrite-existing")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := validateKubeconfig(data, a.Config.AKS.ClusterName); err != nil {
		return err
	}
	if a.Config.AKS.Operator != nil {
		a.progress("Configuring kubelogin to reuse the current Azure CLI login")
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := a.Runner.Run(probeCtx, Command{Args: []string{"kubelogin", "convert-kubeconfig", "--login", "azurecli", "--kubeconfig", path}})
		cancel()
		if err != nil {
			return fmt.Errorf("converting private AKS credentials; existing kubeconfig preserved: %w", err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := validateEntraKubeconfig(data, a.Config.AKS.ClusterName); err != nil {
			return err
		}
	}
	file, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := errors.Join(file.Chmod(0600), file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(path, a.kubeconfig())
}

func (a App) kubeconfig() string { return filepath.Join(a.Config.workspace(), "kubeconfig") }

func (a App) kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return a.runKubectl(ctx, input, false, args...)
}

func (a App) runKubectl(ctx context.Context, input []byte, showOutput bool, args ...string) ([]byte, error) {
	return a.Runner.Run(ctx, Command{
		Args:       append([]string{"kubectl", "--kubeconfig", a.kubeconfig(), "--context", a.Config.AKS.ClusterName}, args...),
		Input:      input,
		ShowOutput: showOutput,
	})
}

func (a App) Destroy(ctx context.Context, confirmation string) error {
	a.progress("Checking teardown confirmation")
	if confirmation != a.Config.Azure.ResourceGroup {
		return fmt.Errorf("deletes cluster, registry and all its images, PostgreSQL data, snapshots and assets; pass --confirm-delete=%s", a.Config.Azure.ResourceGroup)
	}
	if err := a.account(ctx); err != nil {
		return err
	}
	// The CLI deletes local state after a successful Destroy, so leftover
	// installation-tagged groups must fail here rather than be orphaned.
	if err := a.guardTarget(ctx); err != nil {
		return err
	}
	if err := a.ownedGroup(ctx, true); err != nil {
		if errors.Is(err, errGroupAbsent) {
			a.progress("Resource group is already absent; nothing to delete")
			return nil
		}
		return err
	}
	a.progress("Deleting the resource group, cluster, registry images and all persistent data; this can take several minutes")
	_, err := a.az(ctx, "group", "delete", "--name", a.Config.Azure.ResourceGroup, "--yes")
	return err
}
