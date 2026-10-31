package installer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallationStore(t *testing.T) {
	home := t.TempDir()
	if _, err := ResolveInstallation(home, ""); err == nil || !strings.Contains(err.Error(), "no installations") {
		t.Fatalf("empty store resolved: %v", err)
	}
	first := testConfig(t)
	first.Installation.Name = "substrate-one"
	path, err := CreateInstallation(home, first)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "installations", "substrate-one", "installation.json") {
		t.Fatalf("record path %s", path)
	}
	if data, err := os.ReadFile(path); err != nil || strings.Contains(string(data), first.Installation.Dir) {
		t.Fatalf("record must not persist its directory: %s, %v", data, err)
	}
	if _, err := CreateInstallation(home, first); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing installation replaced: %v", err)
	}
	if name, err := ResolveInstallation(home, ""); err != nil || name != "substrate-one" {
		t.Fatalf("single installation not selected: %q, %v", name, err)
	}
	loaded, err := LoadInstallation(home, "substrate-one")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Installation.Dir != filepath.Dir(path) || loaded.workspace() != filepath.Dir(path) || loaded.Installation.ID != first.Installation.ID {
		t.Fatalf("loaded installation %+v", loaded.Installation)
	}

	second := testConfig(t)
	second.Installation.Name = "substrate-two"
	if _, err := CreateInstallation(home, second); err != nil {
		t.Fatal(err)
	}
	if names, err := ListInstallations(home); err != nil || strings.Join(names, ",") != "substrate-one,substrate-two" {
		t.Fatalf("listed %v, %v", names, err)
	}
	if _, err := ResolveInstallation(home, ""); err == nil || !strings.Contains(err.Error(), "substrate-one, substrate-two") {
		t.Fatalf("ambiguous selection allowed: %v", err)
	}
	if _, err := LoadInstallation(home, "substrate-missing"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("missing installation: %v", err)
	}

	// A record copied under another name must not be used as that name.
	if err := os.MkdirAll(filepath.Join(home, "installations", "substrate-copy"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RecordPath(home, "substrate-copy"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstallation(home, "substrate-copy"); err == nil || !strings.Contains(err.Error(), `records installation "substrate-one"`) {
		t.Fatalf("mismatched record accepted: %v", err)
	}

	if err := RemoveInstallation(loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installation directory retained: %v", err)
	}
	outside := loaded
	outside.Installation.Dir = home
	if err := RemoveInstallation(outside); err == nil {
		t.Fatal("removed a directory that is not the installation's")
	}
}

func TestOwnershipOnlyAcceptsCreatedResources(t *testing.T) {
	for _, ownership := range []Ownership{
		{ResourceGroup: "existing", Cluster: OwnershipCreated},
		{ResourceGroup: OwnershipCreated, Cluster: "existing"},
		{},
	} {
		c := testConfig(t)
		c.Installation.Ownership = ownership
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "not supported yet") {
			t.Fatalf("ownership %+v accepted: %v", ownership, err)
		}
	}
}
