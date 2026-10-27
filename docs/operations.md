# Operations

[Quickstart](../README.md) | [Architecture](architecture.md) | [Configuration](configuration.md)

Run commands from the repository root. Each command takes an installation
`NAME` (as an argument or `--name`); it is optional when only one installation
is saved, except for `init`, `setup` and `destroy`. Without a name, `init` and
`setup --yes` create `substrate-dev`, interactive `setup` asks for a name, and
`destroy` refuses to run. Run `bin/substrate-aks list` to see saved
installations. Settings are stored in a per-user state directory; see
[configuration](configuration.md#create-a-configuration).

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

Install the [prerequisites](../README.md#quickstart), then run:

```sh
az login
make setup
```

`make setup` builds `bin/substrate-aks` and runs its `setup` command. Pass setup
flags with `ARGS`, for example `make setup ARGS="--accessible"`.

Plain `setup` creates a new installation and asks for its name; names that are
already saved are rejected. `setup NAME` resumes or reconciles that saved
installation.

### What setup does

Setup works through a fixed sequence of stages. The left rail shows where you
are; the main pane lists each stage's operations as they run.

| Stage | What happens |
| --- | --- |
| Preparation | Checks local tools and the image builder, discovers your Azure identity, detects your public IP, and loads regions. A new installation then asks for a name, region, and optional snapshot access. Read-only Azure checks confirm AKS version and VM availability, CPU quota, and discoverable permissions. You review the installation before anything is billed. |
| Images and assets | Previews and creates the private registry, then builds and publishes images and runtime assets. Skipped when the config already has published images; they are only verified. |
| AKS cluster | Previews and provisions AKS, storage, and identities, then checks the cluster. |
| Substrate | Renders the Azure settings and installs Substrate. |
| Counter demo | Optional; deploys the counter microVM demo. |
| Connect | Installs the matching `kubectl ate` and offers to select the new kube-context. |

Each billable step shows an Azure preview first and waits for your approval.
The registry and the full infrastructure are approved separately. Previews
summarize changes by resource type; press `d` for full resource IDs. When a
preview deletes resources or contains unknown changes, scroll to the end and
press `a` to acknowledge it before approving. Use standalone `plan` for previews
too large to show inline.

The preflight checks are a snapshot, not a reservation: capacity, policy, and
later Azure validation can still fail.

### Using the workspace

In a terminal, setup runs as a full-screen workspace. Timers count setup work
only and pause while a question or approval waits for you. While an operation
runs, the latest lines of its diagnostic log appear beneath the checklist.

| Key | Action |
| --- | --- |
| `l` | Open the full, scrollable diagnostic log for the current command |
| `d` | Show the installation review or full resource-change IDs |
| `PgUp` / `PgDn` | Scroll a review or the log |
| `Esc` | Close details or logs; never skips an approval |
| `a` | Acknowledge a destructive or unknown preview after reading it |
| `?` | Show help |
| `Ctrl-C` | Stop local work after confirming (press twice to stop now); Azure resources are retained |

Letter shortcuts pause while you type a name or filter regions. In the region
list, press `/` to filter, then Enter to select. Tab and Shift+Tab move between
fields.

The welcome screen offers **Quick start**. **Advanced configuration** is a
planned guided editor and cannot start an installation yet. For custom settings,
run `init NAME`, edit the saved `installation.json` it reports, then run
`setup NAME`.

On narrow terminals the rail is hidden. Below 40 columns or 14 rows, setup shows
compact status and waits for a larger window before accepting input; work in
progress continues.

When setup finishes, it restores your terminal and prints one block of shell
exports. On failure it prints recovery commands instead.

### Options and automation

Setup also offers bundled PostgreSQL, Flexible Server, or HorizonDB (preview).
Managed database regions are filtered by subscription-specific capabilities.
Setup rechecks the chosen region before billing approval.

| Option | Use |
| --- | --- |
| `--accessible` | Line-oriented prompts for screen readers; also used automatically for redirected output and `TERM=dumb` |
| `NO_COLOR=1` | Disable color |
| `--yes` | Accept billing and select the new kube-context without prompts |
| `--with-demo` | Include the counter demo without prompts |
| `--skip-context-handoff` | Leave your kubeconfig untouched and print exports for the private kubeconfig |
| `--container-engine=auto\|docker\|podman` | Choose an image builder; `auto` offers a choice when both work |

For automation, pass a name and flags instead of piping answers into prompts.
`--yes` uses `substrate-dev` when no name is given:

```sh
bin/substrate-aks setup substrate-dev --yes --with-demo
```

**Run the final shell exports printed by setup.** The installer cannot change
your shell's `PATH` or `KUBECONFIG`, and never edits shell profiles. CI runs that
only need the installation's private kubeconfig can skip the context handoff:

```sh
bin/substrate-aks setup substrate-dev --yes --skip-context-handoff
```

### macOS host setup

Native builds and race tests need Apple's developer tools. If they are not
already installed, run `xcode-select --install`.

Runtime asset assembly needs `zstd`, a `tar` that reads `--zstd` archives,
`readlink -f`, and `sha256sum`. GNU implementations are not required when the
installed tools support these operations. Install missing tools as needed, for
example:

```sh
brew install zstd
```

If the host lacks `sha256sum` or a compatible `readlink`/`tar`, Homebrew
`coreutils`/`gnu-tar` are alternatives; expose their unprefixed commands on PATH.
Preflight checks the required capabilities before provisioning. Supplying
matching runtime assets skips assembly-tool checks.

Docker Desktop or Podman's machine must be running. Apple Silicon also needs
working Linux/amd64 builds, including execution of the pinned Rust build.
An engine responding to `info` does not prove that cross-architecture builds work.

Go's `-bind_at_load` deprecation warning and Azure CLI's Python invalid-escape
warnings have been reported. A warning alone does not mean installation failed;
check the command's exit status and error output.
The [Go linker warning](https://github.com/golang/go/issues/63154) comes from
upstream tooling, not installer linker flags.

For a failure report, capture the failing command, its exit status, and these
diagnostics **from the affected checkout**:

```sh
sw_vers
go version
go env GOVERSION GOTOOLDIR GOHOSTOS GOHOSTARCH CC CGO_ENABLED GOTOOLCHAIN
xcode-select -p
xcrun clang --version
az version
```

With `GOTOOLCHAIN=auto`, Go can select a newer toolchain for this module.
Native macOS assembly, publication, and installation remain unvalidated;
Darwin cross-builds are not a substitute.

## Registry and image bootstrap

Setup handles these steps for you. To run them individually:

```sh
bin/substrate-aks doctor
bin/substrate-aks plan --registry-only
bin/substrate-aks provision --registry-only
bin/substrate-aks images
```

`doctor` checks local tools without calling Azure. Add an installation `NAME` to
also check runtime assets or the tools to assemble them, and `--azure` for
read-only checks of VM availability, quota, and permissions in the configured
region. `plan` runs a Bicep what-if and deploys nothing.

> [!WARNING]
> Registry provisioning starts billing. It creates the dedicated resource group,
> a Basic ACR, and an `AcrPush` grant for the upload identity. It does not create
> AKS or Blob storage yet.

**Image builder.** Builds target Linux/amd64 with Docker buildx or Podman
(rootless works on Linux). Setup checks the builder before any Azure calls and
offers a choice when both work; `--yes` prefers Docker. Checking Docker starts
the buildx builder, which may download a BuildKit container and has two minutes
to start. If it is unhealthy, automatic mode falls back to Podman; an explicit
`--container-engine` never falls back. Reusing configured images needs no builder.

Registry-only provisioning never creates a managed database or its credentials,
egress IP, or bootstrap Job.

`images` builds the pinned source and publishes the Substrate, demo, and Azure
storage-plugin images. It assembles runtime assets unless `assetsDir` is set,
in which case it verifies the supplied files without overwriting them.
Image settings are saved only after publication and checksum checks succeed.

ACR authentication uses your Azure login. Registry credentials stay in the
private workspace; your Docker credentials, context, and buildx settings are left
alone. If the CLI reports a workspace context-store conflict, move the old store
aside as instructed.

To check that saved images are still accessible:

```sh
bin/substrate-aks images --check
```

`setup` and `install` also run this check. Authentication and network failures
are reported separately from missing images; the check never rebuilds images
or changes your config.

A failed build can leave images in ACR without updating the config; fix the
error and retry. New role grants can take a few minutes to propagate. The
uploader must be the configured upload identity or a member of the configured
publisher group.

Deployment commands refresh credentials for the owned ACR, so expired tokens
never require a rebuild. Other registries use your existing credentials.

## Provision and install

After image publication:

```sh
bin/substrate-aks plan
bin/substrate-aks provision
bin/substrate-aks check
bin/substrate-aks prepare
bin/substrate-aks install
bin/substrate-aks status
bin/substrate-aks client
```

| Command | Result |
| --- | --- |
| `provision` | Create AKS, Blob containers, identities, permissions, and any selected managed database; reuse the ACR |
| `check` | Verify node versions, microVM worker count, and required certificate APIs |
| `prepare` | Render Azure settings into the managed upstream source checkout |
| `install` | Verify images and KVM, upload runtime assets, and deploy Substrate |
| `status` | Show pods, persistent volume claims, and managed database metadata |
| `client` | Install the matching `kubectl ate` and print shell exports |

Managed databases use a restricted firewall, a static AKS outbound IP, and
IP-scoped cluster network permissions. Credentials are retained in a Standard
Key Vault with vault-scoped Secrets Officer access for the configured Azure
identity and seven-day soft-delete retention.

The cluster uses Azure CNI Overlay and Cilium network policy, with a separate
tainted microVM node pool. Automatic Kubernetes version upgrades are disabled to
preserve the supported 1.37 target. Node OS updates use AKS's `NodeImage` channel
with AKS-managed timing: patched images include security and bug fixes without
changing the Kubernetes version. See [configuration](configuration.md) for
identity and access settings.

> [!NOTE]
> Node image updates can drain and reimage nodes, interrupting workloads,
> especially with the default single microVM worker. Actor recovery through this
> maintenance has not been validated, and it is not a zero-downtime or
> cross-node memory guarantee. Configure an AKS maintenance window for
> controlled timing, and recheck KVM readiness and actors afterward. See
> [AKS node OS update channels](https://learn.microsoft.com/azure/aks/auto-upgrade-node-os-image).
> Existing clusters pick up this setting only when infrastructure is provisioned
> again.

Installation runs a temporary privileged probe that opens `/dev/kvm`, creates
a test VM, and checks the native ACR credential provider. This needs cluster
administrator access. The probe is removed afterward; unsupported nodes cause
an error, not a fallback to another runtime.

AKS Ubuntu nodes must provide these files:

```text
/var/lib/kubelet/credential-provider/acr-credential-provider
/etc/kubernetes/azure.json
```

Discovering the certificate APIs is not enough: the signer and consuming
workloads must also roll out successfully. Runtime assets are private Blob
objects, verified by checksum when fetched.

For a [managed PostgreSQL backend](configuration.md#postgresql-backend), `install`
first runs a bounded non-root PostgreSQL-client Job in AKS. It creates the
dedicated `substrate` database and runs the pinned upstream role/schema script
transactionally. The temporary Job, administrator Secret and SQL ConfigMap are
removed on success or failure. Failure does not record a completed install.
The installer observes both successful and failed Job conditions, reporting a
terminal failure without waiting for the full readiness deadline.
The application, not the bootstrap Job, runs its migrations under the separate
owner login.

Upstream receives explicit owner/runtime DSNs through its existing secret
environment contract on full install and component redeploy. It skips bundled
PostgreSQL and reconciles the API Secret and rollout hash itself. `status`
reports the managed endpoint and AKS egress address alongside workload status.
Keep the original config and installation ID for retries. Provisioning reuses
the credentials in the installer-owned Key Vault rather than rotating them.
The workspace contains no newly persisted database passwords; a valid legacy
credential file is migrated during provisioning and removed only after the
vault copy is verified. Run as the configured Azure identity or grant the
installer equivalent vault access. Provisioning waits up to three minutes for
recognized vault RBAC/DNS propagation failures instead of aborting on the first
probe, including Linux and macOS DNS error wording. Metadata-only probes retain
access diagnostics and never read passwords.
If access still fails, retry with the original config once permissions are
correct; a missing secret after database deployment requires restoring that secret.

Secret-handling Azure, Kubernetes Secret and upstream commands suppress
subprocess output even with `--verbose`; nonzero exits remain errors. This
also limits automatic diagnostics. Inspect Azure deployment operations for
provisioning failures. For bootstrap failures check service readiness, egress
firewall, image pulling and Job conditions; treat SQL logs and upstream records
as sensitive. Never paste credentials or DSNs into issues.

## Component redeployment

```sh
bin/substrate-aks install --component atelet
bin/substrate-aks install --component apiserver
```

Use the CLI for component redeploys. They require a completed installation or
a retry of the same interrupted component operation. Full redeploys retain
upstream's active-worker safeguards.

Do not edit managed source checkouts or change fingerprints to force a redeploy.
**Upgrades are not implemented:** changing the source or image set requires a
fresh installation or a separately reviewed upgrade plan.

## Client and counter demo

Run `bin/substrate-aks client` and apply its printed exports before using
`kubectl`. Without the kubeconfig export, an unconfigured client may try localhost.

| Setting | Behavior |
| --- | --- |
| Client version | Built from the configured upstream revision |
| Binary location | `GOBIN`, or the first `GOPATH` entry's `bin`; normally `~/go/bin` |
| Kubeconfig | Private installation file unless you accept the context handoff |
| Sharing | Another installation can replace the client; teardown leaves it installed |

Keep Azure CLI and `kubelogin` on PATH and stay signed in to the cluster's
tenant. If refreshing credentials fails, the previous kubeconfig is kept.

`check` asks Kubernetes whether you may create CRDs, waiting up to five minutes
for a new grant to take effect. It confirms authorization only, not that
workloads are healthy.

### Optional kube-context handoff

Setup offers to import and select the installation in a single `KUBECONFIG`
file, or `~/.kube/config`. The interactive default is **No**; `--yes` accepts.
Use `--context-file PATH` to choose another file. Multi-file `KUBECONFIG` needs
an explicit destination unless handoff is skipped.

`--skip-context-handoff` skips the import, even with `--yes`. Setup still
installs the client and prints exports for the private kubeconfig, without
reading yours. It cannot be combined with `--context-file`.

Imports keep unrelated entries but not YAML comments or formatting. They refuse
conflicting names, malformed files, symlinks, and relative credential paths, and
never overwrite an existing entry: on a name conflict, choose another
`--context-file` or resolve the conflict yourself. Imports take kubectl's
`<destination>.lock` and never remove an existing lock. The private installation
kubeconfig is never changed.

To do this later:

```sh
bin/substrate-aks client --use-context
# Or choose a destination:
bin/substrate-aks client --use-context --context-file ~/.kube/substrate
```

Setup prints one set of shell exports after the context choice. Apply those
exports to connect. If handoff is unavailable or fails, setup warns and completes
with exports for the private kubeconfig, including with `--yes`. Cancellation
still stops setup. An explicit `client --use-context` command returns an error if
importing fails; retry it with a suitable destination.

### Try the counter

```sh
# Skip this if setup already installed the demo.
bin/substrate-aks demo
kubectl ate create actor my-counter \
  -a ate-demo-counter-microvm --template counter-microvm
kubectl ate get actor my-counter -a ate-demo-counter-microvm
kubectl ate suspend actor my-counter -a ate-demo-counter-microvm --mode=sync
kubectl ate resume actor my-counter -a ate-demo-counter-microvm
```

For HTTP access, forward the router in another terminal:

```sh
kubectl -n ate-system port-forward svc/atenet-router 8000:80
```

Follow the [upstream counter guide](https://github.com/agent-substrate/substrate/tree/main/demos/counter)
to call the actor and compare its state before and after suspension.

`kubectl ate` talks directly to Substrate and can port-forward its API.
Without an explicit token, it uses the shared `ate-client` service account.
This is not personal Entra-backed actor authorization.

## Private actor images

Sign in with `az acr login`, push images to the installation's ACR, and reference
them in ActorTemplates, preferably by digest.

Kubernetes and atelet use the node's registry-scoped `AcrPull` access.
Blob sidecars use separate identities. Pull access is installation-wide, not
per actor or tenant. Other private registry providers are not configured.

**Teardown deletes the owned registry and every image pushed there.**
Long-duration credential refresh remains a validation gap.

## Progress and diagnostics

| Output | Location |
| --- | --- |
| Progress and prompts | stderr; plain line updates when redirected |
| Results, previews, and setup's final commands | stdout |
| Detailed command logs | Private files in the installation's `logs/` directory. Before a new installation is saved, Preparation logs to `logs/` in the installer's home directory. |
| `--verbose` | Underlying commands and build output, in standalone commands and accessible setup |

Each command writes its full log to disk, including commands, output, and step
markers; output marked sensitive is never logged. In the workspace, `l` shows a
sanitized tail of the current log so subprocess output never overwrites a form
or approval. For an append-only transcript, use `setup --accessible --verbose`.

Failures show a short error and the log path. If the optional kube-context
handoff fails, setup still completes, warns, and prints exports for the private
kubeconfig.

```sh
bin/substrate-aks provision --registry-only --verbose
bin/substrate-aks status --verbose
```

The `plan` table marks creates, deletes, and modifications with symbols as well
as color, so it stays readable with `NO_COLOR`. Unchanged resources are omitted;
property-level details are in the logs and verbose output.

## Retry and teardown

**Failures retain resources.** Interactive setup offers to retry the failed
command. `--yes` and cancellation do not enter a retry loop. Follow the printed
stage-retry command, or rerun `setup NAME`.

A new setup process checks the installation from the beginning; it does not
resume an arbitrary interrupted stage. Keep the installation ID and target
settings unchanged. Never delete the installation's directory while its
resources still exist.

Ctrl-C, SIGTERM, and terminal hangups (SIGHUP) cancel local command process
groups and allow cleanup. The interactive workspace restores the terminal after
its owned subprocesses exit. A second termination signal exits immediately and
may leave temporary probes to remove manually. Interruption does not undo Azure's
server-side deployment; already-submitted deployments may continue, and resources
remain for retry or teardown.

To delete the installation:

```sh
bin/substrate-aks destroy substrate-dev --confirm-delete substrate-dev
```

> [!CAUTION]
> Deletes the entire owned resource group: AKS, ACR and all its images,
> PostgreSQL data, snapshots, runtime assets, identities, and any unrelated
> resources added to the group. There is no automatic recovery.

The CLI checks the ownership tag and waits for Azure deletion. It does not adopt
or delete a group without the matching tag. An already-absent group is a safe
no-op. After Azure deletion succeeds, `destroy` removes the installation's local
directory, including its settings and logs. Subscription deployment history and
the shared client remain. If `destroy` fails, local state is kept for a retry.

Managed PostgreSQL servers and clusters are created in the installation's
resource group, even when the database region differs from AKS, so group
deletion removes them. Preview HorizonDB deletions can block group deletion;
`destroy` then fails and keeps local state for a retry. The database Key Vault
stays soft-deleted for seven days (purge protection is off); the installer
does not purge or recover it. Vault names derive from the installation ID, so
a new installation with the same name uses a different vault.

If the resource group was deleted outside the installer, clear references to
deleted images before reusing the saved installation:

```sh
bin/substrate-aks images substrate-dev --reset-config
bin/substrate-aks setup substrate-dev
```

Reset requires Azure to confirm that the target group is absent and no other
group carries the installation tag. It clears only image settings, not the
installation identity, source pin, assets, local records, or Azure resources.

If stale config has already recreated an empty registry or cluster, destroy it
before resetting. Do not remove install records to force a rebuild.
Alternatively, restore the exact configured images and retry, or use a fresh
config after teardown.

## Development checks

```sh
make verify
make test
make build
make bicep
```

| Command | Coverage |
| --- | --- |
| `make verify` | Formatting, vet, and module consistency |
| `make test-unit` | Race-enabled unit tests |
| `make test-contract` | Compatibility with a disposable checkout of the exact source pin |
| `make test` | Unit tests and contracts |
| `make bicep` | Compile the infrastructure template |

Contract tests need Git, Go, `kubectl`, and network access to source and module
downloads. Missing prerequisites fail the tests rather than skipping them.
No check above provisions Azure resources.

To use an existing pristine source checkout:

```sh
go test -race -tags=integration ./internal/installer \
  -run 'TestPinned(Upstream|ImagePublisher|ImageCheck|PodmanPublisher)Contract' \
  -count=1 -args -contract-root /absolute/path/to/pinned/substrate
```

The image-check tests use local registries, and publisher tests use controlled
engine commands. They do not prove live ACR access or real image builds.

Additional opt-in contracts check external PostgreSQL selection/secret
reconciliation, compiled network/Key Vault/HorizonDB configuration, deployment
history selection, and the real bootstrap shell twice in an isolated Docker
PostgreSQL instance. These also require Docker and Bicep, but no working
kubeconfig or Azure resource deployment:

```sh
go test -tags=integration ./internal/installer \
  -run 'TestPinnedManagedPostgresContract|TestPinnedDatabaseBootstrapScript|TestManagedBicep(NetworkGrantScope|VaultContract)|TestHorizonBicepAPIContract|TestInfrastructureDeploymentHistorySelection' \
  -args -contract-root /absolute/path/to/pinned/substrate
```

Missing prerequisites fail rather than skip. `make verify` covers formatting
under `deploy/` and the embedded Go checker/publisher sources as well as ordinary Go files.
These tests do not provision Azure.

The read-only Azure capability contracts check the configured fixed profiles
against Flexible Server in `westus3` and HorizonDB in `eastus`, and discover the
subscription-supported choices across the service region catalogs. They require
subscription access but create no resources and read no credentials:

```sh
go test -tags=integration ./internal/installer \
  -run '^TestAzureDatabaseRegion(Capabilities|Discovery)$' \
  -database-region-subscription SUBSCRIPTION_UUID
```

The separate Azure vault contract requires explicit billable-resource
authorization. It creates a disposable resource group and Key Vault for each
managed backend, checks stdin-only secret writes, retry/fresh-workspace reads,
ARM secure secret references and local-file/log leakage, then deletes its groups.
No database or AKS cluster is created. Vaults remain soft-deleted for seven days.

```sh
go test -tags=integration ./internal/installer \
  -run '^TestAzureDatabaseVaultContract$' -timeout=25m \
  -vault-contract-subscription SUBSCRIPTION_UUID \
  -vault-contract-operator SIGNED_IN_ENTRA_OBJECT_UUID
```

### Extending guided setup

Feature work should extend the operation flow, not the terminal renderer:

- Add configuration questions in `setupFlow` (`setup.go`) through
  `setupPrompt.run` or its typed helpers. They automatically use embedded forms
  in the workspace and existing line-oriented prompts in accessible mode.
  Do not call another top-level form/program or write directly to the terminal.
- Add review rows/warnings in `setupUI.summary`. The same summary feeds both
  presenters, so new database/monitoring settings do not need root-model fields.
  Keep saved configuration authoritative; never rebuild it from visible fields.
- Report slow work through `installer.App.Progress` or `setupUI.step`.
  The existing command runner keeps locking, private logs, and retry semantics.
  Structured previews use `App.Preview`; do not parse terminal tables.
- For preparation checklists, declare applicable rows with `setupUI.checklist`
  and run each operation through `setupUI.check`. Results come from the returned
  error, not progress text. Questions and approvals remain separate from checks.
- New lifecycle operations should be assigned to an existing conceptual phase
  in `setupPhase` when possible. Keep phase IDs separate from display titles.
  Business policy belongs in installer operations, not `setupWorkspace.Update`.
- Preserve the setup context when invoking a command: it carries the internal
  presentation session. Standalone commands have no session and retain their
  normal output. Keep raw/sensitive subprocess output out of progress messages.

The Linux `TestWorkspacePTY*` tests use real pseudo-terminals with fake operations,
including input/filtering, resize, retries, subprocess cancellation, restoration,
and post-exit shell handoff. They do not contact Azure. Model tests also cover
planned choices, compact/destructive previews, bounded activity, and tiny windows.

## Live acceptance and limitations

These scenarios have been validated against real Azure resources:

| Area | Validated |
| --- | --- |
| Baseline | AKS 1.37 in `southcentralus`, one system and one microVM node, both `Standard_D4s_v3` |
| Setup | Private image publication, infrastructure, Substrate, runtime assets, demo, and client |
| Actor state | Suspend and resume preserved memory, file, and rootfs counters |
| Cold Blob restore | Memory restored on a new node with a compatible CPU model |
| Component operations | Image checks, cluster checks and status, single-component reinstall, and demo redeployment |
| Managed Entra | `eastus2` install with managed Entra, Azure RBAC, and local accounts disabled |
| Podman | Podman-only build, publication, and installation |
| Guided workspace | Interactive `eastus2` install through both approvals, demo, client, and context import; both contexts reached Ready nodes |
| Teardown | Ownership-checked deletion; installation and managed node resource groups confirmed absent |

Changes made after these runs, including later workspace polish, are covered by
race-enabled unit tests, pseudo-terminal tests, pinned-source contracts, Bicep
compilation, and read-only Azure preflight, but not by another full cloud install.

**The same VM size can have different CPU models.** A cold restore onto an
incompatible CPU lost actor memory, although file and disk state survived. A
successful restore does not guarantee migration across heterogeneous nodes.
Older series such as Dsv3 span several CPU generations with different TSC
rates, and Cloud Hypervisor refuses to restore a memory snapshot across them
(`SetTscKhz` fails). New installations therefore default to `Standard_D4s_v6`,
which runs on one CPU generation. Keep that series, or another x86 D/E/F
`_v6`/`_v7` size, when you run more than one microVM node; see
[#12](https://github.com/Azure/substrate-aks/issues/12). Dsv6 costs about 5%
more than Dsv3 (about $0.202/hour versus $0.192/hour per `D4s` node in
`eastus2`). AKS cannot change an existing pool's VM size, so rerunning
`provision` fails for installations created with the earlier Dsv3 default;
destroy and recreate them.

Changing the snapshot reader on an existing installation once needed a manual
grant. The fix is covered by tests; a clean live update has not been repeated.

Not yet validated: long-running credential renewal, multiple operators, native
macOS installation, automatic node-image replacement and recovery, broader
recovery from interrupted installs, and other regions, SKUs, Kubernetes versions,
and CPU architectures.

Not part of this quickstart: installing into an existing cluster, upgrades,
autoscaling, dashboards, and shared filesystems.

### Managed PostgreSQL acceptance

Separate managed database probes passed the exact pinned bootstrap twice, all
three migrations, owner/runtime role separation, UNLOGGED partition maintenance,
advisory locks and concurrent xid/snapshot outbox fencing over verified TLS:
Flexible Server PostgreSQL 17 in `westus3`, and HorizonDB PostgreSQL 17 in `eastus`.
Real Azure what-if succeeded for all three backends and selected only the chosen
database service; registry-only exclusion is covered independently.

Separate vault-only Azure contracts passed for both managed backend settings:
actual credential creation/readback, reuse from a fresh workspace, ARM secure
secret-reference resolution and local-file/diagnostic leakage checks. The
disposable groups were deleted and confirmed absent; vaults remain soft-deleted
for seven days. Updated real previews include the selected database and vault
only in managed mode, with neither in bundled or registry-only mode.

Fresh AKS installations also passed with Flexible Server in `westus3` and the
previous HorizonDB API `2026-05-01-preview` in `eastus`, both with AKS in
`southcentralus`. Each completed managed bootstrap and
API startup with two ready API replicas, plus two isolated counter microVM actors.
Two synchronous Azure Blob MEMORY snapshots and automatic HTTP-triggered and
explicit CLI resume preserved memory, durable-file and rootfs counters through
1, 2, 3 and 4. API and demo pods remained ready with zero restarts. Test actors
were removed, and both installations were torn down. This establishes scoped
live acceptance, not production readiness.

Independent Opus 5.5 and GPT-6.1 Sol runs each repeated both managed installations
and counter acceptance. Flexible Server reprovisioning passed. Both reproduced
HorizonDB's rejection of the old template's `createMode: Create` during
reprovisioning. The failed same-name ARM deployment then lost its outputs,
breaking installer discovery even though workloads and counters stayed healthy.
All four installation groups and their AKS node groups were deleted and
independently confirmed absent.

The current HorizonDB template uses `2026-10-01-preview`, omits creation
provenance and explicitly deploys pool and authentication resources. **Fresh
installation and repeat deployment with this API remain unverified.** In the
tested subscription, read-only East US discovery returned
`ParameterGroupNotFound` for the required regional
`default-postgresql-17-provisioned` group. Setup and full provisioning now fail
before resource creation if this prerequisite is unavailable. The installer does
not create this read-only service resource or silently revert to the old API.

Full provision attempts now use distinct `substrate-<installation-id>-run-<suffix>`
subscription deployment names. Resource discovery uses the latest successful full
attempt (or the legacy unsuffixed record), not a failed rerun's missing outputs.
Provision still returns its actual failure. This does not reconstruct outputs
already overwritten by an older installer. If no successful outputs remain,
inspect the installation's subscription deployment history and retry with the
original configuration; missing outputs alone do not establish that resources
are absent. Access errors and invalid/mismatched metadata remain errors.

The installed Bicep type catalog does not yet include this HorizonDB preview
API. Templates compile, but Bicep warns that it cannot validate the resource
properties. Compiled-template contracts check the documented API shape; they
do not replace live provider validation.

**Still unverified:** cross-node restore, sustained load, egress IP stability
after infrastructure changes, unauthorized-IP rejection, live component retries,
database reconnection after pod restarts, HorizonDB reprovisioning and renewable
Entra database credentials. These are not established by the scoped live runs,
SQL probes, Docker bootstrap execution or Azure previews. Preview HorizonDB
operations can remain provisioning/deleting for extended periods and fail;
inspect Azure operation status rather than assuming an accepted request succeeded.

Existing-cluster support, managed database migration/adoption/rotation, other
private actor registries, actor-scoped Blob authorization, autoscaling, dashboards,
shared filesystems, and upgrade automation remain deferred.
