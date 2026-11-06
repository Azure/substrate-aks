package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var errDatabaseRegionUnavailable = errors.New("database profile unavailable")

// AvailableDatabaseRegions filters physical regions by service metadata and the
// subscription's advertised profile. The runner is called concurrently, at most
// four times. Discovery errors fail the entire lookup rather than hiding regions.
func AvailableDatabaseRegions(ctx context.Context, runner Runner, subscription string, backend DatabaseBackend, regions []string) ([]string, error) {
	var namespace string
	switch backend {
	case DatabaseFlexible:
		namespace = "Microsoft.DBforPostgreSQL"
	default:
		return nil, fmt.Errorf("cannot discover regions for database backend %q", backend)
	}
	a := App{Config: Config{Azure: AzureConfig{SubscriptionID: subscription}}, Runner: runner}
	data, err := a.az(ctx, "provider", "show", "--namespace", namespace,
		"--query", "resourceTypes[?resourceType=='locations/capabilities'].locations | [0]")
	if err != nil {
		return nil, fmt.Errorf("loading %s service regions: %w", backend, err)
	}
	var locations []string
	if err := json.Unmarshal(data, &locations); err != nil {
		return nil, fmt.Errorf("reading %s service regions: %w", backend, err)
	}
	if len(locations) == 0 {
		return nil, fmt.Errorf("Azure returned no %s service regions; cannot discover database availability", backend)
	}
	serviceRegions := make(map[string]bool, len(locations))
	for _, location := range locations {
		location = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(location)), " ", "")
		if location == "" {
			return nil, fmt.Errorf("Azure returned an invalid %s service region", backend)
		}
		serviceRegions[location] = true
	}
	var candidates []string
	for _, region := range regions {
		if serviceRegions[region] {
			candidates = append(candidates, region)
			delete(serviceRegions, region)
		}
	}
	valid := make([]bool, len(candidates))
	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, 4)
	var group sync.WaitGroup
	var failure sync.Once
	var discoveryErr error
	for i, region := range candidates {
		group.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-checkCtx.Done():
				return
			}
			defer func() { <-slots }()
			if checkCtx.Err() != nil {
				return
			}
			err := ValidateDatabaseRegion(checkCtx, runner, subscription, region, DatabaseConfig{Backend: backend})
			if err == nil {
				valid[i] = true
			} else if !errors.Is(err, errDatabaseRegionUnavailable) {
				failure.Do(func() {
					discoveryErr = err
					cancel()
				})
			}
		})
	}
	group.Wait()
	if discoveryErr != nil {
		return nil, fmt.Errorf("discovering supported %s regions: %w", backend, discoveryErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var available []string
	for i, region := range candidates {
		if valid[i] {
			available = append(available, region)
		}
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("no %s regions advertise the required PostgreSQL profile for subscription %s; choose another backend or request Azure access", backend, subscription)
	}
	sort.Strings(available)
	return available, nil
}

type databaseCapability struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Restricted bool   `json:"restricted"`
}

func (c databaseCapability) available() bool {
	return !c.Restricted && c.Reason == "" &&
		(c.Status == "" || strings.EqualFold(c.Status, "Available") || strings.EqualFold(c.Status, "Enabled"))
}

// ValidateDatabaseRegion checks advertised subscription capabilities, not live capacity.
// It makes no resource changes and does not resolve database credentials.
func ValidateDatabaseRegion(ctx context.Context, runner Runner, subscription, location string, database DatabaseConfig) error {
	c := Config{Azure: AzureConfig{SubscriptionID: subscription, Location: location}, Database: database}
	if !c.ManagedDatabase() {
		return nil
	}
	a := App{Config: c, Runner: runner}
	var data []byte
	var err error
	switch database.Backend {
	case DatabaseFlexible:
		data, err = a.az(ctx, "postgres", "flexible-server", "list-skus", "--location", c.DatabaseLocation())
		if err == nil {
			err = validateFlexibleCapabilities(data)
		}
	default:
		err = fmt.Errorf("unknown database backend %q", database.Backend)
	}
	if err != nil {
		return fmt.Errorf("%s PostgreSQL is not available or could not be validated in %s for subscription %s: %w; choose a supported database.location or request Azure access to this region; no alternate region was selected",
			database.Backend, c.DatabaseLocation(), subscription, err)
	}
	return nil
}

// CheckDatabaseRegion allows restricted profiles only for a verified existing
// database from this installation's deployment outputs, and reports a warning.
func (a App) CheckDatabaseRegion(ctx context.Context) error {
	availabilityErr := ValidateDatabaseRegion(ctx, a.Runner, a.Config.Azure.SubscriptionID, a.Config.Azure.Location, a.Config.Database)
	if availabilityErr == nil {
		return nil
	}
	if !errors.Is(availabilityErr, errDatabaseRegionUnavailable) {
		return availabilityErr
	}
	i, err := a.infrastructure(ctx)
	if errors.Is(err, errGroupAbsent) {
		return availabilityErr
	}
	if err != nil {
		return errors.Join(availabilityErr, fmt.Errorf("verifying the existing managed database: %w", err))
	}
	data, err := a.az(ctx, "resource", "show", "--ids", i.Database.ResourceID, "--api-version", "2025-08-01",
		"--query", "{id:id,location:location,tags:tags}")
	if err != nil {
		return errors.Join(availabilityErr, fmt.Errorf("verifying the existing managed database: %w", err))
	}
	var resource struct {
		ID       string            `json:"id"`
		Location string            `json:"location"`
		Tags     map[string]string `json:"tags"`
	}
	if err := json.Unmarshal(data, &resource); err != nil {
		return errors.Join(availabilityErr, fmt.Errorf("reading the existing managed database: %w", err))
	}
	if !strings.EqualFold(resource.ID, i.Database.ResourceID) || resource.Location != a.Config.DatabaseLocation() ||
		resource.Tags[ownershipTag] != a.Config.Installation.ID {
		return errors.Join(availabilityErr, errors.New("existing managed database identity, region or ownership does not match this installation"))
	}
	a.progress("Warning: Azure no longer advertises this database profile; continuing with the verified existing %s database in %s. New deployments may still be restricted",
		a.Config.Database.Backend, a.Config.DatabaseLocation())
	return nil
}

func validateFlexibleCapabilities(data []byte) error {
	var capabilities []struct {
		databaseCapability
		Features []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"supportedFeatures"`
		Versions []databaseCapability `json:"supportedServerVersions"`
		Editions []struct {
			databaseCapability
			SKUs []databaseCapability `json:"supportedServerSkus"`
		} `json:"supportedServerEditions"`
	}
	if err := json.Unmarshal(data, &capabilities); err != nil {
		return fmt.Errorf("reading Flexible Server capabilities: %w", err)
	}
	if capabilities == nil {
		return errors.New("reading Flexible Server capabilities: Azure returned null")
	}
	for _, capability := range capabilities {
		if capability.Reason != "" {
			return fmt.Errorf("%w: Azure restriction: %s", errDatabaseRegionUnavailable, capability.Reason)
		}
		if !capability.available() {
			continue
		}
		for _, feature := range capability.Features {
			if strings.EqualFold(feature.Name, "OfferRestricted") && strings.EqualFold(feature.Status, "Enabled") {
				return fmt.Errorf("%w: Azure reports OfferRestricted for this subscription", errDatabaseRegionUnavailable)
			}
		}
		versionSupported := false
		for _, version := range capability.Versions {
			if version.Name == "17" && version.available() {
				versionSupported = true
			}
		}
		if !versionSupported {
			continue
		}
		for _, edition := range capability.Editions {
			if edition.Name != "GeneralPurpose" || !edition.available() {
				continue
			}
			for _, sku := range edition.SKUs {
				if sku.Name == "Standard_D2ads_v5" && sku.available() {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("%w: Azure does not advertise the required PostgreSQL 17 GeneralPurpose Standard_D2ads_v5 profile", errDatabaseRegionUnavailable)
}
