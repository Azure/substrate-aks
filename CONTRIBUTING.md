# Contributing

Thanks for your interest in Agent Substrate on AKS. Contributions are covered by the
Microsoft Contributor License Agreement and Code of Conduct; see
[Contributing](README.md#contributing) in the README. Report security issues
as described in [SECURITY.md](SECURITY.md), never in public issues.

Bug reports and feature requests go in
[GitHub Issues](https://github.com/Azure/substrate-aks/issues); see
[SUPPORT.md](SUPPORT.md) for what to include.

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
| `make test-contract` | Compatibility with disposable checkouts of the exact Substrate and plugin pins |
| `make test` | Unit tests and contracts |
| `make bicep` | Compile the infrastructure template |

Contract tests need Git, Go, `kubectl`, and network access to the Substrate and
plugin sources, module downloads, `ghcr.io`, and the microVM asset releases on
GitHub. Missing prerequisites fail the tests rather than skipping them.
No check above provisions Azure resources.

To use existing pristine checkouts of the pins:

```sh
go test -race -tags=integration ./internal/installer \
  -run 'TestPinned.*Contract' -count=1 -args \
  -contract-root /absolute/path/to/pinned/substrate \
  -plugin-contract-root /absolute/path/to/pinned/objectstore-azure-blob
```

The contracts render the plugin's own kustomize components into the pinned
manifests, run the pinned `ate-setup` with every flag the installer passes,
verify the published release inventory, and assemble the pinned runtime assets.
The image-check tests use local registries, and publisher tests use controlled
engine commands. They do not prove live ACR access or real image builds.

### Extending guided setup

Feature work should extend the operation flow, not the terminal renderer. The
files below are in [`cmd/substrate-aks`](cmd/substrate-aks):

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

## Updating the README images

The images in [`docs/images`](docs/images) are recorded from the installer's
simulated tour (`zz_tour_linux_test.go`), which runs the real setup workspace
with Azure operations stubbed out. Regenerate them after visible changes to the
welcome screen or the setup flow. You need Linux,
[VHS](https://github.com/charmbracelet/vhs), `ffmpeg`, `gifsicle`, and
`pngquant`.

```sh
mkdir -p /tmp/substrate-aks-tour/bin
go test -c -tags tour -o /tmp/substrate-aks-tour/bin/substrate-aks ./cmd/substrate-aks
cp docs/images/*.tape /tmp/substrate-aks-tour/
cd /tmp/substrate-aks-tour
vhs hero.tape    # hero.mp4, at twice the terminal size for a sharp crop
vhs setup.tape   # setup.mp4 and setup-preview-full.png
```

The tour binary is named `substrate-aks` so the printed commands read naturally.
Crop and convert the recordings, then copy the results into `docs/images`:

```sh
palette='split[a][b];[a]palettegen=max_colors=64:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle'
# Hero: the logo, title, and tagline, at 1880x480.
ffmpeg -ss 0.5 -t 6 -i hero.mp4 -vf "crop=1880:480:300:246,fps=12,$palette" -loop 0 hero.gif
# Setup walkthrough, at the recorded 1240x780.
ffmpeg -i setup.mp4 -vf "fps=10,$palette" -loop 0 setup.gif
gifsicle -O3 -b hero.gif setup.gif
# Infrastructure preview: the top of the screenshot VHS took.
ffmpeg -i setup-preview-full.png -vf crop=1240:600:0:0 setup-preview-crop.png
pngquant --quality=70-95 --output setup-preview.png setup-preview-crop.png
```

Check that the crops still frame the content if the layout changed, and keep
`setup.gif` well under 5 MB so GitHub renders it inline.
