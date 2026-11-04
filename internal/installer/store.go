package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// HomeEnv overrides where installations are stored.
const HomeEnv = "SUBSTRATE_AKS_HOME"

const (
	recordFile = "installation.json"
	lockFile   = "operation.lock"
)

// ErrInstallationInUse reports that another command holds an installation's
// lock or, for DiscardNewInstallation, has written to it.
var ErrInstallationInUse = errors.New("another operation is using this installation")

// Home returns the directory holding all installations: $SUBSTRATE_AKS_HOME,
// or the platform's per-user state directory. Installations hold credentials
// and the only local link to billable resources, so they must not live in a
// cache directory that cleaners treat as disposable.
func Home() (string, error) {
	if home := os.Getenv(HomeEnv); home != "" {
		return filepath.Abs(home)
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory for installation state (set %s to choose one): %w", HomeEnv, err)
	}
	return defaultHome(runtime.GOOS, os.Getenv, user), nil
}

// defaultHome returns the per-user state directory for goos.
func defaultHome(goos string, getenv func(string) string, user string) string {
	switch goos {
	case "darwin":
		return filepath.Join(user, "Library", "Application Support", "substrate-aks")
	default:
		// XDG Base Directory spec: ignore relative values.
		if dir := getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
			return filepath.Join(dir, "substrate-aks")
		}
		return filepath.Join(user, ".local", "state", "substrate-aks")
	}
}

// RecordPath returns where an installation's record is stored.
func RecordPath(home, name string) string {
	return filepath.Join(home, "installations", name, recordFile)
}

// ErrNoInstallation reports a missing installation record.
var ErrNoInstallation = errors.New("installation not found")

// LoadInstallation reads a saved installation by name.
func LoadInstallation(home, name string) (Config, error) {
	if err := ValidateInstallationName(name); err != nil {
		return Config{}, fmt.Errorf("installation name %q %w", name, err)
	}
	path := RecordPath(home, name)
	c, err := LoadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%w: %s (run %q to list installations)", ErrNoInstallation, name, "substrate-aks list")
	}
	if err != nil {
		return Config{}, err
	}
	if c.Installation.Name != name {
		return Config{}, fmt.Errorf("%s records installation %q, not %q", path, c.Installation.Name, name)
	}
	return c, nil
}

// ListInstallations returns the names of saved installations in order.
func ListInstallations(home string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(home, "installations"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || ValidateInstallationName(e.Name()) != nil {
			continue
		}
		if _, err := os.Stat(RecordPath(home, e.Name())); err == nil {
			names = append(names, e.Name())
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

// ResolveInstallation returns name, or the only saved installation when name
// is empty.
func ResolveInstallation(home, name string) (string, error) {
	if name != "" {
		return name, nil
	}
	names, err := ListInstallations(home)
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", errors.New("no installations found; run setup to create one")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("multiple installations found; choose one of: %s", strings.Join(names, ", "))
	}
}

// CreateInstallation saves a new installation record, refusing to replace an
// existing one.
func CreateInstallation(home string, c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	path := RecordPath(home, c.Installation.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	c.Installation.Dir = ""
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("installation %q already exists; run setup %s to resume it: %w", c.Installation.Name, c.Installation.Name, err)
	}
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(append(data, '\n'))
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// RemoveInstallation deletes an installation's record and local state. Call
// it only after its Azure resources are gone.
func RemoveInstallation(c Config) error {
	if c.Installation.Dir == "" || filepath.Base(c.Installation.Dir) != c.Installation.Name {
		return errors.New("refusing to remove local state outside the installation directory")
	}
	return os.RemoveAll(c.Installation.Dir)
}

// DiscardNewInstallation removes an installation this process created but
// never provisioned. It returns ErrInstallationInUse, keeping the state, when
// another command holds the installation's lock or has written to it since
// record was saved, such as a setup run that resumed it.
func DiscardNewInstallation(c Config, record []byte) error {
	lock, err := Lock(c)
	if err != nil {
		return err
	}
	defer lock.Close()
	entries, err := os.ReadDir(c.workspace())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != recordFile && entry.Name() != lockFile {
			return ErrInstallationInUse
		}
	}
	current, err := os.ReadFile(filepath.Join(c.workspace(), recordFile))
	if err != nil {
		return err
	}
	if !bytes.Equal(current, record) {
		return ErrInstallationInUse
	}
	return RemoveInstallation(c)
}
