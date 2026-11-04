package installer

import (
	"encoding/json"
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

func TestDefaultHomePerPlatform(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	for _, tc := range []struct {
		goos string
		vars map[string]string
		want string
	}{
		{"linux", nil, "/home/u/.local/state/substrate-aks"},
		{"linux", map[string]string{"XDG_STATE_HOME": "/xdg/state"}, "/xdg/state/substrate-aks"},
		{"linux", map[string]string{"XDG_STATE_HOME": "relative"}, "/home/u/.local/state/substrate-aks"},
		{"darwin", map[string]string{"XDG_STATE_HOME": "/xdg/state"}, "/home/u/Library/Application Support/substrate-aks"},
	} {
		got := defaultHome(tc.goos, env(tc.vars), "/home/u")
		if got != filepath.FromSlash(tc.want) {
			t.Errorf("defaultHome(%s, %v) = %q; want %q", tc.goos, tc.vars, got, tc.want)
		}
	}
}

func TestDiscardNewInstallationKeepsResumedState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resume  func(t *testing.T, c Config, path string)
		removed bool
	}{
		{"untouched", func(*testing.T, Config, string) {}, true},
		{"another run logged to it", func(t *testing.T, c Config, _ string) {
			log, err := NewLog(c)
			if err != nil {
				t.Fatal(err)
			}
			log.Close()
		}, false},
		{"record changed", func(t *testing.T, c Config, path string) {
			c.Substrate.Images.Tag = "resumed"
			c.Installation.Dir = ""
			data, err := json.MarshalIndent(c, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"lock held", func(t *testing.T, c Config, _ string) {
			lock, err := Lock(c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path, err := CreateInstallation(home, testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			record, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c, err := LoadInstallation(home, testConfig(t).Installation.Name)
			if err != nil {
				t.Fatal(err)
			}
			tc.resume(t, c, path)
			err = DiscardNewInstallation(c, record)
			_, statErr := os.Stat(c.workspace())
			if tc.removed {
				if err != nil || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("fresh installation was not discarded: %v, %v", err, statErr)
				}
				return
			}
			if !errors.Is(err, ErrInstallationInUse) || statErr != nil {
				t.Fatalf("discard removed or misreported a resumed installation: %v, %v", err, statErr)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("resumed installation record removed: %v", err)
			}
		})
	}
}
