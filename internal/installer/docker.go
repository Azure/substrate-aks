package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func dockerConfigHome() (string, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".docker")
	}
	return filepath.Abs(dir)
}

func (a App) prepareDockerConfig() (string, error) {
	source, err := dockerConfigHome()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(a.Config.workspace(), "docker")
	if err := os.MkdirAll(dest, 0700); err != nil {
		return "", err
	}
	if source == dest {
		return dest, nil
	}
	var settings struct {
		CurrentContext      string   `json:"currentContext"`
		CLIPluginsExtraDirs []string `json:"cliPluginsExtraDirs"`
	}
	data, err := os.ReadFile(filepath.Join(source, "config.json"))
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return "", errors.New("cannot read Docker context/plugin settings from the caller's config.json")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading Docker configuration: %w", err)
	}
	config := map[string]json.RawMessage{}
	path := filepath.Join(dest, "config.json")
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &config); err != nil || config == nil {
			return "", errors.New("isolated Docker config.json is invalid; refusing to overwrite its credentials")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	delete(config, "currentContext")
	if settings.CurrentContext != "" {
		config["currentContext"], err = json.Marshal(settings.CurrentContext)
		if err != nil {
			return "", err
		}
	}
	dirs := append(settings.CLIPluginsExtraDirs, filepath.Join(source, "cli-plugins"))
	for i := range dirs {
		dirs[i], err = filepath.Abs(dirs[i])
		if err != nil {
			return "", err
		}
	}
	config["cliPluginsExtraDirs"], err = json.Marshal(dirs)
	if err != nil {
		return "", err
	}
	contexts := filepath.Join(source, "contexts")
	link := filepath.Join(dest, "contexts")
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("isolated Docker contexts already exist at %s; move that context store aside before reusing the caller's contexts", link)
		}
		if err := os.Remove(link); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(contexts); err == nil {
		if err := os.Symlink(contexts, link); err != nil {
			return "", fmt.Errorf("preserving Docker contexts: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return dest, writeRegistryConfig(dest, config)
}

func writeRegistryConfig(dir string, config map[string]json.RawMessage) error {
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, "config.json"))
}

func dockerBuildEnvironment(dir string) ([]string, error) {
	buildx := os.Getenv("BUILDX_CONFIG")
	if buildx == "" {
		source, err := dockerConfigHome()
		if err != nil {
			return nil, err
		}
		buildx = filepath.Join(source, "buildx")
	}
	buildx, err := filepath.Abs(buildx)
	if err != nil {
		return nil, err
	}
	return []string{"DOCKER_CONFIG=" + dir, "BUILDX_CONFIG=" + buildx, "NO_DEV_ENV=1"}, nil
}
