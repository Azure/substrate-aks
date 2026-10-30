package main

import (
	"bufio"
	"context"
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/Azure/substrate-aks/internal/installer"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

type setupPrompt struct {
	in         io.Reader
	reader     *bufio.Reader
	ui         *setupUI
	accessible bool
}

type setupClusterNameInput struct {
	*huh.Input
	value  *string
	width  int
	resize func()
}

func newSetupClusterNameInput(value *string) *setupClusterNameInput {
	return &setupClusterNameInput{
		Input: huh.NewInput().Title("Dedicated resource group and cluster name").
			Placeholder("substrate-dev").Value(value).
			Validate(func(s string) error { return installer.ValidateInstallationName(strings.TrimSpace(s)) }),
		value: value, width: 80,
	}
}

func (i *setupClusterNameInput) WithWidth(width int) huh.Field {
	i.width = width
	i.Input.WithWidth(width)
	return i
}

func (i *setupClusterNameInput) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	_, cmd := i.Input.Update(msg)
	if i.resize != nil {
		i.resize()
	}
	return i, cmd
}

func (i *setupClusterNameInput) View() string {
	if i.Input.Error() != nil {
		return i.Input.View()
	}
	name := strings.TrimSpace(*i.value)
	// Before typing this is guidance, not an error; color follows validity.
	feedback, style := "✓ Valid cluster name.", lipgloss.NewStyle().Foreground(lipgloss.Color(setupOK))
	if name == "" {
		feedback, style = "Enter a cluster name.", lipgloss.NewStyle().Foreground(lipgloss.Color(setupMuted))
	} else if err := installer.ValidateInstallationName(name); err != nil {
		feedback, style = "✗ Invalid name: "+err.Error(), lipgloss.NewStyle().Foreground(lipgloss.Color(setupFail))
	}
	lines := strings.Split(ansi.Wrap(feedback, max(1, i.width-2), ""), "\n")
	for n, line := range lines {
		lines[n] = "  " + style.Render(line)
	}
	return i.Input.View() + "\n" + strings.Join(lines, "\n")
}

func setupFormTheme(isDark bool) *huh.Styles {
	theme := huh.ThemeCharm(isDark)
	// Huh uses Lip Gloss v2; v1 colors resolve through a different renderer.
	// Prompts use the primary purple (setupAccent) with a deeper border.
	accent := color.RGBA{R: 0xb4, G: 0x8c, B: 0xf2, A: 0xff}
	deep := color.RGBA{R: 0x7c, G: 0x5c, B: 0xc4, A: 0xff}
	muted := ansi.BrightBlack
	// Charm's blurred styles are copied before these overrides, so recolor
	// both states; otherwise inactive fields keep Charm's pink/green/indigo.
	for _, field := range []*huh.FieldStyles{&theme.Focused, &theme.Blurred} {
		field.Title = field.Title.Foreground(accent)
		field.NoteTitle = field.NoteTitle.Foreground(accent)
		field.SelectSelector = field.SelectSelector.Foreground(accent)
		field.NextIndicator = field.NextIndicator.Foreground(accent)
		field.PrevIndicator = field.PrevIndicator.Foreground(accent)
		field.MultiSelectSelector = field.MultiSelectSelector.Foreground(accent)
		field.SelectedOption = field.SelectedOption.Foreground(accent).Bold(true)
		field.SelectedPrefix = field.SelectedPrefix.Foreground(accent)
		field.TextInput.Cursor = field.TextInput.Cursor.Foreground(accent)
		field.TextInput.Prompt = field.TextInput.Prompt.Foreground(accent)
		field.FocusedButton = field.FocusedButton.
			Background(accent).Foreground(color.RGBA{R: 0x18, G: 0x19, B: 0x26, A: 0xff})
		field.Next = field.FocusedButton
	}
	theme.Focused.Base = theme.Focused.Base.BorderForeground(deep)
	theme.Focused.Card = theme.Focused.Base
	theme.Blurred.Title = theme.Blurred.Title.Foreground(muted)
	theme.Blurred.SelectSelector = theme.Blurred.SelectSelector.Foreground(muted)
	theme.Blurred.TextInput.Prompt = theme.Blurred.TextInput.Prompt.Foreground(muted)
	theme.Group.Title = theme.Focused.Title
	return theme
}

func newSetupPrompt(in io.Reader, ui *setupUI, accessible bool) *setupPrompt {
	file, terminal := in.(*os.File)
	terminal = terminal && term.IsTerminal(file.Fd())
	output, outputTerminal := ui.out.(interface{ Fd() uintptr })
	outputTerminal = outputTerminal && term.IsTerminal(output.Fd())
	accessible = accessible || !terminal || !outputTerminal || os.Getenv("TERM") == "dumb"
	return &setupPrompt{in: in, reader: bufio.NewReader(in), ui: ui, accessible: accessible}
}

// Accessible prompts use fresh scanners internally. Limit reads to one byte so
// one prompt cannot consume another's answer, and surface EOF instead of defaults.
type setupInput struct {
	reader *bufio.Reader
	ctx    context.Context
	line   []byte
	err    error
}

func (in *setupInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if in.err != nil {
		return 0, in.err
	}
	if len(in.line) == 0 {
		type result struct {
			line string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			line, err := in.reader.ReadString('\n')
			done <- result{line, err}
		}()
		select {
		case <-in.ctx.Done():
			in.err = in.ctx.Err()
			return 0, in.err
		case r := <-done:
			in.line, in.err = []byte(r.line), r.err
			if len(in.line) == 0 {
				return 0, in.err
			}
		}
	}
	p[0] = in.line[0]
	in.line = in.line[1:]
	return 1, nil
}

func (p *setupPrompt) run(ctx context.Context, field huh.Field) error {
	return p.runForm(ctx, nil, field)
}

func (p *setupPrompt) runForm(ctx context.Context, resize func(height int), fields ...huh.Field) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	form := huh.NewForm(huh.NewGroup(fields...))
	if p.ui.session != nil {
		return p.ui.session.prompt(ctx, form, resize)
	}
	if colorProfile(p.ui.out) == termenv.Ascii {
		form.WithTheme(huh.ThemeFunc(huh.ThemeBase)).
			WithProgramOptions(tea.WithColorProfile(colorprofile.NoTTY))
	}
	form.WithInput(p.in).WithOutput(p.ui.out).WithWidth(p.ui.width).
		WithAccessible(p.accessible)
	if !p.accessible {
		if err := form.RunWithContext(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("setup input: %w", err)
		}
		return nil
	}
	input := &setupInput{reader: p.reader, ctx: ctx}
	err := form.WithInput(input).RunWithContext(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if input.err != nil {
		return fmt.Errorf("reading setup input: %w", input.err)
	}
	return err
}

func (p *setupPrompt) text(ctx context.Context, label, fallback string) (string, error) {
	value := fallback
	title := label
	if p.accessible && fallback != "" {
		title += " [" + fallback + "]"
	}
	err := p.run(ctx, huh.NewInput().Title(title).Value(&value).Validate(func(s string) error {
		if strings.TrimSpace(s) == "" && fallback == "" {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}))
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	return value, err
}

func (p *setupPrompt) region(ctx context.Context, fallback string, regions []string) (string, error) {
	value := fallback
	if p.accessible {
		err := p.run(ctx, huh.NewInput().Title("Azure region ["+fallback+"]").Value(&value).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					s = fallback
				}
				_, err := validateAzureRegion(s, regions)
				return err
			}))
		if err != nil {
			return "", err
		}
		return validateAzureRegion(value, regions)
	}
	err := p.run(ctx, newRegionSelector(&value, regions))
	return value, err
}

const (
	setupSnapshotQuestion = "Grant this Azure identity read-only snapshot inspection?"
	setupSnapshotWarning  = "Optional; runtime access is automatic. Snapshots can contain actor memory, files, and secrets."
)

// installation asks for the name and region and, when reader is non-nil, the
// optional snapshot inspection grant, all in one form.
func (p *setupPrompt) installation(ctx context.Context, name, location string, regions []string, reader *bool) (string, string, error) {
	nameField := newSetupClusterNameInput(&name)
	if p.accessible {
		fallback := strings.TrimSpace(name)
		if fallback != "" {
			nameField.Title("Dedicated resource group and cluster name [" + fallback + "]")
		}
		nameField.Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if s == "" {
				s = fallback
			}
			return installer.ValidateInstallationName(s)
		})
		err := p.run(ctx, nameField)
		if err != nil {
			return "", "", err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			name = fallback
		}
		location, err := p.region(ctx, location, regions)
		if err != nil || reader == nil {
			return name, location, err
		}
		p.ui.warning(setupSnapshotWarning)
		*reader, err = p.confirm(ctx, setupSnapshotQuestion, *reader)
		return name, location, err
	}
	region := newRegionSelector(&location, regions)
	fields := []huh.Field{nameField, region}
	var snapshot huh.Field
	if reader != nil {
		snapshot = huh.NewConfirm().Title(setupSnapshotQuestion).
			Description(setupSnapshotWarning).Value(reader)
		fields = append(fields, snapshot)
	}
	formHeight := 0
	nameField.resize = func() {
		if formHeight == 0 {
			return
		}
		rest := lipgloss.Height(nameField.View()) + 1
		if snapshot != nil {
			rest += lipgloss.Height(snapshot.View()) + 1
		}
		region.WithHeight(max(4, formHeight-rest))
	}
	err := p.runForm(ctx, func(height int) {
		formHeight = height
		nameField.resize()
	}, fields...)
	return strings.TrimSpace(name), location, err
}

func (p *setupPrompt) confirm(ctx context.Context, label string, fallback bool) (bool, error) {
	value := fallback
	err := p.run(ctx, huh.NewConfirm().Title(label).Value(&value))
	return value, err
}

func (p *setupPrompt) containerEngine(ctx context.Context) (string, error) {
	value := "docker"
	if p.accessible {
		err := p.run(ctx, huh.NewInput().Title("Image builder: docker or podman [docker]").Value(&value).
			Validate(func(s string) error {
				switch strings.TrimSpace(s) {
				case "", "docker", "podman":
					return nil
				default:
					return fmt.Errorf("choose docker or podman")
				}
			}))
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = "docker"
		}
		return value, nil
	}
	field := huh.NewSelect[string]().Title("Image builder").
		Description("Both engines are available. Go images use ko with either choice.").
		Options(huh.NewOption("Docker (buildx)", "docker"), huh.NewOption("Podman", "podman")).
		Value(&value)
	if p.ui.session != nil {
		field.Title("Choose an engine").Description("Go images use ko with either engine.")
	}
	err := p.run(ctx, field)
	return value, err
}
