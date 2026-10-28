package installer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

const flexibleCapabilitiesJSON = `[{
	"name":"FlexibleServerCapabilities","status":null,"restricted":null,"reason":null,
	"supportedServerVersions":[{"name":"17"}],
	"supportedServerEditions":[{"name":"GeneralPurpose","supportedServerSkus":[{"name":"Standard_D2ads_v5"}]}]
}]`

const horizonCapabilitiesJSON = `{"value":[{
	"location":"eastus",
	"clusterCapabilities":{"supportedPostgreSqlVersions":[{"name":"17"}]},
	"poolCapabilities":{"supportedProcessors":[{
		"name":"Intel","supportedGenerations":[{
			"computeGeneration":1,"supportedComputeModels":[{
				"type":"Provisioned","supportedComputeSizes":[{"vCores":2}]
			}]
		}]
	}]}
}]}`

func databaseRegionReply(backend DatabaseBackend) reply {
	if backend == DatabaseFlexible {
		return reply{contains: "postgres flexible-server list-skus --location westus3", output: flexibleCapabilitiesJSON}
	}
	return reply{contains: "Microsoft.HorizonDB/locations/eastus/capabilities?api-version=2026-10-01-preview", output: horizonCapabilitiesJSON}
}

// workspaceHasState reports whether anything was written in the installation
// directory, which exists before any command runs.
func workspaceHasState(t *testing.T, c Config) bool {
	t.Helper()
	entries, err := os.ReadDir(c.workspace())
	if err != nil {
		t.Fatal(err)
	}
	return len(entries) != 0
}

func databasePreflightReplies(c Config) []reply {
	return []reply{databaseRegionReply(c.Database.Backend)}
}

func TestFlexibleDatabaseRegionCapabilities(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"supported with null status", flexibleCapabilitiesJSON, ""},
		{"subscription restricted with null status", `[{"status":null,"restricted":null,"reason":"Subscriptions are restricted from provisioning in this region.","supportedServerEditions":[]}]`, "Subscriptions are restricted"},
		{"offer restricted", strings.Replace(flexibleCapabilitiesJSON, `"reason":null`, `"reason":null,"supportedFeatures":[{"name":"OfferRestricted","status":"Enabled"}]`, 1), "OfferRestricted"},
		{"region disabled", strings.Replace(flexibleCapabilitiesJSON, `"status":null`, `"status":"Disabled"`, 1), "required PostgreSQL 17"},
		{"no PostgreSQL 17", strings.ReplaceAll(flexibleCapabilitiesJSON, `"17"`, `"16"`), "required PostgreSQL 17"},
		{"wrong tier", strings.ReplaceAll(flexibleCapabilitiesJSON, `"GeneralPurpose"`, `"Burstable"`), "required PostgreSQL 17"},
		{"wrong SKU", strings.ReplaceAll(flexibleCapabilitiesJSON, `"Standard_D2ads_v5"`, `"Standard_D4ads_v5"`), "required PostgreSQL 17"},
		{"restricted SKU", strings.ReplaceAll(flexibleCapabilitiesJSON, `"name":"Standard_D2ads_v5"`, `"name":"Standard_D2ads_v5","restricted":true`), "required PostgreSQL 17"},
		{"no capabilities", "[]", "required PostgreSQL 17"},
		{"null", "null", "reading Flexible Server"},
		{"wrong shape", "{}", "reading Flexible Server"},
		{"malformed", "{", "reading Flexible Server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFlexibleCapabilities([]byte(tc.data))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestHorizonDatabaseRegionCapabilities(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"supported", horizonCapabilitiesJSON, ""},
		{"wrong region", strings.ReplaceAll(horizonCapabilitiesJSON, `"eastus"`, `"westus3"`), "required PostgreSQL 17"},
		{"no PostgreSQL 17", strings.ReplaceAll(horizonCapabilitiesJSON, `"17"`, `"18"`), "required PostgreSQL 17"},
		{"wrong processor", strings.ReplaceAll(horizonCapabilitiesJSON, `"Intel"`, `"AMD"`), "required PostgreSQL 17"},
		{"service-selected generation", strings.ReplaceAll(horizonCapabilitiesJSON, `"computeGeneration":1`, `"computeGeneration":2`), ""},
		{"wrong model", strings.ReplaceAll(horizonCapabilitiesJSON, `"Provisioned"`, `"Serverless"`), "required PostgreSQL 17"},
		{"wrong size", strings.ReplaceAll(horizonCapabilitiesJSON, `"vCores":2`, `"vCores":4`), "required PostgreSQL 17"},
		{"missing capabilities", `{}`, "reading HorizonDB"},
		{"null", "null", "reading HorizonDB"},
		{"wrong shape", `[]`, "reading HorizonDB"},
		{"malformed", "{", "reading HorizonDB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHorizonCapabilities([]byte(tc.data), "eastus")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDatabaseRegionValidationIsReadOnlyAndSubscriptionScoped(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseBundled, DatabaseFlexible, DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			c, _ := managedTestConfig(t, backend)
			r := script(t)
			if c.ManagedDatabase() {
				r.replies = append(r.replies, databaseRegionReply(backend))
			}
			if err := ValidateDatabaseRegion(t.Context(), r, c.Azure.SubscriptionID, c.Azure.Location, c.Database); err != nil {
				t.Fatal(err)
			}
			for _, call := range r.calls {
				if !strings.Contains(strings.Join(call.Args, " "), "--subscription "+c.Azure.SubscriptionID) {
					t.Fatalf("capability request lost subscription binding: %v", call.Args)
				}
			}
			if workspaceHasState(t, c) {
				t.Fatal("capability validation created local installation state")
			}
		})
	}
}

func TestDatabaseRegionFailureStopsBeforeProvisioning(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		for _, whatIf := range []bool{false, true} {
			c, _ := managedTestConfig(t, backend)
			for _, apiError := range []error{nil, errors.New("Azure capability access denied")} {
				t.Run(string(backend)+"/preview="+map[bool]string{true: "yes", false: "no"}[whatIf]+"/error="+map[bool]string{true: "yes", false: "no"}[apiError != nil], func(t *testing.T) {
					capability := databaseRegionReply(backend)
					capability.output, capability.err = "null", apiError
					r := script(t, accountReply(c), targetReply(), capability)
					err := (App{Config: c, Runner: r}).Provision(t.Context(), whatIf)
					if err == nil || !strings.Contains(err.Error(), c.DatabaseLocation()) ||
						!strings.Contains(err.Error(), "no alternate region") {
						t.Fatalf("capability failure did not stop provisioning: %v", err)
					}
					if apiError != nil && !errors.Is(err, apiError) {
						t.Fatalf("Azure failure was not propagated: %v", err)
					}
					if workspaceHasState(t, c) {
						t.Fatal("invalid region created local provisioning state")
					}
				})
			}
		}
	}
}

func TestDatabaseRegionRestrictionsRequireVerifiedExistingResource(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		for _, scenario := range []string{"existing", "absent group", "unowned group", "missing outputs",
			"missing resource", "wrong resource", "wrong region", "unowned resource", "invalid response"} {
			t.Run(string(backend)+"/"+scenario, func(t *testing.T) {
				c, db := managedTestConfig(t, backend)
				i := testInfrastructure()
				i.Database = db
				outputs, err := json.Marshal(i)
				if err != nil {
					t.Fatal(err)
				}
				capability := databaseRegionReply(backend)
				capability.output = "[]"
				if backend == DatabaseHorizon {
					capability.output = `{"value":[]}`
				}
				replies := []reply{capability}
				if scenario == "absent group" {
					replies = append(replies, reply{contains: "group exists", output: "false"})
				} else {
					group := ownerReply(c)
					if scenario == "unowned group" {
						group.output = strings.ReplaceAll(group.output, c.Installation.ID, "another-installation")
					}
					replies = append(replies, reply{contains: "group exists", output: "true"}, group)
					if scenario != "unowned group" {
						if scenario == "missing outputs" {
							outputs = []byte("null")
						}
						replies = append(replies, reply{contains: "deployment sub list", output: string(outputs)})
						if scenario != "missing outputs" {
							id, region, owner := db.ResourceID, db.Location, c.Installation.ID
							switch scenario {
							case "wrong resource":
								id += "other"
							case "wrong region":
								region = "anotherregion"
							case "unowned resource":
								owner = "another-installation"
							}
							resource, err := json.Marshal(object{"id": id, "location": region, "tags": map[string]string{ownershipTag: owner}})
							if err != nil {
								t.Fatal(err)
							}
							apiVersion := "2025-08-01"
							if backend == DatabaseHorizon {
								apiVersion = "2026-05-01-preview"
							}
							response := reply{contains: "resource show --ids " + db.ResourceID + " --api-version " + apiVersion, output: string(resource)}
							if scenario == "missing resource" {
								response.err = errors.New("ResourceNotFound")
							} else if scenario == "invalid response" {
								response.output = "{"
							}
							replies = append(replies, response)
						}
					}
				}
				r := script(t, replies...)
				var progress []string
				a := App{Config: c, Runner: r, Progress: func(format string, _ ...any) { progress = append(progress, format) }}
				err = a.CheckDatabaseRegion(t.Context())
				if scenario == "existing" {
					if err != nil || !strings.Contains(strings.Join(progress, "\n"), "Warning:") {
						t.Fatalf("existing database did not continue with an explicit warning: %v; %v", err, progress)
					}
				} else if !errors.Is(err, errDatabaseRegionUnavailable) {
					t.Fatalf("restriction was bypassed without a verified resource: %v", err)
				}
				for _, command := range r.calls {
					if !strings.Contains(strings.Join(command.Args, " "), "--subscription "+c.Azure.SubscriptionID) ||
						len(command.Input) != 0 || command.SensitiveOutput {
						t.Fatalf("validation was not a subscription-scoped, metadata-only check: %v", command.Args)
					}
				}
			})
		}
	}
}

func TestManagedProvisionRetriesRestrictedExistingDatabase(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		for _, whatIf := range []bool{false, true} {
			t.Run(string(backend)+map[bool]string{false: "/provision", true: "/preview"}[whatIf], func(t *testing.T) {
				c, db := managedTestConfig(t, backend)
				i := testInfrastructure()
				i.Database = db
				outputs, err := json.Marshal(i)
				if err != nil {
					t.Fatal(err)
				}
				group, err := json.Marshal(object{"location": c.Azure.Location, "tags": map[string]string{
					ownershipTag: c.Installation.ID, "substrate-aks-database": string(backend),
					"substrate-aks-database-location": c.DatabaseLocation(),
				}})
				if err != nil {
					t.Fatal(err)
				}
				resource, err := json.Marshal(object{"id": db.ResourceID, "location": db.Location,
					"tags": map[string]string{ownershipTag: c.Installation.ID}})
				if err != nil {
					t.Fatal(err)
				}
				capability := databaseRegionReply(backend)
				capability.output = "[]"
				if backend == DatabaseHorizon {
					capability.output = `{"value":[]}`
				}
				replies := []reply{accountReply(c), targetReply(), capability, {contains: "group exists", output: "true"},
					{contains: "group show", output: string(group)}, {contains: "deployment sub list", output: string(outputs)},
					{contains: "resource show --ids " + db.ResourceID, output: string(resource)}}
				replies = append(replies, reply{contains: "group exists", output: "true"}, reply{contains: "group show", output: string(group)},
					reply{contains: "aks list", output: "[]"})
				if backend == DatabaseHorizon {
					replies = append(replies, horizonExistingReplies(t, c, db)...)
				}
				action := "create"
				if whatIf {
					action = "what-if"
				} else {
					replies = append(replies, reply{contains: c.deploymentName() + "-vault"})
				}
				replies = append(replies, reply{contains: "deployment sub " + action, output: `{"status":"Succeeded","changes":[]}`})
				r := databaseTestRunner(t, c, true, replies...)
				if err := (App{Config: c, Runner: r}).Provision(t.Context(), whatIf); err != nil {
					t.Fatalf("restricted existing database could not be reconciled: %v", err)
				}
			})
		}
	}
}

type databaseRunnerFunc func(context.Context, Command) ([]byte, error)

func (r databaseRunnerFunc) Run(ctx context.Context, command Command) ([]byte, error) {
	return r(ctx, command)
}

func TestAvailableDatabaseRegionsFiltersServiceSubscriptionAndProfile(t *testing.T) {
	for _, backend := range []DatabaseBackend{DatabaseFlexible, DatabaseHorizon} {
		t.Run(string(backend), func(t *testing.T) {
			c, _ := managedTestConfig(t, backend)
			var mu sync.Mutex
			checked := map[string]int{}
			runner := databaseRunnerFunc(func(_ context.Context, command Command) ([]byte, error) {
				args := strings.Join(command.Args, " ")
				if !strings.Contains(args, "--subscription "+c.Azure.SubscriptionID) {
					t.Errorf("discovery lost subscription binding: %v", command.Args)
				}
				if strings.Contains(args, "provider show") {
					namespace := "Microsoft.DBforPostgreSQL"
					if backend == DatabaseHorizon {
						namespace = "Microsoft.HorizonDB"
					}
					if !strings.Contains(args, "--namespace "+namespace) {
						t.Errorf("discovery used the wrong provider: %v", command.Args)
					}
					return []byte(`["East US","North Europe","South Central US","West US 3","UK South"]`), nil
				}
				var region string
				for _, candidate := range []string{"eastus", "northeurope", "southcentralus", "westus3"} {
					if strings.Contains(args, "--location "+candidate+" ") ||
						strings.Contains(args, "/locations/"+candidate+"/capabilities?") {
						region = candidate
					}
				}
				if region == "" {
					t.Errorf("queried a region outside the physical/service intersection: %v", command.Args)
					return nil, errors.New("unexpected region")
				}
				mu.Lock()
				checked[region]++
				mu.Unlock()
				profile := flexibleCapabilitiesJSON
				if backend == DatabaseHorizon {
					profile = strings.ReplaceAll(horizonCapabilitiesJSON, `"eastus"`, `"`+region+`"`)
				}
				switch region {
				case "southcentralus":
					if backend == DatabaseFlexible {
						return []byte(`[{"status":null,"restricted":null,"reason":"Subscriptions are restricted from provisioning in this region."}]`), nil
					}
					return []byte(`{"value":[]}`), nil
				case "northeurope":
					profile = strings.ReplaceAll(profile, `"17"`, `"16"`)
				}
				return []byte(profile), nil
			})
			got, err := AvailableDatabaseRegions(t.Context(), runner, c.Azure.SubscriptionID, backend,
				[]string{"westus3", "southcentralus", "antarctica", "eastus", "northeurope", "westus3"})
			if err != nil || !slices.Equal(got, []string{"eastus", "westus3"}) {
				t.Fatalf("unavailable regions leaked into choices: %v, %v", got, err)
			}
			for _, region := range []string{"eastus", "northeurope", "southcentralus", "westus3"} {
				if checked[region] != 1 {
					t.Fatalf("capability checks for %s = %d, want 1", region, checked[region])
				}
			}
		})
	}
}

func TestAvailableDatabaseRegionsFailsClosed(t *testing.T) {
	denied := errors.New("capability access denied")
	for _, tc := range []struct {
		name, metadata, capabilities, want string
		err                                error
	}{
		{"empty metadata", "[]", flexibleCapabilitiesJSON, "no flexible-server service regions", nil},
		{"null metadata", "null", flexibleCapabilitiesJSON, "no flexible-server service regions", nil},
		{"malformed metadata", "{", flexibleCapabilitiesJSON, "reading flexible-server service regions", nil},
		{"invalid metadata region", `[""]`, flexibleCapabilitiesJSON, "invalid flexible-server service region", nil},
		{"no matching physical region", `["UK South"]`, flexibleCapabilitiesJSON, "no flexible-server regions", nil},
		{"restricted subscription", `["West US 3"]`, `[{"reason":"Subscription restricted"}]`, "no flexible-server regions", nil},
		{"malformed capability", `["West US 3"]`, "{", "reading Flexible Server capabilities", nil},
		{"null capability", `["West US 3"]`, "null", "reading Flexible Server capabilities", nil},
		{"API failure", `["West US 3"]`, "", "capability access denied", denied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := databaseRunnerFunc(func(_ context.Context, command Command) ([]byte, error) {
				if strings.Contains(strings.Join(command.Args, " "), "provider show") {
					return []byte(tc.metadata), nil
				}
				return []byte(tc.capabilities), tc.err
			})
			got, err := AvailableDatabaseRegions(t.Context(), runner, "subscription", DatabaseFlexible, []string{"westus3"})
			if got != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unusable discovery produced choices: %v, %v", got, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("lost Azure error: %v", err)
			}
		})
	}
}

func TestAvailableDatabaseRegionsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := databaseRunnerFunc(func(ctx context.Context, command Command) ([]byte, error) {
		if strings.Contains(strings.Join(command.Args, " "), "provider show") {
			return []byte(`["West US 3"]`), nil
		}
		cancel()
		return nil, ctx.Err()
	})
	got, err := AvailableDatabaseRegions(ctx, runner, "subscription", DatabaseFlexible, []string{"westus3"})
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery produced choices: %v, %v", got, err)
	}
}
