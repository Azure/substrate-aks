package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	DefaultRepository = "https://github.com/aramase/substrate.git"
	DefaultCommit     = "c3b44aa46312e073a6384f68723a7b14dffd5518"
)

type Config struct {
	Installation InstallationConfig `json:"installation"`
	Azure        AzureConfig        `json:"azure"`
	AKS          AKSConfig          `json:"aks"`
	Substrate    SubstrateConfig    `json:"substrate"`
}

type InstallationConfig struct {
	ID       string `json:"id"`
	StateDir string `json:"stateDir"`
}

type AzureConfig struct {
	SubscriptionID         string `json:"subscriptionId"`
	Location               string `json:"location"`
	ResourceGroup          string `json:"resourceGroup"`
	AssetPublisherObjectID string `json:"assetPublisherObjectId"`
	SnapshotReaderObjectID string `json:"snapshotReaderObjectId,omitempty"`
}

type AKSConfig struct {
	ClusterName          string           `json:"clusterName"`
	KubernetesVersion    string           `json:"kubernetesVersion"`
	APIAuthorizedCIDRs   []string         `json:"apiAuthorizedCidrs"`
	AllowUnrestrictedAPI bool             `json:"allowUnrestrictedApi,omitempty"`
	WorkerPool           WorkerPoolConfig `json:"workerPool"`
	Operator             *OperatorConfig  `json:"operator,omitempty"`
}

type OperatorConfig struct {
	ObjectID      string `json:"objectId"`
	PrincipalType string `json:"principalType"`
}

type WorkerPoolConfig struct {
	VMSize string `json:"vmSize"`
	Count  int    `json:"count"`
}

type SubstrateConfig struct {
	Source  Source        `json:"source"`
	Images  ImagesConfig  `json:"images"`
	Runtime RuntimeConfig `json:"runtime"`
}

type Source struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

type ImagesConfig struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Plugin     string `json:"plugin"`
}

type RuntimeConfig struct {
	AssetsDir string `json:"assetsDir"`
}

func DefaultConfig() Config {
	return Config{
		Installation: InstallationConfig{StateDir: ".substrate-aks"},
		AKS: AKSConfig{
			KubernetesVersion: "1.37",
			WorkerPool:        WorkerPoolConfig{VMSize: "Standard_D4s_v3", Count: 1},
		},
		Substrate: SubstrateConfig{
			Source: Source{Repository: DefaultRepository, Commit: DefaultCommit},
		},
	}
}

var (
	uuidPattern   = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,39}$`)
	shaPattern    = regexp.MustCompile(`^[a-f0-9]{40}$`)
	digestPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
	tagPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
)

func LoadConfig(path string) (Config, error) {
	c, err := readConfig(path)
	if err != nil {
		return Config{}, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	for _, p := range []*string{&c.Installation.StateDir, &c.Substrate.Runtime.AssetsDir} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, nil
}

func readConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	c := DefaultConfig()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s must contain exactly one JSON object", path)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if !uuidPattern.MatchString(c.Installation.ID) || !uuidPattern.MatchString(c.Azure.SubscriptionID) {
		return errors.New("installation.id and azure.subscriptionId must be UUIDs (generate installation.id once and keep it for retries/cleanup)")
	}
	if !namePattern.MatchString(c.Azure.ResourceGroup) || !namePattern.MatchString(c.AKS.ClusterName) {
		return errors.New("azure.resourceGroup and aks.clusterName must be 3-40 lowercase letters, digits or hyphens, starting with a letter")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9]+$`).MatchString(c.Azure.Location) {
		return errors.New("azure.location must be an Azure region name such as eastus2")
	}
	if c.AKS.KubernetesVersion != "1.37" && !regexp.MustCompile(`^1\.37\.[0-9]+$`).MatchString(c.AKS.KubernetesVersion) {
		return errors.New("this iteration targets Kubernetes 1.37 only")
	}
	if c.AKS.WorkerPool.Count < 1 || c.AKS.WorkerPool.Count > 10 || !regexp.MustCompile(`^Standard_[A-Za-z0-9_]+$`).MatchString(c.AKS.WorkerPool.VMSize) {
		return errors.New("aks.workerPool.count must be 1-10 and aks.workerPool.vmSize must be an explicit Standard_* Azure SKU")
	}
	if len(c.AKS.APIAuthorizedCIDRs) == 0 && !c.AKS.AllowUnrestrictedAPI {
		return errors.New("aks.apiAuthorizedCidrs must allow your client/CI IP ranges; only explicit aks.allowUnrestrictedApi=true permits an empty allowlist")
	}
	if len(c.AKS.APIAuthorizedCIDRs) > 0 && c.AKS.AllowUnrestrictedAPI {
		return errors.New("aks.allowUnrestrictedApi cannot be combined with aks.apiAuthorizedCidrs")
	}
	for _, value := range c.AKS.APIAuthorizedCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 {
			return fmt.Errorf("invalid or unrestricted AKS API IPv4 CIDR %q", value)
		}
	}
	if c.Installation.StateDir == "" {
		return errors.New("installation.stateDir must not be empty")
	}
	if !uuidPattern.MatchString(c.Azure.AssetPublisherObjectID) {
		return errors.New("azure.assetPublisherObjectId must be the Entra object ID of the operator or service principal uploading runtime assets")
	}
	if c.Azure.SnapshotReaderObjectID != "" && !uuidPattern.MatchString(c.Azure.SnapshotReaderObjectID) {
		return errors.New("azure.snapshotReaderObjectId must be an Entra object UUID or omitted")
	}
	if operator := c.AKS.Operator; operator != nil {
		if !uuidPattern.MatchString(operator.ObjectID) {
			return errors.New("aks.operator.objectId must be an Entra object UUID, not an application/client ID")
		}
		switch operator.PrincipalType {
		case "User", "ServicePrincipal", "Group":
		default:
			return errors.New("aks.operator.principalType must be User, ServicePrincipal, or Group")
		}
	}
	u, err := url.Parse(c.Substrate.Source.Repository)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("substrate.source.repository must be an HTTPS Git repository URL without credentials, query or fragment")
	}
	if !shaPattern.MatchString(c.Substrate.Source.Commit) {
		return errors.New("substrate.source.commit must be an exact lowercase 40-character commit SHA, not a branch")
	}
	return nil
}

func (c Config) ValidateInstall() error {
	s := c.Substrate.Images
	if s.Repository == "" || strings.ContainsAny(s.Repository, " \t\r\n@") || strings.Contains(s.Repository, "://") || strings.HasPrefix(s.Repository, "-") {
		return errors.New("substrate.images.repository must name a prebuilt image repository compatible with the selected commit")
	}
	if !tagPattern.MatchString(s.Tag) {
		return errors.New("substrate.images.tag must be a Kubernetes-label-compatible tag (at most 63 characters)")
	}
	if !digestPattern.MatchString(s.Plugin) {
		return errors.New("substrate.images.plugin must be an immutable registry image@sha256:<64 hex characters>")
	}
	return nil
}

func (c Config) workspace() string      { return filepath.Join(c.Installation.StateDir, c.Installation.ID) }
func (c Config) deploymentName() string { return "substrate-" + c.Installation.ID }
