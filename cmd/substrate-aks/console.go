package main

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

type console struct {
	logger   *log.Logger
	heading  *log.Logger
	success  *log.Logger
	spinner  lipgloss.Style
	renderer *lipgloss.Renderer
	animated bool
}

func newConsole(out io.Writer) *console {
	logger := log.NewWithOptions(out, log.Options{Level: log.DebugLevel})
	profile := colorProfile(out)
	logger.SetColorProfile(profile)
	renderer := lipgloss.NewRenderer(out)
	renderer.SetColorProfile(profile)
	styles := log.DefaultStyles()
	styles.Levels[log.DebugLevel] = levelStyle("INFO", "8")
	styles.Levels[log.InfoLevel] = levelStyle("STEP", "6")
	styles.Levels[log.WarnLevel] = levelStyle("WARN", "3")
	styles.Levels[log.ErrorLevel] = levelStyle("ERROR", "1")
	logger.SetStyles(styles)

	heading := logger.With()
	headingStyles := log.DefaultStyles()
	headingStyles.Message = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	heading.SetStyles(headingStyles)

	success := logger.With()
	successStyles := log.DefaultStyles()
	successStyles.Levels[log.InfoLevel] = levelStyle("DONE", "2")
	success.SetStyles(successStyles)
	return &console{
		logger: logger, heading: heading, success: success, renderer: renderer,
		spinner:  renderer.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		animated: profile != termenv.Ascii,
	}
}

func levelStyle(label, color string) lipgloss.Style {
	return lipgloss.NewStyle().SetString(label).Bold(true).Width(5).Foreground(lipgloss.Color(color))
}

func colorProfile(out io.Writer) termenv.Profile {
	if file, ok := out.(interface{ Fd() uintptr }); ok &&
		term.IsTerminal(file.Fd()) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" {
		return termenv.NewOutput(out).EnvColorProfile()
	}
	return termenv.Ascii
}
