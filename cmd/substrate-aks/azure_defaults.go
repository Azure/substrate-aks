package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Azure/substrate-aks/internal/installer"
)

type azureDefaults struct {
	subscription, subscriptionName string
	publisher, publisherName       string
}

type azureAccount struct {
	ID, Name, TenantID string
	User               struct{ Name, Type string }
}

const discoveryTimeout = 30 * time.Second

func discoverAzureDefaults(ctx context.Context, subscription, publisher string, runner installer.Runner) (azureDefaults, error) {
	d := azureDefaults{subscription: subscription, publisher: publisher}
	if subscription != "" && publisher != "" {
		return d, nil
	}
	active, err := readAzureAccount(ctx, "", runner)
	if err != nil {
		return d, err
	}
	account := active
	if subscription != "" && !strings.EqualFold(active.ID, subscription) {
		account, err = readAzureAccount(ctx, subscription, runner)
		if err != nil {
			return d, err
		}
	}
	if account.ID == "" || (subscription != "" && !strings.EqualFold(account.ID, subscription)) {
		return d, errors.New("Azure CLI returned a missing or unexpected subscription ID; select an account with az account set or supply explicit subscription and Azure identity object IDs")
	}
	d.subscription, d.subscriptionName = account.ID, account.Name
	if publisher != "" {
		return d, nil
	}
	// Directory commands use the active CLI identity, not --subscription.
	if account.ID != active.ID && (account.TenantID == "" || active.TenantID == "" ||
		!strings.EqualFold(account.TenantID, active.TenantID) ||
		account.User.Name == "" || !strings.EqualFold(account.User.Name, active.User.Name) ||
		!strings.EqualFold(account.User.Type, active.User.Type)) {
		return d, errors.New("selected subscription uses a different or unknown tenant/identity; run az account set --subscription for that target first, or provide --identity-object-id from its tenant")
	}
	var identityArgs []string
	switch strings.ToLower(account.User.Type) {
	case "user":
		identityArgs = []string{"az", "ad", "signed-in-user", "show"}
	case "serviceprincipal":
		if account.User.Name != "" {
			identityArgs = []string{"az", "ad", "sp", "show", "--id", account.User.Name}
		}
	}
	if len(identityArgs) == 0 {
		return d, fmt.Errorf("cannot discover the Azure identity for CLI identity type %q; provide --identity-object-id with its Entra object ID", account.User.Type)
	}
	identityArgs = append(identityArgs, "--only-show-errors", "--output", "json")
	probeCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	data, err := runner.Run(probeCtx, installer.Command{Args: identityArgs})
	if err != nil {
		return d, fmt.Errorf("discovering Azure identity Entra object ID (directory access may be restricted; provide --identity-object-id explicitly): %w", err)
	}
	var identity struct{ ID, DisplayName string }
	if err := json.Unmarshal(data, &identity); err != nil {
		return d, fmt.Errorf("reading Azure identity; provide --identity-object-id explicitly: %w", err)
	}
	if identity.ID == "" {
		return d, errors.New("Azure CLI did not return the identity's Entra object ID; provide --identity-object-id explicitly, not the application/client ID")
	}
	d.publisher, d.publisherName = identity.ID, identity.DisplayName
	return d, nil
}

func readAzureAccount(ctx context.Context, subscription string, runner installer.Runner) (azureAccount, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	var account azureAccount
	args := []string{"az", "account", "show", "--only-show-errors", "--output", "json"}
	if subscription != "" {
		args = append(args, "--subscription", subscription)
	}
	data, err := runner.Run(ctx, installer.Command{Args: args})
	if err != nil {
		return account, fmt.Errorf("discovering Azure subscription (sign in with az login, select an account with az account set, or provide --subscription and --identity-object-id): %w", err)
	}
	if err := json.Unmarshal(data, &account); err != nil {
		return account, fmt.Errorf("reading Azure CLI account: %w", err)
	}
	return account, nil
}

func printAzureDefaults(out io.Writer, d azureDefaults) {
	fmt.Fprintf(out, "Subscription: %s\nAzure identity: %s\n",
		formatAzureID(d.subscriptionName, d.subscription), formatAzureID(d.publisherName, d.publisher))
}

func formatAzureID(name, id string) string {
	if name == "" {
		return id
	}
	return name + " (" + id + ")"
}
