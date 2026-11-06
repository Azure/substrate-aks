package installer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/deploy"
)

type DatabaseInfrastructure struct {
	Backend    DatabaseBackend `json:"backend"`
	Location   string          `json:"location"`
	ResourceID string          `json:"resourceId"`
	Host       string          `json:"host"`
	Name       string          `json:"name"`
	Schema     string          `json:"schema"`
	EgressIP   string          `json:"egressIP"`
}

type databaseSecrets struct {
	Backend      DatabaseBackend `json:"backend"`
	Location     string          `json:"location"`
	Subscription string          `json:"subscription"`
	Group        string          `json:"group"`
	Admin        string          `json:"admin"`
	Owner        string          `json:"owner"`
	ReadWrite    string          `json:"readWrite"`
}

func databaseBackend(c Config) DatabaseBackend {
	if c.Database.Backend == "" {
		return DatabaseBundled
	}
	return c.Database.Backend
}

// horizonAPIVersion is the HorizonDB management API used for deployment and checks.
const horizonAPIVersion = "2026-10-01-preview"

// horizonParameterGroup must match the parameterGroupName default in horizondb.bicep.
const horizonParameterGroup = "default_pg17"

func (a App) horizonCreateMode(ctx context.Context) (string, error) {
	data, err := a.az(ctx, "resource", "list", "--resource-type", "Microsoft.HorizonDb/clusters",
		"--query", "[].{id:id,location:location,tags:tags}")
	if err != nil {
		return "", fmt.Errorf("discovering the owned HorizonDB cluster: %w", err)
	}
	type clusterMetadata struct {
		ID       string            `json:"id"`
		Location string            `json:"location"`
		Tags     map[string]string `json:"tags"`
	}
	var inventory []clusterMetadata
	if err := json.Unmarshal(data, &inventory); err != nil || inventory == nil {
		return "", errors.New("invalid HorizonDB cluster inventory; cannot select Create or Update")
	}
	prefix := "/subscriptions/" + a.Config.Azure.SubscriptionID + "/resourceGroups/" +
		a.Config.Azure.ResourceGroup + "/providers/Microsoft.HorizonDb/clusters/"
	var clusters []clusterMetadata
	for _, cluster := range inventory {
		if cluster.ID == "" || cluster.Location == "" {
			return "", errors.New("incomplete HorizonDB cluster inventory; cannot select Create or Update")
		}
		if cluster.Tags[ownershipTag] == a.Config.Installation.ID ||
			strings.HasPrefix(strings.ToLower(cluster.ID), strings.ToLower(prefix)) {
			clusters = append(clusters, cluster)
		}
	}
	if len(clusters) == 0 {
		return "Create", nil
	}
	if len(clusters) != 1 {
		return "", errors.New("multiple owned HorizonDB clusters found; refusing to select an update target")
	}
	cluster := clusters[0]
	if !strings.HasPrefix(strings.ToLower(cluster.ID), strings.ToLower(prefix)) ||
		len(cluster.ID) == len(prefix) || strings.ContainsAny(cluster.ID[len(prefix):], "/?#") ||
		cluster.Location != a.Config.DatabaseLocation() || cluster.Tags[ownershipTag] != a.Config.Installation.ID {
		return "", errors.New("HorizonDB cluster identity, region or ownership does not match; refusing to update")
	}
	data, err = a.az(ctx, "resource", "show", "--ids", cluster.ID, "--api-version", horizonAPIVersion,
		"--query", "{id:id,location:location,tags:tags,state:properties.provisioningState,version:properties.postgreSqlVersion}")
	if err != nil {
		return "", fmt.Errorf("verifying the existing HorizonDB cluster before reprovisioning: %w", err)
	}
	var existing struct {
		ID, Location, State, Version string
		Tags                         map[string]string
	}
	if err := json.Unmarshal(data, &existing); err != nil {
		return "", fmt.Errorf("reading existing HorizonDB cluster metadata: %w", err)
	}
	if !strings.EqualFold(existing.ID, cluster.ID) || existing.Location != a.Config.DatabaseLocation() ||
		existing.Tags[ownershipTag] != a.Config.Installation.ID || existing.Version != "17" {
		return "", errors.New("existing HorizonDB identity, ownership or PostgreSQL version cannot be verified; refusing to reprovision")
	}
	if existing.State != "Succeeded" {
		return "", fmt.Errorf("existing HorizonDB cluster is in provisioning state %q, not Succeeded; inspect its Azure operations before retrying", existing.State)
	}
	// The administrator login is write-once: resend it only until password
	// authentication is configured, for example after a partial deployment.
	data, err = a.az(ctx, "rest", "--method", "get", "--url",
		"https://management.azure.com"+cluster.ID+"/authentications?api-version="+horizonAPIVersion,
		"--query", "value[?name=='passwordMethod'].{state:properties.state,login:properties.administratorLogin}")
	if err != nil {
		return "", fmt.Errorf("reading HorizonDB password authentication before reprovisioning: %w", err)
	}
	var methods []struct{ State, Login string }
	if err := json.Unmarshal(data, &methods); err != nil || methods == nil || len(methods) > 1 {
		return "", errors.New("invalid HorizonDB authentication listing; cannot select Create or Update")
	}
	if len(methods) == 0 || methods[0].State != "Enabled" {
		return "Create", nil
	}
	if methods[0].Login != "" && methods[0].Login != "substrateadmin" {
		return "", fmt.Errorf("existing HorizonDB administrator %q is not the installer's; refusing to reprovision", methods[0].Login)
	}
	return "Update", nil
}

func validateDatabaseInfrastructure(c Config, db *DatabaseInfrastructure) error {
	if !c.ManagedDatabase() {
		if db != nil {
			return errors.New("deployment uses managed PostgreSQL; backend switching is not supported")
		}
		return nil
	}
	if db == nil {
		return errors.New("deployment is missing managed PostgreSQL outputs; provision the selected backend first")
	}
	kind, suffix := "Microsoft.DBforPostgreSQL/flexibleServers/", ".postgres.database.azure.com"
	if c.Database.Backend == DatabaseHorizon {
		kind, suffix = "Microsoft.HorizonDB/clusters/", "."+c.DatabaseLocation()+".horizondb.azure.com"
	}
	prefix := "/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/" + kind
	name := ""
	if len(db.ResourceID) > len(prefix) && strings.EqualFold(db.ResourceID[:len(prefix)], prefix) {
		name = strings.ToLower(db.ResourceID[len(prefix):])
	}
	ip, ipErr := netip.ParseAddr(db.EgressIP)
	if !regexp.MustCompile(`^pg[a-z0-9]+$`).MatchString(name) ||
		db.Backend != c.Database.Backend || db.Location != c.DatabaseLocation() ||
		db.Name != "substrate" || db.Schema != "substrate" || ipErr != nil || !ip.Is4() || !ip.IsGlobalUnicast() ||
		!strings.HasPrefix(db.Host, name+".") || !strings.HasSuffix(db.Host, suffix) ||
		!regexp.MustCompile(`^[a-z0-9.-]+$`).MatchString(db.Host) {
		return errors.New("managed PostgreSQL outputs are missing or inconsistent with this installation")
	}
	return nil
}

const databaseSecretName = "database-credentials"

var databaseCommandEnvironment = []string{
	"AZURE_LOGGING_ENABLE_LOG_FILE=false",
	"AZURE_CORE_COLLECT_TELEMETRY=false",
}

func (c Config) databaseVaultName() string {
	sum := sha256.Sum256([]byte(strings.ToLower(c.Azure.SubscriptionID + "/" + c.Azure.ResourceGroup + "/" + c.Installation.ID)))
	return "subpg" + hex.EncodeToString(sum[:9])
}

func (c Config) databaseVaultID() string {
	return "/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup +
		"/providers/Microsoft.KeyVault/vaults/" + c.databaseVaultName()
}

func (a App) provisionDatabaseVault(ctx context.Context, dir string) error {
	c := a.Config
	a.progress("Creating or reconciling the database credential Key Vault")
	params := map[string]any{}
	for key, value := range map[string]string{
		"location": c.Azure.Location, "resourceGroupName": c.Azure.ResourceGroup,
		"installationId": c.Installation.ID, "databaseBackend": string(c.Database.Backend),
		"databaseLocation": c.DatabaseLocation(), "vaultName": c.databaseVaultName(),
		"operatorObjectId": c.AKS.Operator.ObjectID, "operatorPrincipalType": c.AKS.Operator.PrincipalType,
	} {
		params[key] = map[string]string{"value": value}
	}
	data, err := json.Marshal(map[string]any{"parameters": params})
	if err != nil {
		return err
	}
	_, err = a.Runner.Run(ctx, Command{
		Args: []string{"az", "deployment", "sub", "create", "--name", c.deploymentName() + "-vault",
			"--location", c.Azure.Location, "--template-file", filepath.Join(dir, "database-vault.bicep"),
			"--parameters", "@/dev/stdin", "--subscription", c.Azure.SubscriptionID, "--only-show-errors", "--output", "json"},
		Input: data,
	})
	if err != nil {
		return err
	}
	return a.waitDatabaseVaultAccess(ctx, 5*time.Second, 3*time.Minute)
}

func (a App) databaseSecretIDs(ctx context.Context) ([]string, error) {
	// This projection returns identifiers only, so access errors can be diagnosed safely.
	data, err := a.Runner.Run(ctx, Command{
		Args: []string{"az", "keyvault", "secret", "list", "--query", "[?name=='" + databaseSecretName + "'].id",
			"--vault-name", a.Config.databaseVaultName(), "--subscription", a.Config.Azure.SubscriptionID,
			"--only-show-errors", "--output", "json"},
		Env: databaseCommandEnvironment,
	})
	if err != nil {
		return nil, fmt.Errorf("accessing database Key Vault %s: %w", a.Config.databaseVaultName(), err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil || ids == nil {
		return nil, errors.New("invalid database credential secret listing")
	}
	return ids, nil
}

func (a App) waitDatabaseVaultAccess(ctx context.Context, interval, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	waiting := false
	for {
		_, err := a.databaseSecretIDs(waitCtx)
		if err == nil {
			return nil
		}
		if waitCtx.Err() != nil {
			return fmt.Errorf("waiting for database Key Vault access: %w", errors.Join(waitCtx.Err(), err))
		}
		message := strings.ToLower(err.Error())
		transient := strings.Contains(message, "forbiddenbyrbac") ||
			strings.Contains(message, "caller is not authorized to perform action") ||
			strings.Contains(message, "name or service not known") ||
			strings.Contains(message, "temporary failure in name resolution") ||
			strings.Contains(message, "nodename nor servname provided, or not known") ||
			strings.Contains(message, "no address associated with hostname")
		if !transient {
			return err
		}
		if !waiting {
			a.progress("Waiting for database Key Vault access while new permissions or DNS propagate")
			waiting = true
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for database Key Vault access: %w", errors.Join(waitCtx.Err(), err))
		case <-time.After(interval):
		}
	}
}

func newDatabaseSecrets(c Config) (databaseSecrets, error) {
	s := databaseSecrets{
		Backend: c.Database.Backend, Location: c.DatabaseLocation(),
		Subscription: c.Azure.SubscriptionID, Group: c.Azure.ResourceGroup,
	}
	for _, target := range []*string{&s.Admin, &s.Owner, &s.ReadWrite} {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return s, err
		}
		*target = hex.EncodeToString(b) + "Aa1!"
	}
	return s, nil
}

func decodeDatabaseSecrets(c Config, data []byte) (databaseSecrets, error) {
	var s databaseSecrets
	if err := json.Unmarshal(data, &s); err != nil {
		return s, errors.New("invalid database credential secret; restore the original Key Vault secret")
	}
	password := regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`)
	if s.Backend != c.Database.Backend || s.Location != c.DatabaseLocation() ||
		!strings.EqualFold(s.Subscription, c.Azure.SubscriptionID) || !strings.EqualFold(s.Group, c.Azure.ResourceGroup) ||
		!password.MatchString(s.Admin) || !password.MatchString(s.Owner) || !password.MatchString(s.ReadWrite) ||
		s.Admin == s.Owner || s.Admin == s.ReadWrite || s.Owner == s.ReadWrite {
		return s, errors.New("database credential secret does not match this installation; restore the original configuration and Key Vault secret")
	}
	return s, nil
}

func (a App) databaseSecretCommand(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return a.Runner.Run(ctx, Command{
		Args: append([]string{"az", "keyvault", "secret"}, append(args,
			"--vault-name", a.Config.databaseVaultName(), "--subscription", a.Config.Azure.SubscriptionID,
			"--only-show-errors", "--output", "json")...),
		Input: input, Env: databaseCommandEnvironment, SensitiveOutput: true,
	})
}

func (a App) databaseCredentials(ctx context.Context, create bool) (databaseSecrets, error) {
	c := a.Config
	ids, err := a.databaseSecretIDs(ctx)
	if err != nil {
		return databaseSecrets{}, err
	}
	read := func() (databaseSecrets, error) {
		data, err := a.databaseSecretCommand(ctx, nil, "show", "--name", databaseSecretName, "--query", "value")
		if err != nil {
			return databaseSecrets{}, fmt.Errorf("reading database credentials from Key Vault: %w", err)
		}
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return databaseSecrets{}, errors.New("invalid Key Vault database secret response")
		}
		return decodeDatabaseSecrets(c, []byte(value))
	}
	if len(ids) != 0 {
		return read()
	}
	if !create {
		return databaseSecrets{}, errors.New("database credentials are missing from Key Vault; restore the original secret before retrying")
	}
	// A registry/vault-only deployment may exist before credentials are generated.
	data, err := a.az(ctx, "deployment", "sub", "list", "--query", c.infrastructureDeploymentFilter()+".name")
	if err != nil {
		return databaseSecrets{}, err
	}
	var existing []string
	if err := json.Unmarshal(data, &existing); err != nil || existing == nil {
		return databaseSecrets{}, errors.New("invalid prior database deployment listing; refusing to generate credentials")
	}
	if len(existing) != 0 {
		return databaseSecrets{}, errors.New("database credentials are missing after a deployment was submitted; restore the original Key Vault secret, do not generate new passwords. " +
			"If the resource group was deleted outside the installer, the database was deleted with it; create a new installation")
	}
	s, err := newDatabaseSecrets(c)
	if err != nil {
		return s, err
	}
	data, err = json.Marshal(s)
	if err != nil {
		return s, err
	}
	if _, err := a.databaseSecretCommand(ctx, data, "set", "--name", databaseSecretName,
		"--file", "/dev/stdin", "--encoding", "utf-8"); err != nil {
		return s, fmt.Errorf("saving database credentials to Key Vault: %w", err)
	}
	stored, err := read()
	if err != nil {
		return s, err
	}
	if stored != s {
		return s, errors.New("Key Vault database credential verification failed; refusing database deployment")
	}
	return s, nil
}

func databaseDSN(db *DatabaseInfrastructure, login, password string) string {
	u := url.URL{Scheme: "postgresql", Host: db.Host + ":5432", Path: "/" + db.Name, User: url.UserPassword(login, password)}
	q := url.Values{"sslmode": {"verify-full"}, "connect_timeout": {"20"}}
	u.RawQuery = q.Encode()
	return u.String()
}

func (a App) databaseEnvironment(ctx context.Context) ([]string, error) {
	if !a.Config.ManagedDatabase() {
		return nil, nil
	}
	i, err := a.infrastructure(ctx)
	if err != nil {
		return nil, err
	}
	s, err := a.databaseCredentials(ctx, false)
	if err != nil {
		return nil, err
	}
	return []string{
		"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING=" + databaseDSN(i.Database, "substrate_readwrite_user", s.ReadWrite),
		"ATE_API_POSTGRES_OWNER_CONNECTION_STRING=" + databaseDSN(i.Database, "substrate_owner_user", s.Owner),
		"ATE_API_POSTGRES_READ_WRITE_ROLE=substrate_readwrite",
		"ATE_API_POSTGRES_OWNER_ROLE=substrate_owner",
		"ATE_API_POSTGRES_SCHEMA=substrate",
	}, nil
}

func (a App) bootstrapDatabase(ctx context.Context, root string, db *DatabaseInfrastructure) (resultErr error) {
	if db == nil {
		return nil
	}
	s, err := a.databaseCredentials(ctx, false)
	if err != nil {
		return err
	}
	sql, err := os.ReadFile(filepath.Join(root, "pkg", "postgressetup", "setup.sql"))
	if err != nil {
		return fmt.Errorf("reading pinned database bootstrap: %w", err)
	}
	a.progress("Bootstrapping managed PostgreSQL roles and schema from AKS (verified TLS)")
	cleanup := func(ctx context.Context) error {
		_, err := a.kubectl(ctx, nil, "delete", "job/substrate-aks-postgres-bootstrap",
			"secret/substrate-aks-postgres-bootstrap", "configmap/substrate-aks-postgres-bootstrap",
			"--namespace=ate-system", "--ignore-not-found", "--wait=true", "--timeout=60s")
		return err
	}
	if _, err := a.kubectl(ctx, []byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ate-system"}}`), "apply", "-f", "-"); err != nil {
		return err
	}
	if err := cleanup(ctx); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, cleanup(cleanupCtx))
	}()
	metadata := object{"name": "substrate-aks-postgres-bootstrap", "namespace": "ate-system"}
	input, err := json.Marshal(object{"apiVersion": "v1", "kind": "List", "items": []any{
		object{"apiVersion": "v1", "kind": "Secret", "metadata": metadata, "type": "Opaque",
			"stringData": object{"PGHOST": db.Host, "PGPASSWORD": s.Admin, "OWNER_PASSWORD": s.Owner, "READWRITE_PASSWORD": s.ReadWrite}},
		object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata, "data": object{"setup.sql": string(sql)}},
	}})
	if err != nil {
		return err
	}
	if _, err := a.Runner.Run(ctx, Command{
		Args:  []string{"kubectl", "--kubeconfig", a.kubeconfig(), "--context", a.Config.AKS.ClusterName, "apply", "-f", "-"},
		Input: input, SensitiveOutput: true,
	}); err != nil {
		return err
	}
	job, err := deploy.DatabaseBootstrap()
	if err != nil {
		return err
	}
	if _, err := a.kubectl(ctx, job, "apply", "-f", "-"); err != nil {
		return err
	}
	err = a.waitDatabaseBootstrap(ctx, 2*time.Second, 660*time.Second)
	if err != nil {
		_, diagnosticErr := a.kubectl(ctx, nil, "describe", "job", "substrate-aks-postgres-bootstrap", "--namespace=ate-system")
		return errors.Join(fmt.Errorf("managed PostgreSQL bootstrap did not complete; check database readiness, AKS egress firewall and Key Vault credentials: %w", err), diagnosticErr)
	}
	return nil
}

func (a App) waitDatabaseBootstrap(ctx context.Context, interval, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		data, err := a.kubectl(waitCtx, nil, "get", "job", "substrate-aks-postgres-bootstrap",
			"--namespace=ate-system", "--output=json")
		if waitCtx.Err() != nil {
			return fmt.Errorf("waiting for database bootstrap: %w", errors.Join(waitCtx.Err(), err))
		}
		if err != nil {
			return err
		}
		var job *struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
					Reason string `json:"reason"`
				} `json:"conditions"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &job); err != nil || job == nil {
			return errors.New("invalid database bootstrap Job status")
		}
		for _, condition := range job.Status.Conditions {
			if condition.Status != "True" {
				continue
			}
			switch condition.Type {
			case "Failed":
				return fmt.Errorf("database bootstrap Job failed: %s", condition.Reason)
			case "Complete":
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for database bootstrap: %w", waitCtx.Err())
		case <-time.After(interval):
		}
	}
}
