package installer

import (
	"errors"
	"strings"

	"go.yaml.in/yaml/v3"
)

func validateKubeconfig(data []byte, expected string) error {
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
			userFound = userFound || (user.Name == context.Context.User && len(user.User) != 0)
		}
		if clusterFound && userFound {
			return nil
		}
	}
	return invalid
}
