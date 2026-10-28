package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/substrate-aks/internal/installer"
)

func readAzureRegions(ctx context.Context, subscription string, runner installer.Runner) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	data, err := runner.Run(ctx, installer.Command{Args: []string{
		"az", "rest", "--method", "get",
		"--url", "https://management.azure.com/subscriptions/" + subscription + "/locations?api-version=2022-12-01",
		"--query", "value[?metadata.regionType=='Physical'].name", "--only-show-errors", "--output", "json",
	}})
	if err != nil {
		return nil, fmt.Errorf("loading Azure regions for the selected subscription: %w", err)
	}
	var regions []string
	if err := json.Unmarshal(data, &regions); err != nil {
		return nil, fmt.Errorf("reading Azure region list: %w", err)
	}
	if len(regions) == 0 {
		return nil, errors.New("Azure returned no physical regions; cannot validate the region name")
	}
	for _, region := range regions {
		if region == "" || region != strings.ToLower(strings.TrimSpace(region)) {
			return nil, errors.New("Azure returned an invalid region name; cannot validate the region list")
		}
	}
	sort.Strings(regions)
	return regions, nil
}

func validateAzureRegion(value string, regions []string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	var suggestions []string
	for _, region := range regions {
		if value == region {
			return region, nil
		}
		if len(value) >= 3 && strings.HasPrefix(region, value) {
			suggestions = append(suggestions, region)
		}
	}
	if len(suggestions) > 0 {
		sort.Strings(suggestions)
		return "", fmt.Errorf("unknown Azure region %q; possible matches: %s", value, strings.Join(suggestions[:min(3, len(suggestions))], ", "))
	}
	return "", fmt.Errorf("unknown Azure region %q; use a region name from az account list-locations (for example eastus2)", value)
}
