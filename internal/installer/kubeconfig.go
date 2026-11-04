package installer

import (
	"errors"
	"strings"

	"go.yaml.in/yaml/v3"
)

func validateEntraKubeconfig(data []byte, expected string) error {
	return validateKubeconfigAuth(data, expected, true)
}

func validateKubeconfig(data []byte, expected string) error {
	return validateKubeconfigAuth(data, expected, false)
}

func validateKubeconfigAuth(data []byte, expected string, entra bool) error {
	var config struct {
		APIVersion     string `yaml:"apiVersion"`
		Kind           string `yaml:"kind"`
		CurrentContext string `yaml:"current-context"`
		Contexts       []struct {
			Name    string `yaml:"name"`
			Context struct {
				Cluster string `yaml:"cluster"`
				User    string `yaml:"user"`
			} `yaml:"context"`
		} `yaml:"contexts"`
		Clusters []struct {
			Name    string `yaml:"name"`
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			Name string         `yaml:"name"`
			User map[string]any `yaml:"user"`
		} `yaml:"users"`
	}
	invalid := errors.New("AKS returned an invalid or incomplete kubeconfig; existing credentials were preserved")
	if err := yaml.Unmarshal(data, &config); err != nil ||
		config.APIVersion != "v1" || config.Kind != "Config" || config.CurrentContext != expected {
		return invalid
	}
	for _, context := range config.Contexts {
		if context.Name != expected || context.Context.Cluster == "" || context.Context.User == "" {
			continue
		}
		clusterFound, userFound := false, false
		for _, cluster := range config.Clusters {
			clusterFound = clusterFound || (cluster.Name == context.Context.Cluster && strings.HasPrefix(cluster.Cluster.Server, "https://"))
		}
		for _, user := range config.Users {
			if user.Name != context.Context.User || len(user.User) == 0 {
				continue
			}
			if !entra {
				userFound = true
				continue
			}
			exec, ok := user.User["exec"].(map[string]any)
			args, argsOK := exec["args"].([]any)
			azureCLI := false
			for i := 0; i+1 < len(args); i++ {
				if (args[i] == "--login" || args[i] == "-l") && args[i+1] == "azurecli" {
					azureCLI = true
				}
			}
			userFound = ok && argsOK && len(args) > 0 && args[0] == "get-token" &&
				exec["command"] == "kubelogin" && azureCLI && len(user.User) == 1
		}
		if clusterFound && userFound {
			return nil
		}
	}
	return invalid
}
