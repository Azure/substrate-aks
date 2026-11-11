package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type vaultTestRunner struct {
	*scriptedRunner
	secret []byte
}

func (r *vaultTestRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if len(c.Args) < 4 || strings.Join(c.Args[:3], " ") != "az keyvault secret" {
		return r.scriptedRunner.Run(ctx, c)
	}
	r.calls = append(r.calls, c)
	if c.Args[3] != "list" && !c.SensitiveOutput {
		r.t.Fatal("Key Vault credential command is not confidential")
	}
	switch c.Args[3] {
	case "list":
		if c.SensitiveOutput || !strings.Contains(strings.Join(c.Args, " "), "--query [?name=='"+databaseSecretName+"'].id") {
			r.t.Fatal("metadata-only listing must retain diagnostics and project only secret IDs")
		}
		if r.secret == nil {
			return []byte("[]"), nil
		}
		return []byte(`["https://vault.vault.azure.net/secrets/database-credentials"]`), nil
	case "show":
		if r.secret == nil {
			return nil, errors.New("secret not found")
		}
		return json.Marshal(string(r.secret))
	case "set":
		if r.secret != nil {
			r.t.Fatal("retry overwrote existing Key Vault credentials")
		}
		if !strings.Contains(strings.Join(c.Args, " "), "--file /dev/stdin --encoding utf-8") {
			r.t.Fatal("Key Vault write did not use stdin")
		}
		r.secret = append([]byte{}, c.Input...)
		return []byte("{}"), nil
	default:
		r.t.Fatalf("unexpected Key Vault operation: %v", c.Args)
		return nil, nil
	}
}

func databaseTestRunner(t *testing.T, c Config, existing bool, replies ...reply) *vaultTestRunner {
	t.Helper()
	r := &vaultTestRunner{scriptedRunner: script(t, replies...)}
	if existing {
		s, err := newDatabaseSecrets(c)
		if err != nil {
			t.Fatal(err)
		}
		r.secret, err = json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func managedTestConfig(t *testing.T, backend DatabaseBackend) (Config, *DatabaseInfrastructure) {
	t.Helper()
	c := testConfig(t)
	c.Database = DatabaseConfig{Backend: backend, Location: "westus3"}
	// A separate operator shows that the vault grant does not follow the upload identity.
	c.AKS.Operator = &OperatorConfig{ObjectID: "44444444-4444-4444-8444-444444444444", PrincipalType: "Group"}
	kind, host := "Microsoft.DBforPostgreSQL/flexibleServers/", "pgexample.postgres.database.azure.com"
	return c, &DatabaseInfrastructure{
		Backend: backend, Location: c.Database.Location,
		ResourceID: "/subscriptions/" + c.Azure.SubscriptionID + "/resourceGroups/" + c.Azure.ResourceGroup + "/providers/" + kind + "pgexample",
		Host:       host, Name: "substrate", Schema: "substrate", EgressIP: "20.1.2.3",
	}
}

func TestDatabaseSelectionValidationAndLegacyDefault(t *testing.T) {
	for _, backend := range []DatabaseBackend{"", DatabaseBundled, DatabaseFlexible, "invalid"} {
		c := testConfig(t)
		c.Database = DatabaseConfig{Backend: backend}
		c.AKS.Operator = &OperatorConfig{ObjectID: "44444444-4444-4444-8444-444444444444", PrincipalType: "User"}
		if err := c.Validate(); (err != nil) != (backend == "invalid") {
			t.Fatalf("backend %q validation: %v", backend, err)
		}
		managed := backend == DatabaseFlexible
		c.AKS.Operator = nil
		if err := c.Validate(); managed != (err != nil && strings.Contains(err.Error(), "aks.operator")) {
			t.Fatalf("backend %q without an operator, who receives vault access: %v", backend, err)
		}
	}
	c := testConfig(t)
	c.Database.Location = "westus3"
	if c.Validate() == nil {
		t.Fatal("bundled accepted a managed database region")
	}
	c.Database = DatabaseConfig{Backend: DatabaseFlexible, Location: "invalid region"}
	if c.Validate() == nil {
		t.Fatal("managed database accepted an invalid region")
	}
}

func TestManagedDatabaseOutputsAreBoundToTheInstallation(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible} {
		t.Run(string(backend), func(t *testing.T) {
			c, db := managedTestConfig(t, backend)
			if err := validateDatabaseInfrastructure(c, db); err != nil {
				t.Fatal(err)
			}
			canonical := *db
			canonical.ResourceID = strings.ReplaceAll(canonical.ResourceID, "Microsoft.DBforPostgreSQL", "Microsoft.DBForPostgreSQL")
			if err := validateDatabaseInfrastructure(c, &canonical); err != nil {
				t.Fatalf("ARM's case-insensitive resource ID was rejected: %v", err)
			}
			i := testInfrastructure()
			bundled := fingerprint(c, i)
			i.Database = db
			if fingerprint(c, i) == bundled {
				t.Fatal("managed identity did not affect installation fingerprint")
			}
			for _, change := range []func(*DatabaseInfrastructure){
				func(d *DatabaseInfrastructure) { d.Backend = DatabaseBundled },
				func(d *DatabaseInfrastructure) { d.ResourceID += "/unexpected" },
				func(d *DatabaseInfrastructure) { d.Host = "attacker.example" },
				func(d *DatabaseInfrastructure) { d.Host += ":5432" },
				func(d *DatabaseInfrastructure) { d.EgressIP = "0.0.0.0" },
				func(d *DatabaseInfrastructure) { d.Name = "postgres" },
			} {
				altered := *db
				change(&altered)
				if validateDatabaseInfrastructure(c, &altered) == nil {
					t.Fatalf("accepted inconsistent output: %+v", altered)
				}
			}
			if validateDatabaseInfrastructure(c, nil) == nil {
				t.Fatal("managed backend accepted bundled outputs")
			}
			c.Database = DatabaseConfig{Backend: DatabaseBundled}
			if validateDatabaseInfrastructure(c, db) == nil {
				t.Fatal("bundled backend adopted a managed database")
			}
		})
	}
}

func TestDatabaseCredentialsPersistInVaultWithoutLocalFilesOrRotation(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible} {
		t.Run(string(backend), func(t *testing.T) {
			c, _ := managedTestConfig(t, backend)
			r := databaseTestRunner(t, c, false, reply{contains: "deployment sub list", output: "[]"})
			a := App{Config: c, Runner: r}
			first, err := a.databaseCredentials(t.Context(), true)
			if err != nil {
				t.Fatal(err)
			}
			second, err := a.databaseCredentials(t.Context(), true)
			if err != nil || first != second {
				t.Fatalf("retry rotated credentials: %v", err)
			}
			for _, cmd := range r.calls {
				for _, secret := range []string{first.Admin, first.Owner, first.ReadWrite} {
					if strings.Contains(strings.Join(cmd.Args, " "), secret) {
						t.Fatal("credential leaked into argv")
					}
				}
			}
			assertNoPersistedCredentials(t, c, first)
			a.Config.Database.Location = "anotherregion"
			if _, err := a.databaseCredentials(t.Context(), true); err == nil {
				t.Fatal("different database reused credentials")
			}
			r.secret = nil
			r.replies = append(r.replies, reply{contains: "deployment sub list", output: `["existing-deployment"]`})
			if _, err := a.databaseCredentials(t.Context(), true); err == nil {
				t.Fatal("lost credentials were replaced after provisioning")
			}
		})
	}
}

func TestDatabaseVaultFailuresNeverRegenerateCredentials(t *testing.T) {
	c, _ := managedTestConfig(t, DatabaseFlexible)
	for _, tc := range []struct {
		name    string
		create  bool
		replies []reply
	}{
		{"access denied", true, []reply{{contains: "keyvault secret list", err: errors.New("access denied")}}},
		{"network unavailable", true, []reply{{contains: "keyvault secret list", err: errors.New("network unavailable")}}},
		{"invalid listing", true, []reply{{contains: "keyvault secret list", output: "null"}}},
		{"missing on install", false, []reply{{contains: "keyvault secret list", output: "[]"}}},
		{"missing after deployment", true, []reply{
			{contains: "keyvault secret list", output: "[]"},
			{contains: "deployment sub list", output: `["submitted"]`},
		}},
		{"invalid deployment listing", true, []reply{
			{contains: "keyvault secret list", output: "[]"},
			{contains: "deployment sub list", output: "null"},
		}},
		{"invalid saved value", true, []reply{
			{contains: "keyvault secret list", output: `["secret-id"]`},
			{contains: "keyvault secret show", output: `"invalid"`},
		}},
		{"disabled secret", true, []reply{
			{contains: "keyvault secret list", output: `["secret-id"]`},
			{contains: "keyvault secret show", err: errors.New("disabled")},
		}},
		{"write failed", true, []reply{
			{contains: "keyvault secret list", output: "[]"},
			{contains: "deployment sub list", output: "[]"},
			{contains: "keyvault secret set", err: errors.New("write failed")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t, tc.replies...)
			if _, err := (App{Config: c, Runner: r}).databaseCredentials(t.Context(), tc.create); err == nil {
				t.Fatal("invalid or unavailable credentials became success")
			}
			if workspaceHasState(t, c) {
				t.Fatal("failure wrote local installation state")
			}
		})
	}
}

func TestDatabaseVaultAccessWaitsForPropagationWithoutCredentials(t *testing.T) {
	for _, message := range []string{"ForbiddenByRbac", "Caller is not authorized to perform action on resource",
		"Name or service not known", "Temporary failure in name resolution",
		"nodename nor servname provided, or not known", "No address associated with hostname"} {
		t.Run(message, func(t *testing.T) {
			c, _ := managedTestConfig(t, DatabaseFlexible)
			pending := errors.New(message)
			r := script(t, reply{contains: "keyvault secret list", err: pending},
				reply{contains: "keyvault secret list", err: pending},
				reply{contains: "keyvault secret list", output: "[]"})
			var progress []string
			a := App{Config: c, Runner: r, Progress: func(message string, _ ...any) { progress = append(progress, message) }}
			if err := a.waitDatabaseVaultAccess(t.Context(), 0, time.Minute); err != nil {
				t.Fatal(err)
			}
			if len(progress) != 1 || !strings.Contains(progress[0], "Waiting") {
				t.Fatalf("readiness retries were silent or excessively logged: %v", progress)
			}
			for _, command := range r.calls {
				if command.SensitiveOutput || len(command.Input) != 0 ||
					!strings.Contains(strings.Join(command.Args, " "), "--query [?name=='"+databaseSecretName+"'].id") {
					t.Fatal("access probe hid diagnostics or handled more than secret identifiers")
				}
			}
		})
	}
}

func TestDatabaseVaultAccessDoesNotRetryPermanentOrInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
	}{
		{"authentication", "", errors.New("run az login")},
		{"firewall", "", errors.New("ForbiddenByFirewall")},
		{"malformed response", "null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := managedTestConfig(t, DatabaseFlexible)
			r := script(t, reply{contains: "keyvault secret list", output: tc.output, err: tc.err})
			err := (App{Config: c, Runner: r}).waitDatabaseVaultAccess(t.Context(), 0, time.Minute)
			if err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("invalid access became success or lost the original error: %v", err)
			}
		})
	}
}

func TestDatabaseVaultAccessWaitIsBounded(t *testing.T) {
	c, _ := managedTestConfig(t, DatabaseFlexible)
	pending := errors.New("ForbiddenByRbac")
	r := script(t, reply{contains: "keyvault secret list", err: pending})
	err := (App{Config: c, Runner: r}).waitDatabaseVaultAccess(t.Context(), time.Hour, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, pending) {
		t.Fatalf("wait did not preserve the deadline and underlying access error: %v", err)
	}
}

func TestManagedProvisionWaitsForVaultAccessBeforeCredentials(t *testing.T) {
	c, _ := managedTestConfig(t, DatabaseFlexible)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := script(t, accountReply(c), targetReply(), databaseRegionReply(c.Database.Backend),
		reply{contains: "group exists", output: "false"}, reply{contains: c.deploymentName() + "-vault"})
	pending := errors.New("ForbiddenByRbac")
	runner := databaseRunnerFunc(func(ctx context.Context, command Command) ([]byte, error) {
		if strings.Contains(strings.Join(command.Args, " "), "keyvault secret list") {
			cancel()
			return nil, pending
		}
		return base.Run(ctx, command)
	})
	err := (App{Config: c, Runner: runner}).Provision(ctx, false)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, pending) {
		t.Fatalf("provisioning skipped the cancellable vault-readiness gate: %v", err)
	}
}

// assertNoPersistedCredentials fails if any generated password was written to
// the installation directory.
func assertNoPersistedCredentials(t *testing.T, c Config, s databaseSecrets) {
	t.Helper()
	if err := filepath.WalkDir(c.workspace(), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, password := range []string{s.Admin, s.Owner, s.ReadWrite} {
			if bytes.Contains(data, []byte(password)) {
				t.Fatalf("credential persisted in %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedProvisionSecretTransportAndRegistryExclusion(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible} {
		for _, registry := range []bool{false, true} {
			t.Run(string(backend)+"/registry="+map[bool]string{true: "yes", false: "no"}[registry], func(t *testing.T) {
				c, _ := managedTestConfig(t, backend)
				replies := []reply{accountReply(c), targetReply()}
				if !registry {
					replies = append(replies, databasePreflightReplies(c)...)
				}
				replies = append(replies, reply{contains: "group exists", output: "false"})
				if !registry {
					replies = append(replies, reply{contains: c.deploymentName() + "-vault"},
						reply{contains: "deployment sub list", output: "[]"})
				}
				replies = append(replies, reply{contains: "deployment sub create"})
				r := databaseTestRunner(t, c, false, replies...)
				if err := (App{Config: c, Runner: r}).provision(t.Context(), false, registry); err != nil {
					t.Fatal(err)
				}
				last := r.calls[len(r.calls)-1]
				if last.SensitiveOutput == registry {
					t.Fatal("incorrect protection for secret-bearing provisioning command")
				}
				for _, call := range r.calls {
					if !strings.Contains(strings.Join(call.Args, " "), c.deploymentName()+"-vault") {
						continue
					}
					var vault struct {
						Parameters map[string]struct{ Value string }
					}
					if err := json.Unmarshal(call.Input, &vault); err != nil {
						t.Fatal(err)
					}
					// The upload identity has no path to database credentials.
					if vault.Parameters["operatorObjectId"].Value != c.AKS.Operator.ObjectID ||
						vault.Parameters["operatorPrincipalType"].Value != c.AKS.Operator.PrincipalType {
						t.Fatalf("database vault access was not granted to the AKS operator: %+v", vault.Parameters)
					}
				}
				path := filepath.Join(c.workspace(), "infra", "parameters.json")
				if !registry {
					s, err := decodeDatabaseSecrets(c, r.secret)
					if err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(data, []byte(c.databaseVaultID())) || !bytes.Contains(data, []byte(`"reference"`)) {
						t.Fatal("ARM did not receive a Key Vault reference")
					}
					for _, password := range []string{s.Admin, s.Owner, s.ReadWrite} {
						if bytes.Contains(data, []byte(password)) || strings.Contains(strings.Join(last.Args, " "), password) {
							t.Fatal("password leaked into a local parameter file or argv")
						}
					}
					assertNoPersistedCredentials(t, c, s)
				} else {
					for _, call := range r.calls {
						if strings.Contains(strings.Join(call.Args, " "), "keyvault") {
							t.Fatalf("registry bootstrap accessed database credentials: %v", call.Args)
						}
					}
				}
			})
		}
	}
}

func TestManagedProvisionEnforcesDatabaseImmutability(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible} {
		for _, tc := range []struct {
			name, recordedBackend, recordedRegion string
			reject                                bool
		}{
			{"matching managed retry", string(backend), "", false},
			{"untagged bundled group", "", "", true},
			{"backend change", string(DatabaseBundled), "", true},
			{"region change", string(backend), "anotherregion", true},
			{"missing managed region", string(backend), "-", true},
		} {
			t.Run(string(backend)+"/"+tc.name, func(t *testing.T) {
				c, _ := managedTestConfig(t, backend)
				region := tc.recordedRegion
				if region == "" {
					region = c.DatabaseLocation()
				} else if region == "-" {
					region = ""
				}
				group, err := json.Marshal(object{"location": c.Azure.Location, "tags": map[string]string{
					ownershipTag: c.Installation.ID, "substrate-aks-database": tc.recordedBackend,
					"substrate-aks-database-location": region,
				}})
				if err != nil {
					t.Fatal(err)
				}
				replies := append([]reply{accountReply(c), targetReply()}, databasePreflightReplies(c)...)
				replies = append(replies, reply{contains: "group exists", output: "true"}, reply{contains: "group show", output: string(group)})
				if !tc.reject {
					replies = append(replies, reply{contains: "aks list", output: "[]"})
					replies = append(replies, reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
				}
				r := script(t, replies...)
				err = (App{Config: c, Runner: r}).Provision(t.Context(), true)
				if tc.reject {
					if err == nil || !strings.Contains(err.Error(), "switching") {
						t.Fatalf("database change did not stop before deployment: %v", err)
					}
				} else if err != nil {
					t.Fatalf("matching managed retry was rejected: %v", err)
				}
			})
		}
	}
}

func TestManagedPreviewDoesNotPersistCredentials(t *testing.T) {
	c, _ := managedTestConfig(t, DatabaseFlexible)
	r := script(t, accountReply(c), targetReply(), databaseRegionReply(c.Database.Backend), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub what-if", output: `{"status":"Succeeded","changes":[]}`})
	if err := (App{Config: c, Runner: r}).Provision(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.workspace(), "infra/parameters.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview retained a credential-bearing parameter file")
	}
	last := r.calls[len(r.calls)-1]
	if last.SensitiveOutput || !strings.Contains(strings.Join(last.Args, " "), "--parameters @/dev/stdin") ||
		!bytes.Contains(last.Input, []byte("PreviewOnly-NotARealPassword1!")) {
		t.Fatal("preview must use public placeholder input and retain Azure diagnostics")
	}
}

func TestManagedUpstreamUsesExplicitSecretEnvironment(t *testing.T) {
	for _, tc := range []struct {
		backend   DatabaseBackend
		component string
	}{
		{DatabaseFlexible, "ate-system"}, {DatabaseFlexible, "apiserver"},
	} {
		t.Run(string(tc.backend)+"/"+tc.component, func(t *testing.T) {
			component := tc.component
			c, db := managedTestConfig(t, tc.backend)
			r := databaseTestRunner(t, c, true)
			a := App{Config: c, Runner: r}
			s, err := a.databaseCredentials(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			i := testInfrastructure()
			i.Database = db
			outputs, err := json.Marshal(i)
			if err != nil {
				t.Fatal(err)
			}
			r.replies = append(r.replies, reply{contains: "group exists", output: "true"}, ownerReply(c),
				reply{contains: "deployment sub list", output: string(outputs)},
				reply{contains: "go run ./cmd/ate-setup deploy " + component})
			if err := a.upstream(t.Context(), t.TempDir(), "deploy", component); err != nil {
				t.Fatal(err)
			}
			cmd := r.calls[len(r.calls)-1]
			if !cmd.SensitiveOutput || !strings.Contains(strings.Join(cmd.Env, "\n"), "ATE_API_POSTGRES_OWNER_CONNECTION_STRING=") ||
				!strings.Contains(strings.Join(cmd.Env, "\n"), "sslmode=verify-full") {
				t.Fatal("upstream lost external database settings or output protection")
			}
			for _, secret := range []string{s.Admin, s.Owner, s.ReadWrite} {
				if strings.Contains(strings.Join(cmd.Args, " "), secret) {
					t.Fatal("credential leaked into argv")
				}
			}
			for _, identity := range []struct{ key, login, password string }{
				{"ATE_API_POSTGRES_OWNER_CONNECTION_STRING=", "substrate_owner_user", s.Owner},
				{"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING=", "substrate_readwrite_user", s.ReadWrite},
			} {
				found := false
				for _, entry := range cmd.Env {
					if value, ok := strings.CutPrefix(entry, identity.key); ok {
						dsn, err := url.Parse(value)
						if err != nil || dsn.User == nil {
							t.Fatal("invalid external DSN")
						}
						password, _ := dsn.User.Password()
						if dsn.User.Username() != identity.login || password != identity.password ||
							dsn.Hostname() != db.Host || dsn.Path != "/substrate" {
							t.Fatal("upstream received the wrong database login or endpoint")
						}
						found = true
					}
				}
				if !found {
					t.Fatal("upstream did not receive both database identities")
				}
			}
		})
	}
}

func TestDatabaseBootstrapFailureCleansSecretsAndDoesNotSucceed(t *testing.T) {
	c, db := managedTestConfig(t, DatabaseFlexible)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg/postgressetup"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg/postgressetup/setup.sql"), []byte("SELECT 1;"), 0600); err != nil {
		t.Fatal(err)
	}
	r := databaseTestRunner(t, c, true, reply{contains: "apply -f -"}, reply{contains: "delete job/"},
		reply{contains: "apply -f -"}, reply{contains: "apply -f -"},
		reply{contains: "get job substrate-aks-postgres-bootstrap",
			output: `{"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`},
		reply{contains: "describe job"},
		reply{contains: "delete job/"})
	a := App{Config: c, Runner: r}
	if err := a.bootstrapDatabase(t.Context(), root, db); err == nil || !strings.Contains(err.Error(), "BackoffLimitExceeded") {
		t.Fatalf("bootstrap failure was hidden: %v", err)
	}
	secretCall := r.calls[4]
	if !secretCall.SensitiveOutput || !bytes.Contains(secretCall.Input, []byte("PGPASSWORD")) {
		t.Fatal("bootstrap secret transport is not protected")
	}
	for _, cmd := range r.calls {
		if strings.Contains(strings.Join(cmd.Args, " "), "logs") {
			t.Fatal("bootstrap retrieved potentially secret-bearing SQL logs")
		}
	}
}

func TestDatabaseBootstrapWaitObservesTerminalConditions(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		responses  []string
	}{
		{"complete after pending", "", []string{`{"status":{}}`,
			`{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`}},
		{"failed immediately", "BackoffLimitExceeded", []string{
			`{"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`}},
		{"only true conditions are terminal", "DeadlineExceeded", []string{
			`{"status":{"conditions":[{"type":"Failed","status":"False"},{"type":"Complete","status":"False"}]}}`,
			`{"status":{"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded"}]}}`}},
		{"malformed response", "invalid database bootstrap Job status", []string{"{"}},
		{"null response", "invalid database bootstrap Job status", []string{"null"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := make([]reply, len(tc.responses))
			for i, response := range tc.responses {
				replies[i] = reply{contains: "get job substrate-aks-postgres-bootstrap --namespace=ate-system --output=json", output: response}
			}
			r := script(t, replies...)
			c, _ := managedTestConfig(t, DatabaseFlexible)
			err := (App{Config: c, Runner: r}).waitDatabaseBootstrap(t.Context(), 0, time.Minute)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDatabaseBootstrapWaitPropagatesQueryFailure(t *testing.T) {
	c, _ := managedTestConfig(t, DatabaseFlexible)
	failed := errors.New("Kubernetes API unavailable")
	r := script(t, reply{contains: "get job", err: failed})
	if err := (App{Config: c, Runner: r}).waitDatabaseBootstrap(t.Context(), time.Hour, time.Minute); !errors.Is(err, failed) {
		t.Fatalf("query failure was lost or retried: %v", err)
	}
}

func TestDatabaseBootstrapWaitCancellationAndDeadline(t *testing.T) {
	for _, scenario := range []string{"canceled", "deadline during query", "deadline between queries"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := managedTestConfig(t, DatabaseFlexible)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := context.DeadlineExceeded
			calls := 0
			r := databaseRunnerFunc(func(queryCtx context.Context, _ Command) ([]byte, error) {
				calls++
				switch scenario {
				case "canceled":
					cancel()
				case "deadline during query":
					<-queryCtx.Done()
					return nil, queryCtx.Err()
				}
				return []byte(`{"status":{}}`), nil
			})
			if scenario == "canceled" {
				want = context.Canceled
			}
			timeout := time.Millisecond
			if scenario == "canceled" {
				timeout = time.Minute
			}
			err := (App{Config: c, Runner: r}).waitDatabaseBootstrap(ctx, time.Hour, timeout)
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("wait did not stop on %v after one query: calls=%d, err=%v", want, calls, err)
			}
		})
	}
}
