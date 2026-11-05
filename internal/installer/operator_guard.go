package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type clusterState struct {
	Name                 string
	DisableLocalAccounts bool
	AADProfile           *struct{ Managed, EnableAzureRBAC bool }
	AgentPoolProfiles    []struct {
		Name, VMSize string
		Count        int
		OSDiskType   string `json:"osDiskType"`
		OSDiskSizeGB int    `json:"osDiskSizeGb"`
		MaxPods      int    `json:"maxPods"`
	}
}

// guardWorkerPool rejects changes to settings AKS fixes when the microVM pool
// is created, before a deployment fails on them partway through.
func (a App) guardWorkerPool(cluster *clusterState) error {
	if cluster == nil {
		return nil
	}
	w := a.Config.AKS.WorkerPool
	for _, pool := range cluster.AgentPoolProfiles {
		if pool.Name != "microvm" {
			continue
		}
		maxPods := w.MaxPods
		if maxPods == 0 {
			// Bicep omits an unset limit, which only matches a pool on the AKS default.
			maxPods = AKSDefaultMaxPods
		}
		if !strings.EqualFold(pool.OSDiskType, w.osDiskType()) || pool.OSDiskSizeGB != w.osDiskSizeGB() ||
			pool.MaxPods != maxPods {
			return fmt.Errorf("the existing microVM pool has osDiskType %s, osDiskSizeGB %d and maxPods %d, which AKS cannot change; restore them in aks.workerPool, or destroy and recreate the installation",
				pool.OSDiskType, pool.OSDiskSizeGB, pool.MaxPods)
		}
	}
	return nil
}

func (a App) guardOperator(ctx context.Context) (*clusterState, error) {
	a.progress("Checking that the existing AKS operator and authentication mode have not changed")
	data, err := a.az(ctx, "aks", "list", "--resource-group", a.Config.Azure.ResourceGroup)
	if err != nil {
		return nil, fmt.Errorf("checking existing AKS authentication settings: %w", err)
	}
	var clusters []clusterState
	if err := json.Unmarshal(data, &clusters); err != nil {
		return nil, err
	}
	if clusters == nil {
		return nil, errors.New("Azure did not return an AKS cluster list; authentication mode is unverified")
	}
	for _, cluster := range clusters {
		if !strings.EqualFold(cluster.Name, a.Config.AKS.ClusterName) {
			continue
		}
		managed := a.Config.AKS.Operator != nil
		hasEntra := cluster.AADProfile != nil && (cluster.AADProfile.Managed || cluster.AADProfile.EnableAzureRBAC)
		modeMatches := !cluster.DisableLocalAccounts && !hasEntra
		if managed {
			modeMatches = cluster.DisableLocalAccounts && hasEntra && cluster.AADProfile.Managed && cluster.AADProfile.EnableAzureRBAC
		}
		if !modeMatches {
			return nil, errors.New("aks.operator would change the existing cluster authentication mode; restore the original operator settings. Authentication migration is not supported")
		}
		data, err := a.az(ctx, "deployment", "group", "show", "--resource-group", a.Config.Azure.ResourceGroup,
			"--name", "substrate-aks-"+a.Config.Installation.ID)
		if err != nil {
			return nil, fmt.Errorf("cannot verify the original AKS operator from the infrastructure deployment; restore the original configuration before retrying: %w", err)
		}
		var record struct {
			Properties struct {
				Parameters map[string]struct{ Value json.RawMessage }
			}
		}
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		id, present := record.Properties.Parameters["operatorObjectId"]
		if !present {
			return nil, errors.New("existing infrastructure deployment has no operator parameter; authentication consistency is unverified")
		}
		kind, present := record.Properties.Parameters["operatorPrincipalType"]
		if !present {
			return nil, errors.New("existing infrastructure deployment has no operator principal type; authentication consistency is unverified")
		}
		var objectID, principalType *string
		if err := json.Unmarshal(id.Value, &objectID); err != nil {
			return nil, fmt.Errorf("reading deployed operator object ID: %w", err)
		}
		if err := json.Unmarshal(kind.Value, &principalType); err != nil {
			return nil, fmt.Errorf("reading deployed operator principal type: %w", err)
		}
		if objectID == nil || principalType == nil {
			return nil, errors.New("existing infrastructure deployment has null operator parameters; authentication consistency is unverified")
		}
		operator := a.Config.AKS.Operator
		if operator == nil && *objectID != "" || operator != nil && (!strings.EqualFold(operator.ObjectID, *objectID) || operator.PrincipalType != *principalType) {
			return nil, errors.New("aks.operator differs from the existing installation; restore its original objectId and principalType. Operator changes require a separately reviewed migration")
		}
		return &cluster, nil
	}
	return nil, nil
}
