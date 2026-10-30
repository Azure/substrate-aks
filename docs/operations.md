# Operations

[Quickstart](../README.md) | [Architecture](architecture.md) | [Configuration](configuration.md)

Run commands from the repository root. They use `deploy.json` by default;
add `--config PATH` when managing another installation.

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
make build
az login
bin/substrate-aks setup
```

Setup asks for a name and region, explains snapshot access, and previews changes
before requesting approval. It then builds images, creates the infrastructure,
checks the cluster, installs Substrate, and sets up the client.
Before any provisioning, read-only Azure checks verify discoverable deployment
permissions, AKS version/SKU availability, and CPU quota. These checks do not
reserve capacity or replace later Azure validation.
VM SKU discovery uses a server-filtered regional Resource SKUs request and
follows all result pages; it does not download the subscription-wide catalog.

Interactive terminals use an alternate-screen installation workspace. The
header, phase rail, content pane, and keyboard footer persist after the welcome
screen, including initial configuration, approvals, retries, and client handoff.
The rail begins with Preparation and derives the remaining phases from the same
plan used to execute setup. Registry preview and provisioning share the Images
and assets phase with image publication and verification; their individual
checks and approval boundaries are preserved. The rail has no separate heading.
Long checklists keep the active or failed check in view, including when returning
from logs to a retry prompt.
Every phase reserves a marker column: gray pending
circle, purple current-phase circle, green completion check, or red failure
cross. The progress bar above the phases is blue.
The rail hides on narrow terminals and uses tighter spacing on short terminals.
The active phase reports named operations and elapsed time. Environment
discovery and deployment validation show their applicable checks up front.
The same preset checklist pattern covers registry bootstrap, image publication
and verification, AKS provisioning, Substrate installation, the optional demo,
and client setup. Rows represent the applicable setup operations, not individual
Azure log messages. Substeps update one detail line beneath the active row;
they never append a growing list. Resource previews expand beneath their
checklist row with the approval below, so the phase checklist stays in place.
Timers count setup work only: they pause while a question or approval waits on
you, and the panel shows "paused for input".
Image verification declares separate rows for resolving registry credentials,
fetching the pinned source, preparing the checker, and verifying published images
and the plugin. These rows remain visible on reruns that reuse saved images;
each completes only when its operation returns successfully.
Successful checks receive green checkmarks; the active check shows a blue spinner,
elapsed time, and one short status line beneath its row. The status line is
indented with a tree branch and light-blue text, distinguishing it from pending
operations; failed-operation details remain red. Pending rows use only their
circle marker and operation name, without a redundant status label.
While a check runs, the latest lines of the current command's private
diagnostic log fill the space beneath the checklist; `l` expands them into the full,
scrollable logs view. Progress messages alone never mark a check successful. Image-builder selection
is part of the local environment check, before Azure discovery or validation, with the
choice immediately below the active row. The one-line header shows the Substrate on AKS badge on the left and
the cluster, region, and subscription name on the right; the subscription ID is
shown, shortened, until its name is known. Unknown values show a dash and long
values shorten, so completing configuration does not move the header or change
the content width.
The header has no global status or elapsed timer.

New installations show the cluster/resource-group name and searchable Azure
region together in one form. Enter or Tab moves between fields; Shift+Tab goes
back to edit the name. The form sits below its heading without an empty status
area, and the region list grows with the available terminal height.
The name input shows an example placeholder when empty and validation feedback
as it is edited. Invalid names cannot advance; validation uses the same
3-40-character naming rules as the saved configuration.
Short terminals scroll the form to the focused field.
Accessible mode asks the same questions sequentially. Snapshot-access consent
and billable-change approvals remain separate.

Quick start is available; Advanced configuration is explicitly marked **Planned**
and cannot start an installation. The branded opening screen has two selectable
cards; use the arrow keys to choose, then Enter to begin Quick start. Saved
configuration files remain supported; "Advanced" refers to a future guided editor.

| Key | Action |
| --- | --- |
| `d` | Inspect the installation review or full resource-change IDs |
| `l` | Follow the current command's private diagnostic log (commands, output, and steps) |
| `PgUp` / `PgDn` | Scroll a review or the log |
| `Esc` | Close details/logs; never skips an approval |
| `?` | Show help |
| `a` | Acknowledge a destructive/unknown summary after reading it through |
| `Ctrl-C` | Stop owned subprocess work, retaining Azure resources |

Registry and full-infrastructure approvals remain separate. Compact previews
use a change summary with nonzero counts and readable resource categories.
Child resources, such as blob containers or federated credentials, are counted
on indented rows beneath their parent rather than in the parent's count;
full provider types and resource IDs remain available in details (`d`).
Completion chatter stays in activity rather than the approval. Deletion IDs and
unknown change types are shown explicitly; read their summary through to the bottom and press `a`
to acknowledge it before answering the approval. Oversized previews are refused,
not silently truncated. Use standalone `plan` for its full table.
Each resource category shows explicit action counts: green creates, yellow
updates, and red deletes. Text labels remain visible with `NO_COLOR`.
Letter shortcuts pause while entering text or filtering choices, so names and
searches can contain those letters normally.

Narrow windows use one column. Below 40 columns or 14 rows, setup shows compact
status and pauses input until resized; work and cancellation remain active.
After restoring the terminal, setup prints persistent connection instructions
with exactly one final set of exports. The completion card and command headings
retain terminal colors; `NO_COLOR` and redirected output remain unstyled.
Failures print recovery commands instead.

| Option | Use |
| --- | --- |
| `--accessible` | Line-oriented prompts; also used for redirected output and `TERM=dumb` |
| `NO_COLOR=1` | Disable color; use `--accessible` for append-only output |
| `--yes` | Accept billing and select the new kube-context without prompts |
| `--skip-context-handoff` | Leave the user kubeconfig untouched and print private kubeconfig exports |
| `--with-demo` | Include the counter demo in noninteractive setup |
| `--container-engine=auto\|docker\|podman` | Choose an image builder; auto mode offers a choice when both work |

`--yes` retains the startup banner in a normal terminal. Narrow, accessible, and
redirected output use a plain heading.

For automation, create a complete [configuration](configuration.md) rather than
piping answers into prompts:

```sh
bin/substrate-aks setup --config deploy.json --yes --with-demo
```

**Run the final shell exports printed by setup.** The installer cannot change
your shell's PATH or `KUBECONFIG`, and does not edit shell profiles. Interactive
setup asks before selecting the new kube-context; `--yes` selects it automatically.
CI runs that only need the installation's private kubeconfig can opt out:

```sh
bin/substrate-aks setup --config deploy.json --yes --skip-context-handoff
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

`doctor` checks local tools without Azure resource calls. Add `--config deploy.json`
to check the configured runtime assets or assembly prerequisites too.
Add `--azure --config deploy.json` for read-only cloud deployment checks.
VM SKU checks request only the configured region and follow all result pages;
permissions and quota are checked live rather than reused from a cache.
`plan` runs Azure Bicep what-if without deploying resources.

> [!WARNING]
> Registry provisioning starts billing. It creates the dedicated resource group,
> a Basic ACR, and an `AcrPush` grant for the upload identity. It does not create
> AKS or Blob storage yet.

Setup checks the image builder with the local tools, before any Azure discovery,
and offers a choice when both Docker/buildx and Podman work.
`--yes` prefers Docker/buildx, then Podman. An explicit `--container-engine`
never falls back to another engine. Reusing configured images needs no builder.
Builds target Linux/amd64; Podman can run rootless on Linux.
Before registry provisioning, Docker checks include bootstrapping the selected
buildx builder, with a separate two-minute startup limit. This may download or
start a BuildKit container. An unreachable or unhealthy builder triggers Podman
fallback in automatic mode, or an error when Docker was selected explicitly.

`images` builds the pinned source and publishes the Substrate, demo, and Azure
storage-plugin images. It assembles runtime assets unless `assetsDir` is set,
in which case it verifies the supplied files without overwriting them.
Image settings are saved only after publication and checksum checks succeed.

ACR authentication uses your Azure login. Registry credentials stay in the
private workspace rather than replacing your usual Docker credentials.
Docker context and buildx settings are preserved. If the CLI reports a workspace
context-store conflict, move that old store aside as instructed.

To check that saved images are still accessible:

```sh
bin/substrate-aks images --check --config deploy.json
```

`setup` and `install` also run this check. Authentication and network failures
are reported separately from missing images; the check never rebuilds images
or changes your config.

Failed builds can leave images in ACR without updating config. Fix the error
and retry. New role grants can take time to propagate; the uploader must use
the configured upload identity or belong to the configured publisher group.

Deployment commands refresh credentials for the owned ACR, so token expiry does
not require rebuilding images. Other registries use your existing credentials.

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
| `provision` | Create AKS, Blob containers, identities, and permissions; reuse the ACR |
| `check` | Verify node versions, microVM worker count, and required certificate APIs |
| `prepare` | Render Azure settings into the managed upstream source checkout |
| `install` | Verify images and KVM, upload runtime assets, and deploy Substrate |
| `status` | Show pods and persistent volume claims |
| `client` | Install the matching `kubectl ate` and print shell exports |

The cluster uses Azure CNI Overlay and Cilium network policy, with a separate
tainted microVM node pool. Automatic Kubernetes version upgrades are disabled to
preserve the supported 1.37 target. Node OS updates use AKS's `NodeImage` channel
with AKS-managed timing: patched images include security and bug fixes without
changing the Kubernetes version. See [configuration](configuration.md) for
identity and access settings.

Node image updates can drain/reimage nodes and interrupt workloads, especially
with the default single microVM worker. Automatic replacement and actor recovery
through this maintenance have not been validated; enabling updates is not a
zero-downtime or cross-node memory-restoration guarantee. Configure an AKS
maintenance window if you need controlled timing, and recheck KVM/runtime
readiness and actor restoration after maintenance. See
[AKS node OS update channels](https://learn.microsoft.com/azure/aks/auto-upgrade-node-os-image).
The template change takes effect only when infrastructure is provisioned again;
it does not update an existing cluster merely by downloading a new installer.

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

Keep Azure CLI and Azure's `kubelogin` on PATH and sign in to the cluster's tenant.
Credential refresh preserves the old kubeconfig if fetching or conversion fails.
`check` uses a structured Kubernetes SelfSubjectAccessReview for permission to
create CRDs, waiting up to five minutes for denied access to become allowed after
a new grant. This is authorization readiness, not proof that CRDs or workloads
are healthy. Authentication, network, malformed-response, and authorization
evaluation failures are reported directly.

### Optional kube-context handoff

Setup offers to import and select the installation in a single `KUBECONFIG`
file, or `~/.kube/config`. The interactive default is **No**; `--yes` accepts.
Use `--context-file PATH` to choose another file. Multi-file `KUBECONFIG` needs
an explicit destination unless handoff is skipped.

Use `--skip-context-handoff` to skip importing/selecting a context, even with
`--yes`. Setup still prepares the matching client and prints exports for the
private installation kubeconfig, without inspecting the user kubeconfig or
prompting for handoff. This option cannot be combined with `--context-file`.

Unrelated entries are preserved. Conflicting names, malformed files, symlinks,
and relative credential paths are refused. YAML comments and formatting are
not preserved. The private installation kubeconfig stays unchanged.
Imports use kubectl's exclusive `<destination>.lock` convention and recheck the
destination after consent. An existing writer lock is never removed. On a name
collision, choose an unused `--context-file` or deliberately resolve the named
conflict; retrying the same conflicting destination will not overwrite it.

To do this later:

```sh
bin/substrate-aks client --config deploy.json --use-context
# Or choose a destination:
bin/substrate-aks client --config deploy.json --use-context --context-file ~/.kube/substrate
```

Setup prints one set of shell exports after the context choice. Apply those
exports to connect. If handoff fails, the cluster is retained; rerun `client`
with a suitable destination.

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
| Progress and completion | stderr; plain updates when redirected |
| Standalone results/previews and final setup commands | stdout |
| Detailed command logs | Private files under the installation's `logs/` directory |
| `--verbose` | Underlying commands, build output, and diagnostics in append-only/standalone flows |

The interactive workspace owns the terminal: subprocess output never writes
over a form or approval. The logs view (`l`) renders a bounded, sanitized tail of
the current command's private log inside the workspace: commands, output, and
step markers, with terminal control sequences removed. Output marked sensitive
is never written to the log. The full file stays on disk, including with
`--verbose`. For an append-only verbose transcript, use
`setup --accessible --verbose`.

Failures show a short error and a log path. The success banner appears only after
client setup and any accepted context handoff succeed. Color is terminal-only;
`NO_COLOR=1` disables it.

```sh
bin/substrate-aks provision --registry-only --verbose
bin/substrate-aks status --verbose
```

The `plan` table distinguishes creates, deletions, and modifications with symbols
as well as color. Unchanged resources are omitted; property-level details are in
the logs and verbose output.

## Retry and teardown

**Failures retain resources.** Interactive setup offers to retry the failed
command. `--yes` and cancellation do not enter a retry loop. Follow the printed
stage-retry command, or rerun setup with the same config.

A new setup process checks the installation from the beginning; it does not
resume an arbitrary interrupted stage. Keep the installation ID and target
settings unchanged. Never discard the config while its resources still exist.

Ctrl-C, SIGTERM, and terminal hangups (SIGHUP) cancel local command process
groups and allow cleanup. The interactive workspace restores the terminal after
its owned subprocesses exit. A second termination signal exits immediately and
may leave temporary probes to remove manually. Interruption does not undo Azure's
server-side deployment; already-submitted deployments may continue, and resources
remain for retry or teardown.

To delete the installation:

```sh
bin/substrate-aks destroy --config deploy.json --confirm-delete substrate-dev
```

> [!CAUTION]
> Deletes the entire owned resource group: AKS, ACR and all its images,
> PostgreSQL data, snapshots, runtime assets, identities, and any unrelated
> resources added to the group. There is no automatic recovery.

The CLI checks the ownership tag and waits for Azure deletion. It does not adopt
or delete a group without the matching tag. An already-absent group is a safe
no-op. Local config, logs, subscription deployment history, and the shared client
remain.

After complete teardown, clear references to deleted images before reusing the
config:

```sh
bin/substrate-aks images --reset-config --config deploy.json
bin/substrate-aks setup --config deploy.json
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

| Area | Completed validation |
| --- | --- |
| Baseline | AKS 1.37 in `southcentralus`; one system and one microVM node, both `Standard_D4s_v3` |
| Setup | Private image publication, infrastructure, Substrate, runtime assets, demo, and client setup |
| Actor state | Suspend/resume preserved memory, file, and rootfs counters, advancing from 2 to 3 |
| Cold Blob restore | Memory restored on a new node with a compatible CPU model |
| Component operations | Image checks, cluster checks/status, a single-component reinstall, and demo redeployment |
| Managed Entra | Later `eastus2` installation; managed Entra, Azure RBAC, and disabled local accounts independently inspected |
| Actor-acceptance baseline | Author completed fresh cloud actor acceptance on `acae87e` |
| Podman | Author confirmed a real Podman-only build, publication, and installation |
| Later review fixes | Race-enabled tests, pinned contracts, Bicep compilation, and live read-only Azure preflight passed; no new full-install acceptance of these fixes |
| Teardown | Audit and inspected Entra installations deleted; main and managed node groups independently confirmed absent |
| Guided workspace | Real interactive `eastus2` installation: image publication, two approved infrastructure previews, Substrate, demo, matching client, and isolated context import succeeded; private and imported contexts reached Ready nodes; one final export block appeared after alternate-screen exit |
| Workspace teardown | Ownership-checked teardown succeeded; installation and managed node resource groups independently confirmed absent, with no related groups remaining |

The workspace acceptance used the first-pass binary. Subsequent form-navigation,
resize, compact-layout, and cancellation hardening passed race-enabled tests and
real Linux pseudo-terminal checks; it was not followed by another cloud install.

**The same VM size can have different CPU models.** A cold restore onto an
incompatible CPU reset memory while durable file/disk state survived. The
successful restore is not a guarantee of migration across heterogeneous nodes.

The audit's snapshot-reader role update needed a manual grant. The guard was
later fixed and regression-tested; clean live reconciliation of that update
remains unproven.

Remaining gaps include long-duration credential renewal, multi-operator use,
native macOS installation, automatic node-image replacement/recovery, broader interrupted
installation recovery, and other regions, SKUs, Kubernetes versions, and runtime
architectures.

Existing-cluster installation, managed PostgreSQL, upgrades, autoscaling,
dashboards, and shared filesystems are not part of this quickstart.
