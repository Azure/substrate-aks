# Operations

[Quickstart](../README.md) | [Architecture](architecture.md) | [Configuration](configuration.md)

Run examples from the repository root. Commands use `deploy.json` by default;
add `--config PATH` consistently when managing another installation.

- [Guided installation](#guided-installation)
- [Registry and image bootstrap](#registry-and-image-bootstrap)
- [Provision and install](#provision-and-install)
- [Component redeployment](#component-redeployment)
- [Client and counter demo](#client-and-counter-demo)
- [Private actor images](#private-actor-images)
- [Progress and diagnostics](#progress-and-diagnostics)
- [Retry and teardown](#retry-and-teardown)
- [Development checks](#development-checks)
- [Live acceptance and limitations](#live-acceptance-and-limitations)

## Guided installation

Install the [quickstart prerequisites](../README.md#quickstart), including Bicep
(`az bicep install`). The installer host supports Linux/macOS with Go 1.27.3+
(also satisfying the pinned upstream source's Go 1.27 requirement), installed
explicitly or through automatic toolchain selection. Default image builds require
Docker with buildx or Podman,
`bash`, `curl`, `tar` with `--zstd`, `zstd`, `readlink -f`, and `sha256sum`.
Supplying the four matching runtime assets skips assembly-tool checks.

```sh
make build
az login
bin/substrate-aks setup
```

The guide uses the active Azure subscription and identity for a new config,
displays the subscription as `Name (GUID)`, and asks for a name and region.
Press `/` in the region selector to filter. It explains snapshot permissions and
billing, previews registry and full-infrastructure changes, and asks before
applying them. It builds images, provisions AKS/storage, checks prerequisites,
installs Substrate, optionally deploys the demo, and prints client shell exports.

| Mode | Behavior |
| --- | --- |
| Default terminal | Interactive huh forms |
| `--accessible`, `TERM=dumb`, or redirected output | Line-oriented prompts with validated region input |
| `NO_COLOR=1` | Disable styling |
| `--yes` | Accept billable deployment and the final kube-context import/selection without prompts |
| `--with-demo` | Also deploy the counter demo in noninteractive setup |
| `--container-engine=auto\|docker\|podman` | Detect working builders and offer a choice when both work (`--yes` prefers Docker/buildx), or force one; applies only when building images |

`--yes` skips prompts, not the startup artwork. Both modes show the same banner
in a normal terminal; accessible, redirected, dumb, or narrow terminals retain
the plain heading.

For automation, create a complete [configuration](configuration.md) first:

```sh
bin/substrate-aks setup --config deploy.json --yes --with-demo
```

Do not pipe answers as an automation interface. Saved configuration remains
authoritative. The wizard invokes the same lifecycle commands below, not a
second deployment implementation. Apply its printed exports yourself; it never
modifies shell profiles or PATH. At successful interactive completion, it offers
an optional kube-context import and selection before printing demo next steps.
`--yes` accepts this handoff automatically. Declining or EOF in interactive setup
leaves your kubeconfig untouched.

### macOS host setup

Install Apple's command-line developer tools (`xcode-select --install`) for native
Go/cgo builds and race-enabled tests. For runtime asset assembly, install the
utilities used by the pinned script and make their unprefixed names available:

```sh
brew install coreutils gnu-tar zstd
export PATH="$(brew --prefix coreutils)/libexec/gnubin:$(brew --prefix gnu-tar)/libexec/gnubin:$PATH"
```

These are additions to the prerequisites above, not a complete tool installation.
Homebrew's GNU `readlink` and `tar` otherwise use prefixed names; installing a
package alone does not ensure the pinned script will find the expected command.
The export affects the current shell only. Supplied runtime assets skip assembly
tool checks. Docker Desktop or Podman's machine must also be running for image
publication, with Linux/amd64 build support on Apple Silicon.

Two reported tooling warnings need to be distinguished from installation failures:

- `ld: warning: -bind_at_load is deprecated on macOS` comes from Go's external
  linker invocation, not installer flags. Go already contains an exact-warning
  filter associated with [golang/go#63154](https://github.com/golang/go/issues/63154).
  The flag supports correctness with older Apple linkers; do not remove it or
  disable `-race` to hide the warning. A recurrence needs the actual Go and Apple
  toolchain versions and the failing or successful command's exit status.
- Homebrew Azure CLI can emit a Python `SyntaxWarning` for an invalid escape in
  an SDK docstring. David's report names
  `azure/mgmt/resource/deploymentstacks/models/_models.py`, not repository Python
  code. The same warning class has appeared in other SDK packages
  ([Azure/azure-cli#31789](https://github.com/Azure/azure-cli/issues/31789)); that
  issue is not proof of a fix for this particular package. Successful installer
  subprocesses keep stderr diagnostics separate from parsed stdout. Direct
  `az` commands and `make bicep` still show upstream stderr. Do not globally
  suppress Python warnings or patch the user's Homebrew environment.

For a native reproduction, record:

```sh
sw_vers
go version
go env GOHOSTOS GOHOSTARCH CC CGO_ENABLED GOTOOLCHAIN
xcode-select -p
xcrun clang --version
az version
```

Native macOS/Apple Silicon runtime assembly, image publication, and installation
remain acceptance gates; successful Darwin cross-builds do not establish them.

## Registry and image bootstrap

```sh
bin/substrate-aks doctor
bin/substrate-aks plan --registry-only
bin/substrate-aks provision --registry-only
bin/substrate-aks images
```

`doctor` works without a configuration and checks local tools without Azure resource
calls. Use `doctor --config deploy.json` to also check mode-specific assembly
prerequisites or supplied asset files. Setup checks local tools before Azure
discovery and applicable runtime prerequisites before billing confirmation.
Account, directory, region, and local-tool probes have visible progress and
30-second deadlines. Supplied files must be nonempty regular files; their exact
pinned checksums are still verified during source preparation.

`plan` runs real
subscription-scope Bicep what-if; it requires Azure access but submits no resource
deployment.

> [!WARNING]
> **Registry provisioning starts billing.** It creates the dedicated resource
> group, a deterministically named Basic ACR, and an ACR-scoped `AcrPush` grant
> for the configured upload identity. It does not create AKS or Blob storage yet.

Setup checks working image builders before the installation summary and billing
confirmation when images need publishing. If both Docker/buildx and Podman work,
setup offers an **Image builder** choice, defaulting to Docker. If only one works,
it selects that engine automatically. Accessible mode supports the same choice
with validated line-oriented input. An explicit `--container-engine` skips the
picker and never falls back to another engine; `--yes` also skips the picker and
prefers working Docker/buildx, then Podman. Reusing configured images requires no
builder check or choice. Probes are bounded, and the summary shows the selected
engine, which stays pinned through subsequent stages.
`doctor --config deploy.json --container-engine=auto` runs the same local check
without Azure resource calls. The choice is not saved in deployment configuration.

On Linux, Podman can run rootless; on macOS, its machine must be running.
Builds target `linux/amd64`. Other host architectures need working amd64
execution/emulation for the pinned Rust build; `podman info` alone does not
establish that capability. Real rootless publication and macOS/ARM behavior
still need live validation; the contract tests below use controlled commands.

`images` obtains an ACR token with `az acr login --expose-token`, without invoking
Docker. Token output (including failure output) is captured privately and never
written to diagnostic logs or verbose/result streams. Credentials are atomically
written to a mode-0600 Docker-compatible config in the ignored workspace for
`ko`, upstream digest lookups, and Podman's explicit `--authfile`.
Other registry credentials and credential helpers are not copied or replaced.
For Docker builds, the installer preserves the caller's active context (including
TLS material), CLI plugin search paths, and selected buildx state. Explicit
Docker host/context/TLS and `BUILDX_CONFIG` overrides remain effective; Docker
preflight and publication use the same environment. If the isolated workspace
already has a separate, non-symlink context store, move that store aside as
instructed rather than letting the CLI overwrite it.

From the exact source commit, it publishes platform/demo images with upstream
`ate-setup publish release-images` and the Azure plugin with a pinned ko tool.
Builds get fresh commit-derived tags; the plugin is recorded by digest in
`substrate/plugins/objectstore/azure-blob`.

The pinned publisher initializes Kubernetes clients unnecessarily. A guarded
startup adaptation in the disposable build checkout allows publishing without
a cluster/kubeconfig. It does not change deployment startup or the source used
for installation. An unknown startup contract fails before publication.
For Podman, an additional guarded build-only adapter replaces the Envoy
Dockerfile publisher with `podman build` and `podman push`, validating the digest
written by the push before recording success. Base-image names are qualified
with `docker.io` to avoid short-name prompts; their pinned digests are unchanged.
Go images and the Azure plugin still use `ko`. Docker-specific build flags are
rejected on the Podman path rather than silently ignored.

If `substrate.runtime.assetsDir` is unset, `images` runs the pinned
`hack/microvm-assets/assemble.sh`; otherwise it verifies supplied files without
overwriting them. It verifies all four asset checksums before atomically updating
only image/runtime settings. Concurrent config edits and already-configured image
sets are rejected.

Saved references are configuration, not a publication receipt. `setup` checks the
configured set before provisioning AKS; `install` checks it again before source
preparation, cluster probes, asset uploads, or an `installing` checkpoint. Run the
same check independently:

```sh
bin/substrate-aks images --check --config deploy.json
```

The check uses the exact pinned publisher's component inventory and dataplane
image, plus the configured Azure plugin digest. A read-only helper is compiled in
a disposable source checkout; the managed installation checkout is not changed.
It performs read-only registry
manifest lookups with a 30-second timeout per image and stops on the first failure.
Missing manifests, authentication failures, authorization failures, and network
errors have separate diagnostics. An access failure is not evidence that images
are missing and never triggers publication or changes the config.

Failed builds can leave images in ACR but do not update config. Role-propagation
errors remain failures: wait and retry. The uploader must authenticate as the
configured Azure identity.

Deployment, component retries, and demo commands refresh owned-ACR credentials
and reuse the isolated Docker config for upstream digest lookups; expiration
does not require rebuilding images. Other image repositories use your existing
Docker credentials.

## Provision and install

```sh
bin/substrate-aks plan
bin/substrate-aks provision
bin/substrate-aks check
bin/substrate-aks prepare
bin/substrate-aks install
bin/substrate-aks status
bin/substrate-aks client
```

Full provisioning reconciles the same ACR and creates:

| Resource | Configuration |
| --- | --- |
| Dedicated resource group and AKS | One system node and a separate tainted amd64 microVM pool |
| Cluster networking | Azure CNI Overlay and Cilium network policy |
| Cluster features | Managed disks, OIDC, workload identity; Kubernetes and node OS auto-upgrade channels disabled |
| Blob storage | Separate snapshot and asset containers; no public Blob access or shared-key authentication |
| Runtime identities | Separate node/control identities, service-account federation, and container-scoped roles |
| Upload permissions | Asset-container upload access for the configured Azure identity |
| Image pulls | Registry-scoped `AcrPull` for the AKS kubelet identity |

Registry-only bootstrap has a separate deployment name and cannot replace the
full deployment's infrastructure outputs. Authorization propagation can take
time; errors remain explicit rather than silently skipping required grants.

`check` verifies control-plane/node versions, expected microVM node count, and
discovery of `PodCertificateRequest` and `ClusterTrustBundle`. **Discovery does
not prove kubelet certificate projection works:** installation starts the signer
and consuming workloads, whose rollout must also succeed.

Before installation, the CLI verifies image availability, checks assets and the
install record, then runs a temporary privileged DaemonSet on the microVM pool.
It opens `/dev/kvm`, calls
`KVM_CREATE_VM`, and checks the native ACR provider and managed-identity node
configuration. The probe is removed on success or failure and requires cluster
administrator permissions. There is no silent container, gVisor, or anonymous
registry fallback.

AKS Ubuntu nodes must provide
`/var/lib/kubelet/credential-provider/acr-credential-provider` and
`/etc/kubernetes/azure.json`. The pinned atelet image includes the glibc runtime
needed to run the host provider.

`prepare` renders the [Azure Kustomize components](architecture.md) into a managed
source checkout. Runtime assets are uploaded privately to Blob during installation,
not through GCS/S3 or a public asset container. The node sidecar can read the asset
container; the control identity cannot. Runtime fetches verify SHA-256 values.

## Component redeployment

```sh
bin/substrate-aks install --component atelet
bin/substrate-aks install --component apiserver
```

Use this CLI for supported component redeploys. They require a completed install
or a retry of the same interrupted component operation. Full redeploys retain
upstream's active-worker safeguards.

Managed checkouts are verified and must not be edited. Demo-only adapter changes
use a new source cache without changing the installed deployment fingerprint.
**Upgrades are not implemented:** a changed source/image fingerprint is rejected
instead of attempting a mixed-version deployment. Changing the upstream pin
requires a fresh installation or a separately reviewed upstream upgrade plan.

## Client and counter demo

Run `bin/substrate-aks client` (also run by setup) and apply its printed exports
before using `kubectl`. Without the kubeconfig export, an unconfigured client may
try localhost.

| Setting | Behavior |
| --- | --- |
| Plugin installation | `go install ./cmd/kubectl-ate` from the exact configured source |
| Binary destination | `GOBIN`, or the first `GOPATH` entry's `bin` directory; normally `~/go/bin` |
| Kubeconfig | Installation-specific; ambient config is changed at the final handoff with interactive consent or `setup --yes`, or by `client --use-context` |
| PATH export | Printed only if the installed plugin is not discoverable |
| Sharing | Another installation can replace the client; teardown leaves it installed |

Credential refresh fetches and validates a private temporary kubeconfig before
atomically replacing the installation's file. Failed or incomplete Azure CLI
output leaves the previous credentials intact.
For managed Entra configurations, conversion to `kubelogin --login azurecli`
happens on that temporary file before replacement; a failed conversion or
non-Entra result also preserves the previous file. Keep `az` and Azure's
`kubelogin` on PATH and sign in to the cluster's tenant. `check` waits up to five
minutes for an explicit authorization denial to clear after new RBAC grants.
Network, authentication, and unexpected-response failures are surfaced rather
than treated as propagation. Each authorization probe is limited to 15 seconds.

The managed Entra path has local tests and compiled-template validation, but
has not yet been validated in a fresh live deployment. Earlier live acceptance
used the legacy authentication path.

### Optional kube-context handoff

After client installation, setup shows the destination file and the previous/new
contexts, then asks whether to import and select this installation. The default is
**No** interactively; `--yes` accepts without prompting. The destination is a single
`KUBECONFIG` file or `~/.kube/config`. Use setup's `--context-file PATH` to choose
another file.
Multi-file `KUBECONFIG` is not flattened automatically.

Only the selected installation's context, cluster, and user are imported.
Unrelated credentials, contexts, and extensions are preserved. Different entries
with colliding names, malformed files, symlinks, and relative private credential
paths are refused rather than overwritten. Accepted updates use a private
temporary file and atomic replacement, with a check for destination changes
since the prompt. YAML comments/formatting are not retained. The installer's
private kubeconfig is never changed by handoff.

If an unattended handoff cannot be completed, setup reports an error instead of
claiming success without selecting the context. The installed cluster is retained;
use `client --use-context --context-file PATH` to complete the handoff.

To explicitly perform the same handoff later:

```sh
bin/substrate-aks client --config deploy.json --use-context
# Or select a single destination explicitly:
bin/substrate-aks client --config deploy.json --use-context --context-file ~/.kube/substrate
```

The child process cannot change your parent shell's `KUBECONFIG` or PATH. If
handoff is accepted, apply the **final** printed kubeconfig export instead of
the earlier private-file export, then run the printed pod check and demo commands.

Actor requests use upstream `kubectl ate` directly, with no installer ownership
query or source check per request. Upstream automatically port-forwards its API
and supports authentication flags such as `--token-file`. Without one, it warns
that the shared `ate-client` service account is used; this is not personal
Entra-backed actor authorization.

```sh
# Skip deployment if setup already installed the demo.
bin/substrate-aks demo
kubectl ate create actor my-counter \
  -a ate-demo-counter-microvm --template counter-microvm
kubectl ate get actor my-counter -a ate-demo-counter-microvm
kubectl ate suspend actor my-counter -a ate-demo-counter-microvm --mode=sync
kubectl ate resume actor my-counter -a ate-demo-counter-microvm
```

For HTTP access, use the exported kubeconfig and forward the router:

```sh
kubectl -n ate-system port-forward svc/atenet-router 8000:80
```

Follow the [upstream counter guide](https://github.com/agent-substrate/substrate/tree/main/demos/counter)
to compare actor state before suspension and after resume. Deployment success
alone does not establish state preservation.

## Private actor images

Kubelet pulls Kubernetes images with its managed identity. Atelet independently
invokes the mounted native ACR credential provider through upstream's kubelet
credential-provider protocol. It uses node configuration and IMDS; both paths
use the kubelet identity's registry-scoped `AcrPull`. Blob sidecars instead use
their separate federated identities.

Authenticate with `az acr login`, push actor images to the owned registry, and
reference them in actor templates, preferably by digest. The generated provider
configuration matches Azure public-cloud ACR hosts. Other private registry
providers and service-account image-pull identity bindings are not configured.
This is installation-wide pull access, not per-actor or per-tenant authorization.

**Teardown deletes the owned registry and every image pushed there.**
Long-duration credential refresh remains a live validation gate.

## Progress and diagnostics

| Output | Behavior |
| --- | --- |
| Progress | stderr; headings, steps, completion, and an indeterminate elapsed-time spinner |
| Plain progress | Every 20 seconds with redirected output, `--verbose`, `NO_COLOR`, or `TERM=dumb`; status also uses plain updates |
| Results | stdout, including infrastructure preview and pod status |
| Setup completion | Branded success banner on stderr, followed by numbered demo commands on stdout; printed only after client setup and any accepted context handoff succeed |
| Detailed log | Mode-0600 per-command file in the installation's `logs/` directory |
| `--verbose` | Stream underlying commands, raw build output, and diagnostics |

The spinner clears before other output, errors, interruption, and completion.
Color is terminal-only; labels remain meaningful without it. Subprocess and
actor output pass through rather than receiving installer styling. Failures
include context, a short subprocess error excerpt when available, and a log path.
The completion banner uses the welcome's purple accent and a green `DONE` label.
Demo headings and commands are colorized only when their output is a terminal;
`NO_COLOR`, dumb terminals, and redirected streams remain plain. Copyable command
lines are never auto-wrapped or padded after shell-continuation backslashes.

```sh
bin/substrate-aks provision --registry-only --verbose
bin/substrate-aks status --verbose
```

`plan` renders a resource-change table: green creates, red deletions, yellow
modifications, cyan deployments, and visible unknown changes. Symbols also work
without color. IDs are relative to the displayed subscription when possible;
cross-subscription IDs stay fully qualified and long IDs wrap without truncation.
Unchanged resources are omitted; property-level details remain in logs and
verbose output.

## Retry and teardown

The CLI uses an explicit subscription/context, separate kubeconfig, and
per-installation lock. Inherited Substrate/GCP/S3 settings that could redirect
operations are removed.

**Failures retain resources.** Interactive setup offers to retry the failed
command with the same settings; declining exits. Cancellation and `--yes` never
enter a retry loop. Failure instructions print the exact stage command and a
separate full-reconciliation command, preserving the selected builder, demo,
verbosity, and accessibility options and the invoked executable path.

Ctrl-C does not roll back Azure's server-side deployment. A new setup process
starts from the beginning with the same config, checking ownership and verifying
recorded images before reuse; it is not a durable stage resume. Keep the config
and installation ID unchanged.
Do not discard the config while resources from a retry may still exist.

The first Ctrl-C cancels work and allows bounded cleanup. A second Ctrl-C
terminates immediately; it can leave temporary cluster probes for manual cleanup.

Do not change the saved subscription, resource group, or location to move an
installation. Provision and plan refuse another live group bearing the same
installation tag in the configured subscription. Restore the original target for
management or teardown; a separate installation needs a new configuration.
Tag discovery never authorizes deleting additional groups.

Groups without the matching installation ownership tag are not adopted or
deleted. This guard does not make it safe to add unrelated resources to the
dedicated group.

```sh
bin/substrate-aks destroy --config deploy.json --confirm-delete substrate-dev
```

> [!CAUTION]
> **Deletes the entire owned group and its persistent data:** AKS, ACR/images,
> PostgreSQL data, snapshots, runtime assets, identities, and any other resources
> in that group. There is no automatic recovery.

The command waits for Azure deletion; an already-absent group is a safe no-op.
Local config/logs, subscription deployment history, and the shared client remain.
Deleting the ACR also deletes its images, but does not clear saved references.
After complete teardown, explicitly reset those references before reusing the
configuration:

```sh
bin/substrate-aks images --reset-config --config deploy.json
bin/substrate-aks setup --config deploy.json
```

`--reset-config` requires Azure to confirm that the configured resource group is
absent and no differently named group bears this installation's tag in the
configured subscription. It atomically clears only `substrate.images`; installation identity,
source pin, runtime asset settings, and other configuration stay unchanged. It
does not publish images, remove checkpoints, or modify cloud resources. A
concurrent config edit or inability to verify absence is an error.

If setup has already recreated an empty registry or cluster from stale config,
fully destroy that group first, then reset and rerun setup. Reset is refused
while any group exists, including registry-only and partially installed targets.
Do not delete install records or change fingerprints to force a rebuild.
Alternatively, restore the exact configured image set and retry without changing
its references, or use a new config after completing teardown.

## Development checks

```sh
make test
make verify
make build
make bicep
```

`make test` runs race-enabled unit tests and the pinned upstream contracts.
The contracts automatically fetch a disposable, pristine checkout of the exact
source pin and check manifests, full/component rendering, and image publication
without a working kubeconfig. Git, Go (including the upstream-required toolchain),
`kubectl`, and network access to source/module downloads are required. The checkout
is removed afterward. `make test-unit` is the faster unit-only alternative;
`make test-contract` runs only the contracts.

To supply an existing pristine checkout instead:

```sh
go test -tags=integration ./internal/installer \
  -run 'TestPinned(Upstream|ImagePublisher|ImageCheck|PodmanPublisher)Contract' \
  -args -contract-root /absolute/path/to/pinned/substrate
```

Missing prerequisites fail rather than skip. `make verify` covers formatting
under `deploy/` and the embedded Go checker/publisher sources as well as ordinary Go files.
These tests do not provision Azure.
The image-check contract builds the real pinned checker and exercises its full
inventory, plugin digest, mixed registry credentials, and failure classifications
against local HTTP registries. It does not establish live ACR access.
The Podman publisher contract compiles the adapted pinned publisher and runs
controlled `ko`/Podman commands, asserting build targets, isolated credentials,
push digests, and build/push/digest failures. It does not perform real image builds.

## Live acceptance and limitations

| Check | Historical result |
| --- | --- |
| Target | Fresh AKS 1.37 installation in `southcentralus`; one system and one microVM node, both `Standard_D4s_v3` |
| `setup --yes --with-demo` | Completed private ACR publication, infrastructure, installation, runtime assets, golden snapshots, demo, and client setup |
| Stateful suspend/resume | Synchronous suspend and HTTP-triggered resume; memory, file, and rootfs counters advanced from 2 to 3 |
| Operator inspection | Listed 14 snapshot blobs; verified snapshots-container Storage Blob Data Reader scope |
| Teardown | Completed in 8m14s; installation and actual AKS node groups independently confirmed absent |

The cloud run used noninteractive consent; interactive prompts and decline paths
were checked separately, not as a second cloud deployment. It predates the shared
Go-installed client and used the former installation-local binary, so it does not
establish live acceptance of that later client change. Cloud persistent data and
registry images were deleted; ignored local diagnostics remain.

**Not established by that run:** cross-node restore from Blob (same-node cache
may have served resume), long-duration credential refresh, live component
redeploys, broader partial-install recovery, external resource changes, or
multi-operator use. Offline rendering tests are not live redeploy evidence.
Other regions, SKUs, Kubernetes versions, and CPU architectures were not validated.

Existing-cluster support, managed PostgreSQL (Flexible Server/Horizon DB), other
private actor registries, actor-scoped Blob authorization, autoscaling, dashboards,
shared filesystems, and upgrade automation remain deferred.
