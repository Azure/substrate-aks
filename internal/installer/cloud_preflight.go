package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type cloudPermissions struct {
	Value []struct {
		Actions, NotActions []string
	}
	NextLink string
}

func (permissions cloudPermissions) allows(action string) bool {
	matches := func(pattern string) bool {
		pattern = "(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
		return regexp.MustCompile(pattern).MatchString(action)
	}
	for _, permission := range permissions.Value {
		grant, excluded := false, false
		for _, pattern := range permission.Actions {
			grant = grant || matches(pattern)
		}
		for _, pattern := range permission.NotActions {
			excluded = excluded || matches(pattern)
		}
		if grant && !excluded {
			return true
		}
	}
	return false
}

func (a App) cloudPermissions(ctx context.Context, scope string) (cloudPermissions, error) {
	url := "https://management.azure.com" + scope + "/providers/Microsoft.Authorization/permissions?api-version=2015-07-01"
	data, err := a.az(ctx, "rest", "--method", "get", "--url", url)
	if err != nil {
		return cloudPermissions{}, fmt.Errorf("cannot verify deployment permissions at %s before provisioning: %w", scope, err)
	}
	var permissions cloudPermissions
	if err := json.Unmarshal(data, &permissions); err != nil {
		return permissions, fmt.Errorf("reading Azure permissions: %w", err)
	}
	if permissions.Value == nil || permissions.NextLink != "" {
		return permissions, errors.New("Azure permissions response is incomplete; deployment authority is unverified")
	}
	return permissions, nil
}

func (a App) CloudPreflight(ctx context.Context) error {
	if err := a.Config.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
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
	var cluster *clusterState
	if exists {
		cluster, err = a.guardOperator(ctx)
		if err != nil {
			return err
		}
	}
	a.progress("Checking subscription deployment and role-assignment permissions")
	scope := "/subscriptions/" + a.Config.Azure.SubscriptionID
	subscriptionPermissions, err := a.cloudPermissions(ctx, scope)
	if err != nil {
		return err
	}
	if !subscriptionPermissions.allows("Microsoft.Resources/deployments/write") {
		return errors.New("subscription permission Microsoft.Resources/deployments/write is missing; obtain subscription deployment rights before provisioning")
	}
	resourcePermissions := subscriptionPermissions
	if exists {
		resourcePermissions, err = a.cloudPermissions(ctx, scope+"/resourceGroups/"+a.Config.Azure.ResourceGroup)
		if err != nil {
			return err
		}
	}
	for _, action := range []string{
		"Microsoft.Resources/subscriptions/resourceGroups/write",
		"Microsoft.Resources/deployments/write",
		"Microsoft.Authorization/roleAssignments/write",
		"Microsoft.ContainerService/managedClusters/write",
		"Microsoft.ContainerRegistry/registries/write",
		"Microsoft.Storage/storageAccounts/write",
		"Microsoft.Storage/storageAccounts/blobServices/write",
		"Microsoft.Storage/storageAccounts/blobServices/containers/write",
		"Microsoft.ManagedIdentity/userAssignedIdentities/write",
		"Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials/write",
	} {
		if !resourcePermissions.allows(action) {
			return fmt.Errorf("permission %s is missing for the installation resource group; obtain the required deployment/role-assignment rights before provisioning", action)
		}
	}
	a.progress("Checking AKS %s availability in %s", a.Config.AKS.KubernetesVersion, a.Config.Azure.Location)
	data, err := a.az(ctx, "aks", "get-versions", "--location", a.Config.Azure.Location)
	if err != nil {
		return err
	}
	var versions struct {
		Values []struct {
			Version       string
			PatchVersions map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &versions); err != nil {
		return fmt.Errorf("reading AKS versions: %w", err)
	}
	available := false
	for _, version := range versions.Values {
		if a.Config.AKS.KubernetesVersion == version.Version && len(version.PatchVersions) > 0 {
			available = true
		}
		if _, ok := version.PatchVersions[a.Config.AKS.KubernetesVersion]; ok {
			available = true
		}
	}
	if !available {
		return fmt.Errorf("AKS %s is not listed as available in %s; select a supported region/version before provisioning (an older Azure CLI may need updating)", a.Config.AKS.KubernetesVersion, a.Config.Azure.Location)
	}
	needed := map[string]int64{}
	total := int64(0)
	var skuCatalog []byte
	for _, pool := range []struct {
		name, size string
		count      int
	}{{"system", SystemVMSize, 1}, {"microvm", a.Config.AKS.WorkerPool.VMSize, a.Config.AKS.WorkerPool.Count}} {
		count := pool.count
		if cluster != nil {
			for _, existing := range cluster.AgentPoolProfiles {
				if existing.Name == pool.name && strings.EqualFold(existing.VMSize, pool.size) {
					count = max(0, count-existing.Count)
				}
			}
		}
		if count == 0 {
			continue
		}
		a.progress("Checking availability and CPU requirements for %s", pool.size)
		if skuCatalog == nil {
			a.progress("Loading the VM SKU catalog for %s", a.Config.Azure.Location)
			skuCatalog, err = a.regionalSKUCatalog(ctx)
			if err != nil {
				return err
			}
		}
		family, cpus, err := a.cloudSKU(skuCatalog, pool.size)
		if err != nil {
			return err
		}
		additional := int64(count) * cpus
		needed[strings.ToLower(family)] += additional
		total += additional
	}
	if total > 0 {
		a.progress("Checking regional and VM-family CPU quota for %d additional vCPUs", total)
		data, err := a.az(ctx, "vm", "list-usage", "--location", a.Config.Azure.Location)
		if err != nil {
			return err
		}
		var usage []struct {
			Name         struct{ Value string }
			CurrentValue json.Number
			Limit        json.Number
		}
		if err := json.Unmarshal(data, &usage); err != nil {
			return fmt.Errorf("reading Azure CPU quota: %w", err)
		}
		needed["cores"] = total
		for family, required := range needed {
			found := false
			for _, quota := range usage {
				if !strings.EqualFold(quota.Name.Value, family) {
					continue
				}
				found = true
				used, usedErr := quota.CurrentValue.Int64()
				limit, limitErr := quota.Limit.Int64()
				if usedErr != nil || limitErr != nil || used < 0 || limit < 0 {
					return fmt.Errorf("CPU quota %s has invalid usage or limit; capacity is unverified", family)
				}
				if required > limit-used {
					return fmt.Errorf("insufficient %s CPU quota in %s: %d used, %d limit, %d additional required; request quota or select another region/SKU before provisioning", family, a.Config.Azure.Location, used, limit, required)
				}
			}
			if !found {
				return fmt.Errorf("Azure did not return CPU quota for %s; capacity is unverified", family)
			}
		}
	}
	a.progress("Azure preflight passed. Permissions, quota and capacity can change; conditional/deny policies and later provider checks may still block deployment")
	return nil
}

func (a App) regionalSKUCatalog(ctx context.Context) ([]byte, error) {
	// Azure CLI's list-skus can filter --location locally, downloading every region.
	endpoint := "https://management.azure.com/subscriptions/" + a.Config.Azure.SubscriptionID + "/providers/Microsoft.Compute/skus"
	params := url.Values{
		"api-version": {"2021-07-01"},
		"$filter":     {"location eq '" + strings.ReplaceAll(a.Config.Azure.Location, "'", "''") + "'"},
	}
	next := endpoint + "?" + params.Encode()
	query := fmt.Sprintf("{value:value[?resourceType=='virtualMachines' && (name=='%s' || name=='%s')],nextLink:nextLink}", SystemVMSize, a.Config.AKS.WorkerPool.VMSize)
	var skus []json.RawMessage
	seen := map[string]bool{}
	for next != "" {
		pageURL, err := url.Parse(next)
		if err != nil || pageURL.Scheme != "https" || pageURL.Host != "management.azure.com" ||
			pageURL.User != nil || pageURL.Fragment != "" ||
			"https://"+pageURL.Host+pageURL.EscapedPath() != endpoint {
			return nil, fmt.Errorf("Azure returned an invalid VM SKU pagination URL")
		}
		if seen[next] {
			return nil, fmt.Errorf("Azure repeated a VM SKU catalog page; availability is unverified")
		}
		seen[next] = true
		data, err := a.az(ctx, "rest", "--method", "get", "--url", next, "--query", query)
		if err != nil {
			return nil, err
		}
		var page struct {
			Value    []json.RawMessage
			NextLink string
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("reading regional VM SKU catalog: %w", err)
		}
		if page.Value == nil {
			return nil, fmt.Errorf("Azure returned an incomplete VM SKU catalog; availability is unverified")
		}
		skus = append(skus, page.Value...)
		next = page.NextLink
		if next != "" {
			a.progress("Loading VM SKU catalog page %d for %s", len(seen)+1, a.Config.Azure.Location)
		}
	}
	return json.Marshal(skus)
}

func (a App) cloudSKU(data []byte, size string) (string, int64, error) {
	var skus []struct {
		Name, Family string
		Capabilities []struct{ Name, Value string }
		Restrictions []struct {
			Type   string
			Values []string
		}
	}
	if err := json.Unmarshal(data, &skus); err != nil {
		return "", 0, fmt.Errorf("reading VM SKU %s: %w", size, err)
	}
	for _, sku := range skus {
		if !strings.EqualFold(sku.Name, size) {
			continue
		}
		for _, restriction := range sku.Restrictions {
			if strings.EqualFold(restriction.Type, "Location") {
				if len(restriction.Values) == 0 {
					return "", 0, fmt.Errorf("VM SKU %s has an unspecified location restriction; availability is unverified", size)
				}
				for _, location := range restriction.Values {
					if strings.EqualFold(location, a.Config.Azure.Location) {
						return "", 0, fmt.Errorf("VM SKU %s is restricted in %s for this subscription", size, location)
					}
				}
			}
		}
		for _, capability := range sku.Capabilities {
			if capability.Name == "vCPUs" && sku.Family != "" {
				cpus, err := strconv.ParseInt(capability.Value, 10, 64)
				if err != nil || cpus <= 0 || cpus > 1024 {
					return "", 0, fmt.Errorf("VM SKU %s has invalid vCPU capability", size)
				}
				return sku.Family, cpus, nil
			}
		}
		return "", 0, fmt.Errorf("VM SKU %s has no usable CPU/family information", size)
	}
	return "", 0, fmt.Errorf("VM SKU %s is not available in %s", size, a.Config.Azure.Location)
}
