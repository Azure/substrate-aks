package installer

import (
	"bytes"
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

// The Substrate release and the Azure Blob object-store plugin revision the
// installer is reviewed against. Bump them together with the contract tests.
const (
	DefaultRepository = "https://github.com/agent-substrate/substrate.git"
	DefaultTag        = "v1.0.0-rc.3"
	DefaultCommit     = "4ee8f66ac237adfc3303086b12275aefc150fd6f"
	// DefaultReleaseImages is where upstream publishes DefaultTag's images.
	DefaultReleaseImages = "ghcr.io/agent-substrate/substrate"

	// The plugin is pinned to an unreleased pull request head until it is tagged.
	DefaultPluginRepository = "https://github.com/agent-substrate-plugins/objectstore-azure-blob.git"
	DefaultPluginCommit     = "b21387e82cee399c65f49178a665727534e1c073"

	// Dsv6 runs on one CPU generation; memory snapshots cannot restore across TSC rates.
	DefaultWorkerVMSize = "Standard_D4s_v6"
	SystemVMSize        = "Standard_D4s_v6"
	// The overlay pod CIDR (10.244.0.0/16) gives each node a /24, so the cluster holds at most 256 nodes.
	MaxWorkerCount = 100
)

type Config struct {
	Installation InstallationConfig `json:"installation"`
	Azure        AzureConfig        `json:"azure"`
	AKS          AKSConfig          `json:"aks"`
	Substrate    SubstrateConfig    `json:"substrate"`
}

type InstallationConfig struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Ownership Ownership `json:"ownership"`
	// Dir is the installation's state directory. It is derived from where
	// the record is stored rather than saved in it.
	Dir string `json:"-"`
}

// Ownership records whether the installer created each Azure resource or was
// given an existing one. Only installer-created resources are supported today;
// teardown deletes the whole resource group, so reused resources need
// different provisioning and teardown paths before they can be accepted.
type Ownership struct {
	ResourceGroup string `json:"resourceGroup"`
	Cluster       string `json:"cluster"`
}

const OwnershipCreated = "created"

type AzureConfig struct {
	SubscriptionID              string `json:"subscriptionId"`
	Location                    string `json:"location"`
	ResourceGroup               string `json:"resourceGroup"`
	AssetPublisherObjectID      string `json:"assetPublisherObjectId"`
	AssetPublisherPrincipalType string `json:"assetPublisherPrincipalType,omitempty"`
	SnapshotReaderObjectID      string `json:"snapshotReaderObjectId,omitempty"`
	SnapshotReaderPrincipalType string `json:"snapshotReaderPrincipalType,omitempty"`
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

// WorkerPoolConfig sizes the microVM pool. AKS fixes OSDiskType, OSDiskSizeGB
// and MaxPods when the pool is created; changing them needs a new installation.
type WorkerPoolConfig struct {
	VMSize string `json:"vmSize"`
	Count  int    `json:"count"`
	// OSDiskType is Managed or Ephemeral, which places the OS disk, and with it
	// /var/lib/ate, on the VM's local storage (NVMe on v6/v7 "d" sizes).
	OSDiskType   string `json:"osDiskType,omitempty"`
	OSDiskSizeGB int    `json:"osDiskSizeGB,omitempty"`
	// MaxPods is the AKS per-node pod limit; 0 keeps the AKS default.
	MaxPods int `json:"maxPods,omitempty"`
}

const (
	DefaultWorkerOSDiskType   = "Managed"
	DefaultWorkerOSDiskSizeGB = 128
)

func (w WorkerPoolConfig) osDiskType() string {
	if w.OSDiskType == "" {
		return DefaultWorkerOSDiskType
	}
	return w.OSDiskType
}

func (w WorkerPoolConfig) osDiskSizeGB() int {
	if w.OSDiskSizeGB == 0 {
		return DefaultWorkerOSDiskSizeGB
	}
	return w.OSDiskSizeGB
}

type SubstrateConfig struct {
	Source       Source `json:"source"`
	PluginSource Source `json:"pluginSource"`
	// ImageSource selects how images bootstraps substrate.images: "release"
	// (the default) imports the published release images, and "source" builds
	// them from substrate.source.
	ImageSource ImageSource   `json:"imageSource,omitempty"`
	Images      ImagesConfig  `json:"images"`
	Runtime     RuntimeConfig `json:"runtime"`
}

type ImageSource string

const (
	ImageSourceRelease ImageSource = "release"
	ImageSourceBuild   ImageSource = "source"
)

// Source pins a Git revision. Tag is optional; when set, the tag must name
// Commit, and release images are looked up by it.
type Source struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Commit     string `json:"commit"`
}

// UnmarshalJSON replaces the whole default pin, so a configured commit never
// inherits the default tag.
func (s *Source) UnmarshalJSON(data []byte) error {
	type plain Source
	var p plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*s = Source(p)
	return nil
}

// EffectiveImageSource is substrate.imageSource, defaulting to release.
func (c Config) EffectiveImageSource() ImageSource {
	if c.Substrate.ImageSource == "" {
		return ImageSourceRelease
	}
	return c.Substrate.ImageSource
}

// Describe names the pinned revision for people: the tag when there is one,
// then the short commit, then the repository when it is not the default.
func (s Source) Describe(defaultRepository string) string {
	text := s.Commit[:min(12, len(s.Commit))]
	if s.Tag != "" {
		text = s.Tag + " (" + text + ")"
	}
	if s.Repository != defaultRepository {
		text += " from " + s.Repository
	}
	return text
}

// NeedsImageBuilder reports whether images still has to build with Docker or
// Podman: only source builds do, and only until images are configured.
func (c Config) NeedsImageBuilder() bool {
	return c.EffectiveImageSource() == ImageSourceBuild && c.Substrate.Images == (ImagesConfig{})
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
		Installation: InstallationConfig{Ownership: Ownership{ResourceGroup: OwnershipCreated, Cluster: OwnershipCreated}},
		AKS: AKSConfig{
			KubernetesVersion: "1.37",
			WorkerPool: WorkerPoolConfig{VMSize: DefaultWorkerVMSize, Count: 1,
				OSDiskType: DefaultWorkerOSDiskType, OSDiskSizeGB: DefaultWorkerOSDiskSizeGB},
		},
		Substrate: SubstrateConfig{
			Source:       Source{Repository: DefaultRepository, Tag: DefaultTag, Commit: DefaultCommit},
			PluginSource: Source{Repository: DefaultPluginRepository, Commit: DefaultPluginCommit},
		},
	}
}

var (
	uuidPattern   = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)
	shaPattern    = regexp.MustCompile(`^[a-f0-9]{40}$`)
	digestPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
	tagPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	// releasePattern is upstream's release tag form, which ate-setup requires
	// of published image tags.
	releasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
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
	c.Installation.Dir = base
	if p := &c.Substrate.Runtime.AssetsDir; *p != "" && !filepath.IsAbs(*p) {
		*p = filepath.Join(base, *p)
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

// ValidateInstallationName validates a cluster or dedicated resource-group name.
func ValidateInstallationName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("must be 3-40 lowercase letters, digits or hyphens, starting with a letter and ending with a letter or digit")
	}
	return nil
}

func (c Config) Validate() error {
	if !uuidPattern.MatchString(c.Installation.ID) || !uuidPattern.MatchString(c.Azure.SubscriptionID) {
		return errors.New("installation.id and azure.subscriptionId must be UUIDs (generate installation.id once and keep it for retries/cleanup)")
	}
	if err := ValidateInstallationName(c.Installation.Name); err != nil {
		return fmt.Errorf("installation.name %w", err)
	}
	if c.Installation.Ownership.ResourceGroup != OwnershipCreated || c.Installation.Ownership.Cluster != OwnershipCreated {
		return errors.New(`installation.ownership must be "created" for both resourceGroup and cluster; existing resource groups and clusters are not supported yet`)
	}
	for _, name := range []string{c.Azure.ResourceGroup, c.AKS.ClusterName} {
		if err := ValidateInstallationName(name); err != nil {
			return fmt.Errorf("azure.resourceGroup and aks.clusterName %w", err)
		}
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9]+$`).MatchString(c.Azure.Location) {
		return errors.New("azure.location must be an Azure region name such as eastus2")
	}
	if c.AKS.KubernetesVersion != "1.37" && !regexp.MustCompile(`^1\.37\.[0-9]+$`).MatchString(c.AKS.KubernetesVersion) {
		return errors.New("this iteration targets Kubernetes 1.37 only")
	}
	if c.AKS.WorkerPool.Count < 1 || c.AKS.WorkerPool.Count > MaxWorkerCount {
		return fmt.Errorf("aks.workerPool.count must be 1-%d", MaxWorkerCount)
	}
	if !regexp.MustCompile(`^Standard_[A-Za-z0-9_]+$`).MatchString(c.AKS.WorkerPool.VMSize) {
		return errors.New("aks.workerPool.vmSize must be an explicit Standard_* Azure SKU")
	}
	if t := c.AKS.WorkerPool.OSDiskType; t != "" && t != "Managed" && t != "Ephemeral" {
		return errors.New("aks.workerPool.osDiskType must be Managed or Ephemeral")
	}
	if size := c.AKS.WorkerPool.OSDiskSizeGB; size != 0 && (size < 30 || size > 2048) {
		return errors.New("aks.workerPool.osDiskSizeGB must be 30-2048")
	}
	if pods := c.AKS.WorkerPool.MaxPods; pods != 0 && (pods < 10 || pods > 250) {
		return errors.New("aks.workerPool.maxPods must be 10-250, or omitted for the AKS default")
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
	if !uuidPattern.MatchString(c.Azure.AssetPublisherObjectID) {
		return errors.New("azure.assetPublisherObjectId must be an Entra object UUID for the runtime asset publisher")
	}
	if c.Azure.SnapshotReaderObjectID != "" && !uuidPattern.MatchString(c.Azure.SnapshotReaderObjectID) {
		return errors.New("azure.snapshotReaderObjectId must be an Entra object UUID or omitted")
	}
	for field, value := range map[string]string{
		"azure.assetPublisherPrincipalType": c.Azure.AssetPublisherPrincipalType,
		"azure.snapshotReaderPrincipalType": c.Azure.SnapshotReaderPrincipalType,
	} {
		if value != "" && value != "User" && value != "ServicePrincipal" && value != "Group" {
			return fmt.Errorf("%s must be User, ServicePrincipal, or Group", field)
		}
	}
	if c.Azure.SnapshotReaderPrincipalType != "" && c.Azure.SnapshotReaderObjectID == "" {
		return errors.New("azure.snapshotReaderPrincipalType requires azure.snapshotReaderObjectId")
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
	if err := c.Substrate.Source.validate("substrate.source"); err != nil {
		return err
	}
	if err := c.Substrate.PluginSource.validate("substrate.pluginSource"); err != nil {
		return err
	}
	switch c.Substrate.ImageSource {
	case "", ImageSourceRelease, ImageSourceBuild:
	default:
		return errors.New(`substrate.imageSource must be "release" or "source"`)
	}
	return nil
}

func (s Source) validate(field string) error {
	u, err := url.Parse(s.Repository)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s.repository must be an HTTPS Git repository URL without credentials, query or fragment", field)
	}
	if !shaPattern.MatchString(s.Commit) {
		return fmt.Errorf("%s.commit must be an exact lowercase 40-character commit SHA, not a branch", field)
	}
	if s.Tag != "" && !releasePattern.MatchString(s.Tag) {
		return fmt.Errorf("%s.tag must be a release tag such as v1.0.0 or v1.0.0-rc.3, or omitted", field)
	}
	return nil
}

func (c Config) publisherPrincipalType() string {
	if c.Azure.AssetPublisherPrincipalType != "" {
		return c.Azure.AssetPublisherPrincipalType
	}
	if operator := c.AKS.Operator; operator != nil && strings.EqualFold(operator.ObjectID, c.Azure.AssetPublisherObjectID) {
		return operator.PrincipalType
	}
	return ""
}

func (c Config) readerPrincipalType() string {
	if c.Azure.SnapshotReaderPrincipalType != "" {
		return c.Azure.SnapshotReaderPrincipalType
	}
	if strings.EqualFold(c.Azure.SnapshotReaderObjectID, c.Azure.AssetPublisherObjectID) {
		return c.publisherPrincipalType()
	}
	if operator := c.AKS.Operator; operator != nil && strings.EqualFold(operator.ObjectID, c.Azure.SnapshotReaderObjectID) {
		return operator.PrincipalType
	}
	return ""
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

func (c Config) workspace() string      { return c.Installation.Dir }
func (c Config) deploymentName() string { return "substrate-" + c.Installation.ID }
