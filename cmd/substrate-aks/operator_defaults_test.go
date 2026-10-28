package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/substrate-aks/internal/installer"
)

func TestOperatorDefaultsNeverInheritPublisherOverride(t *testing.T) {
	const publisher = "77777777-7777-4777-8777-777777777777"
	for _, kind := range []string{"user", "servicePrincipal"} {
		t.Run(kind, func(t *testing.T) {
			r := defaultsRunner(func(_ context.Context, c installer.Command) ([]byte, error) {
				if c.Args[1] == "account" {
					return []byte(accountJSON(kind)), nil
				}
				if kind == "servicePrincipal" && !strings.Contains(strings.Join(c.Args, " "), "ad sp show --id") {
					t.Fatal("service principal object ID lookup missing")
				}
				return []byte(`{"id":"` + defaultPublisher + `"}`), nil
			})
			d, err := discoverInstallationDefaults(t.Context(), defaultSubscription, publisher, "", "", r)
			wantType := "User"
			if kind == "servicePrincipal" {
				wantType = "ServicePrincipal"
			}
			if err != nil || d.publisher != publisher || d.operator.ObjectID != defaultPublisher || d.operator.PrincipalType != wantType {
				t.Fatalf("operator incorrectly inherited publisher or lost principal type: %+v, %v", d, err)
			}
		})
	}
}

func TestExplicitInstallationIdentitiesAvoidDirectoryAccess(t *testing.T) {
	d, err := discoverInstallationDefaults(t.Context(), defaultSubscription, defaultPublisher,
		"77777777-7777-4777-8777-777777777777", "Group",
		defaultsRunner(func(context.Context, installer.Command) ([]byte, error) {
			t.Fatal("explicit subscription and identities made a discovery call")
			return nil, nil
		}))
	if err != nil || d.operator.PrincipalType != "Group" || d.operator.ObjectID == d.publisher {
		t.Fatalf("explicit operator settings were not preserved: %+v, %v", d, err)
	}
}
