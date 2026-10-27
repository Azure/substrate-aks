package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

type setupPrompt struct {
	in         io.Reader
	reader     *bufio.Reader
	ui         *setupUI
	accessible bool
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
	if err := ctx.Err(); err != nil {
		return err
	}
	form := huh.NewForm(huh.NewGroup(field))
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
	err := p.run(ctx, huh.NewSelect[string]().Title("Image builder").
		Description("Both engines are available. Go images use ko with either choice.").
		Options(huh.NewOption("Docker (buildx)", "docker"), huh.NewOption("Podman", "podman")).
		Value(&value))
	return value, err
}
