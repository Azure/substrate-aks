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
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The Substrate release and the Azure Blob object-store plugin revision the
// installer is reviewed against. Bump them together with the contract tests.
const (
	DefaultRepository = "https://github.com/agent-substrate/substrate.git"
	DefaultTag        = "v1.0.0"
	DefaultCommit     = "b34fbee742906494b5eb4f1f36987176bf8770a4"
	// DefaultReleaseImages is where upstream publishes DefaultTag's images.
	DefaultReleaseImages = "ghcr.io/agent-substrate/substrate"

	// The plugin is pinned to a commit on its main branch.
	DefaultPluginRepository = "https://github.com/agent-substrate-plugins/objectstore-azure-blob.git"
	DefaultPluginCommit     = "5639af04ad429cf51d2dbf452c80709d3b68e48b"

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
	Database     DatabaseConfig     `json:"database"`
}

type DatabaseBackend string

const (
	DatabaseBundled  DatabaseBackend = "bundled"
	DatabaseFlexible DatabaseBackend = "flexible-server"
)

type DatabaseConfig struct {
	Backend  DatabaseBackend `json:"backend"`
	Location string          `json:"location,omitempty"`
	// SKU is the Flexible Server compute SKU; it can't change after creation.
	SKU DatabaseSKU `json:"sku,omitzero"`
}

// DatabaseSKU names a Flexible Server compute SKU and its tier, as ARM does.
type DatabaseSKU struct {
	Name string `json:"name"`
	Tier string `json:"tier"`
}

// DefaultDatabaseSKU is used when a managed database record does not choose a SKU.
var DefaultDatabaseSKU = DatabaseSKU{Name: "Standard_D2ads_v5", Tier: "GeneralPurpose"}

// ComputeSKU returns the configured SKU, or the default when none is set.
func (d DatabaseConfig) ComputeSKU() DatabaseSKU {
	if d.SKU == (DatabaseSKU{}) {
		return DefaultDatabaseSKU
	}
	return d.SKU
}

// WithDefaults fills the SKU of a managed database that doesn't choose one.
func (d DatabaseConfig) WithDefaults() DatabaseConfig {
	if d.Backend != "" && d.Backend != DatabaseBundled {
		d.SKU = d.ComputeSKU()
	}
	return d
}

func (c Config) ManagedDatabase() bool {
	return c.Database.Backend != "" && c.Database.Backend != DatabaseBundled
}

func (c Config) DatabaseLocation() string {
	if c.Database.Location != "" {
		return c.Database.Location
	}
	return c.Azure.Location
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
	// Azure caps ephemeral OS disks at 2,040 GiB, below the managed limit.
	MaxEphemeralOSDiskSizeGB = 2040
	// AKSDefaultMaxPods is the pod limit AKS applies with Azure CNI overlay.
	AKSDefaultMaxPods = 250
	// MinPoolPods is the fewest pods AKS lets a node pool hold in total.
	MinPoolPods = 30
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
	Sizing      *SizingConfig `json:"sizing,omitempty"`
	Atelet      *AteletConfig `json:"atelet,omitempty"`
}

type SizingConfig struct {
	APIServer *APIServerSizing `json:"apiServer,omitempty"`
	Router    *RouterSizing    `json:"router,omitempty"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type APIServerSizing struct {
	Replicas             *int                  `json:"replicas,omitempty"`
	Resources            *ResourceRequirements `json:"resources,omitempty"`
	PostgresPoolMaxConns *int                  `json:"postgresPoolMaxConns,omitempty"`
}

type RouterSizing struct {
	Replicas  *int             `json:"replicas,omitempty"`
	Resources *RouterResources `json:"resources,omitempty"`
}

type RouterResources struct {
	AtenetRouter *ResourceRequirements `json:"atenetRouter,omitempty"`
	Envoy        *ResourceRequirements `json:"envoy,omitempty"`
}

type sizedContainer struct {
	name, field, identity string
	resources             *ResourceRequirements
}

type sizedWorkload struct {
	name, field string
	replicas    *int
	pool        *int
	containers  []sizedContainer
}

func (s SubstrateConfig) sizedWorkloads() [2]sizedWorkload {
	api := sizedWorkload{
		name: "ate-api-server", field: "apiServer",
		containers: []sizedContainer{{name: "ate-api-server", field: "apiServer.resources", identity: "apiServer"}},
	}
	router := sizedWorkload{
		name: "atenet-router", field: "router",
		containers: []sizedContainer{
			{name: "atenet-router", field: "router.resources.atenetRouter", identity: "router.atenetRouter"},
			{name: "envoy", field: "router.resources.envoy", identity: "router.envoy"},
		},
	}
	if s.Sizing != nil {
		if value := s.Sizing.APIServer; value != nil {
			api.replicas, api.pool = value.Replicas, value.PostgresPoolMaxConns
			api.containers[0].resources = value.Resources
		}
		if value := s.Sizing.Router; value != nil {
			router.replicas = value.Replicas
			if value.Resources != nil {
				router.containers[0].resources = value.Resources.AtenetRouter
				router.containers[1].resources = value.Resources.Envoy
			}
		}
	}
	return [2]sizedWorkload{api, router}
}

type AteletConfig struct {
	SnapshotUploadDelay *string `json:"snapshotUploadDelay,omitempty"`
	SnapshotDetachDelay *string `json:"snapshotDetachDelay,omitempty"`
}

const (
	// Defaults at the release pin, used only to validate an omitted peer.
	// https://github.com/agent-substrate/substrate/blob/b34fbee742906494b5eb4f1f36987176bf8770a4/cmd/atelet/internal/snapshottracker/background.go
	defaultSnapshotUploadDelay = 5 * time.Minute
	defaultSnapshotDetachDelay = 30 * time.Minute
	// https://github.com/agent-substrate/substrate/blob/b34fbee742906494b5eb4f1f36987176bf8770a4/manifests/ate-install/ate-api-server.yaml
	defaultAPIServerReplicas   = 2
	bundledPostgresConnections = 100
)

func validateConnectionBudget(replicas, pool, available int) error {
	if replicas < 1 || pool < 1 || available/replicas < 6 || pool > available/replicas-6 {
		return fmt.Errorf("API server replicas times (postgresPoolMaxConns + 6) exceeds the database connection budget of %d after headroom", available)
	}
	return nil
}

// Flexible Server creation defaults and reserved slots:
// https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/concepts-limits#maximum-connections
func managedDatabaseMaxConnections(sku DatabaseSKU) (int, bool) {
	switch sku.Tier {
	case "Burstable":
		switch sku.Name {
		case "Standard_B1ms":
			return 50, true
		case "Standard_B2s":
			return 429, true
		case "Standard_B2ms":
			return 859, true
		case "Standard_B4ms":
			return 1718, true
		case "Standard_B8ms":
			return 3437, true
		case "Standard_B12ms", "Standard_B16ms", "Standard_B20ms":
			return 5000, true
		}
	case "GeneralPurpose":
		switch sku.Name {
		case "Standard_D2s_v3", "Standard_D2ds_v4", "Standard_D2ds_v5", "Standard_D2ads_v5":
			return 859, true
		case "Standard_D4s_v3", "Standard_D4ds_v4", "Standard_D4ds_v5", "Standard_D4ads_v5":
			return 1718, true
		case "Standard_D8s_v3", "Standard_D8ds_v4", "Standard_D8ds_v5", "Standard_D8ads_v5":
			return 3437, true
		case "Standard_D16s_v3", "Standard_D16ds_v4", "Standard_D16ds_v5", "Standard_D16ads_v5",
			"Standard_D32s_v3", "Standard_D32ds_v4", "Standard_D32ds_v5", "Standard_D32ads_v5",
			"Standard_D48s_v3", "Standard_D48ds_v4", "Standard_D48ds_v5", "Standard_D48ads_v5",
			"Standard_D64s_v3", "Standard_D64ds_v4", "Standard_D64ds_v5", "Standard_D64ads_v5",
			"Standard_D96ds_v5", "Standard_D96ads_v5":
			return 5000, true
		}
	case "MemoryOptimized":
		switch sku.Name {
		case "Standard_E2s_v3", "Standard_E2ds_v4", "Standard_E2ds_v5", "Standard_E2ads_v5":
			return 1718, true
		case "Standard_E4s_v3", "Standard_E4ds_v4", "Standard_E4ds_v5", "Standard_E4ads_v5":
			return 3437, true
		case "Standard_E8s_v3", "Standard_E8ds_v4", "Standard_E8ds_v5", "Standard_E8ads_v5",
			"Standard_E16s_v3", "Standard_E16ds_v4", "Standard_E16ds_v5", "Standard_E16ads_v5",
			"Standard_E20ds_v4", "Standard_E20ds_v5", "Standard_E20ads_v5",
			"Standard_E32s_v3", "Standard_E32ds_v4", "Standard_E32ds_v5", "Standard_E32ads_v5",
			"Standard_E48s_v3", "Standard_E48ds_v4", "Standard_E48ds_v5", "Standard_E48ads_v5",
			"Standard_E64s_v3", "Standard_E64ds_v4", "Standard_E64ds_v5", "Standard_E64ads_v5",
			"Standard_E96ds_v5", "Standard_E96ads_v5":
			return 5000, true
		}
	}
	return 0, false
}

func (c Config) validateSizing() error {
	if err := c.Substrate.validateSizing(); err != nil {
		return err
	}
	for _, workload := range c.Substrate.sizedWorkloads() {
		if workload.name != "ate-api-server" || workload.pool == nil {
			continue
		}
		limit := bundledPostgresConnections
		switch c.Database.Backend {
		case "", DatabaseBundled:
		case DatabaseFlexible:
			var ok bool
			limit, ok = managedDatabaseMaxConnections(c.Database.ComputeSKU())
			if !ok {
				return errors.New("connection budget unavailable for this database SKU")
			}
		default:
			return errors.New("connection budget unavailable for this database backend")
		}
		headroom := limit / 10
		if limit%10 != 0 {
			headroom++
		}
		if c.Database.Backend == DatabaseFlexible {
			headroom = max(headroom, 15)
		}
		replicas := defaultAPIServerReplicas
		if workload.replicas != nil {
			replicas = *workload.replicas
		}
		return validateConnectionBudget(replicas, *workload.pool, limit-headroom)
	}
	return nil
}

func (s SubstrateConfig) validateSizing() error {
	validateResources := func(field string, r *ResourceRequirements) error {
		if r == nil {
			return nil
		}
		for _, values := range []map[string]string{r.Requests, r.Limits} {
			for name, value := range values {
				q, err := resource.ParseQuantity(value)
				if err != nil || q.Sign() < 0 {
					return fmt.Errorf("%s: invalid non-negative quantity for %s: %q", field, name, value)
				}
			}
		}
		return nil
	}
	for _, workload := range s.sizedWorkloads() {
		if workload.replicas != nil && *workload.replicas < 1 {
			return fmt.Errorf("substrate.sizing.%s.replicas must be at least 1", workload.field)
		}
		if workload.name == "ate-api-server" {
			if workload.pool != nil && *workload.pool < 1 {
				return errors.New("substrate.sizing.apiServer.postgresPoolMaxConns must be at least 1")
			}
			if workload.replicas != nil && workload.pool == nil {
				return errors.New("substrate.sizing.apiServer.replicas requires an explicit postgresPoolMaxConns")
			}
		}
		for _, container := range workload.containers {
			if err := validateResources("substrate.sizing."+container.field, container.resources); err != nil {
				return err
			}
		}
	}
	if s.Atelet != nil {
		if s.Source.Commit != DefaultCommit && (s.Atelet.SnapshotUploadDelay == nil) != (s.Atelet.SnapshotDetachDelay == nil) {
			return errors.New("set both substrate.atelet snapshot delays for a non-default source revision; its built-in defaults are not known")
		}
		upload, detach := defaultSnapshotUploadDelay, defaultSnapshotDetachDelay
		for _, item := range []struct {
			field  string
			value  *string
			target *time.Duration
		}{
			{"snapshotUploadDelay", s.Atelet.SnapshotUploadDelay, &upload},
			{"snapshotDetachDelay", s.Atelet.SnapshotDetachDelay, &detach},
		} {
			if item.value == nil {
				continue
			}
			d, err := time.ParseDuration(*item.value)
			if err != nil || d <= 0 {
				return fmt.Errorf("substrate.atelet.%s must be a positive Go duration", item.field)
			}
			*item.target = d
		}
		if upload > detach {
			return errors.New("substrate.atelet snapshot upload delay must not exceed the detach delay")
		}
	}
	return nil
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
		Database:     DatabaseConfig{Backend: DatabaseBundled},
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
	c.Database = c.Database.WithDefaults()
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
	switch c.Database.Backend {
	case "", DatabaseBundled:
		if c.Database.Location != "" {
			return errors.New("database.location is only valid for managed PostgreSQL")
		}
		if c.Database.SKU != (DatabaseSKU{}) {
			return errors.New("database.sku is only valid for managed PostgreSQL")
		}
	case DatabaseFlexible:
		if !regexp.MustCompile(`^[a-z][a-z0-9]+$`).MatchString(c.DatabaseLocation()) {
			return errors.New("database.location must be an Azure region name")
		}
		sku := c.Database.ComputeSKU()
		if !regexp.MustCompile(`^Standard_[A-Z][A-Za-z0-9]*(_v[0-9]+)?$`).MatchString(sku.Name) {
			return fmt.Errorf("database.sku.name %q must be a Flexible Server compute SKU such as %s", sku.Name, DefaultDatabaseSKU.Name)
		}
		switch sku.Tier {
		case "Burstable", "GeneralPurpose", "MemoryOptimized":
		default:
			return fmt.Errorf("database.sku.tier %q must be Burstable, GeneralPurpose or MemoryOptimized", sku.Tier)
		}
		// The database credential vault grants access to the AKS operator.
		if c.AKS.Operator == nil {
			return errors.New("managed PostgreSQL requires aks.operator, which receives access to the database credential vault")
		}
	default:
		return errors.New("database.backend must be bundled or flexible-server")
	}
	if err := c.validateSizing(); err != nil {
		return err
	}
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
	if c.AKS.WorkerPool.osDiskType() == "Ephemeral" && c.AKS.WorkerPool.osDiskSizeGB() > MaxEphemeralOSDiskSizeGB {
		return fmt.Errorf("aks.workerPool.osDiskSizeGB must be at most %d for an Ephemeral OS disk", MaxEphemeralOSDiskSizeGB)
	}
	if pods := c.AKS.WorkerPool.MaxPods; pods != 0 && (pods < 10 || pods > 250) {
		return errors.New("aks.workerPool.maxPods must be 10-250, or omitted for the AKS default")
	}
	if pods := c.AKS.WorkerPool.MaxPods; pods != 0 && pods*c.AKS.WorkerPool.Count < MinPoolPods {
		return fmt.Errorf("aks.workerPool.maxPods times aks.workerPool.count must be at least %d, which AKS requires of every pool", MinPoolPods)
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

func (c Config) infrastructureDeploymentFilter() string {
	return fmt.Sprintf("[?name=='%s' || starts_with(name, '%s-run-')]", c.deploymentName(), c.deploymentName())
}
