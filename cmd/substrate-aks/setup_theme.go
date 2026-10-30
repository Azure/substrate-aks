package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// Setup palette. Green is Agent Substrate's brand. Purple is AKS and the
// primary color: where you are and what needs you (rail position, selection,
// prompts, keys). Azure blue is live work: the progress bar and the running
// check. Status colors stay on ANSI indexes
// so they follow the user's terminal theme; the rest downsample automatically.
const (
	setupSubstrate  = "#64d86b"
	setupAccent     = "#b48cf2"
	setupActive     = "#3b9cf0"
	setupActiveSoft = "#9ccbf5"
	setupOK         = "2"
	setupWarn       = "3"
	setupFail       = "1"
	setupMuted      = "8"
)

type setupStyles struct {
	// accent is the primary purple; active marks running work and soft is its
	// secondary detail text.
	accent, accentBold, active, activeBold, soft, substrate, aks, ok, warn, fail, muted, bold, key lipgloss.Style
}

func newSetupStyles(r *lipgloss.Renderer) setupStyles {
	s := r.NewStyle()
	return setupStyles{
		accent:     s.Foreground(lipgloss.Color(setupAccent)),
		accentBold: s.Bold(true).Foreground(lipgloss.Color(setupAccent)),
		active:     s.Foreground(lipgloss.Color(setupActive)),
		activeBold: s.Bold(true).Foreground(lipgloss.Color(setupActive)),
		soft:       s.Foreground(lipgloss.Color(setupActiveSoft)),
		substrate:  s.Bold(true).Foreground(lipgloss.Color(setupSubstrate)),
		aks:        s.Bold(true).Foreground(lipgloss.Color(setupAccent)),
		ok:         s.Foreground(lipgloss.Color(setupOK)),
		warn:       s.Foreground(lipgloss.Color(setupWarn)),
		fail:       s.Foreground(lipgloss.Color(setupFail)),
		muted:      s.Foreground(lipgloss.Color(setupMuted)),
		bold:       s.Bold(true),
		key:        s.Bold(true).Foreground(lipgloss.Color(setupAccent)),
	}
}

// setupWordmark draws "substrate" with thin rounded box-drawing letters.
func setupWordmark(r *lipgloss.Renderer) string {
	glyphs := map[rune][3]string{
		's': {"╭─╴", "╰─╮", "╶─╯"}, 'u': {"╷ ╷", "│ │", "╰─╯"}, 'b': {"┌─╮", "├─┤", "└─╯"},
		't': {"╶┬╴", " │ ", " ╵ "}, 'r': {"┌─╮", "├┬╯", "╵╰╴"}, 'a': {"╭─╮", "├─┤", "╵ ╵"},
		'e': {"┌─╴", "├─ ", "└─╴"},
	}
	gradient := []string{"#b9f0bc", "#a6ebaa", "#93e698", "#80e086", "#71dc78", "#64d86b", "#55c95d", "#45b94f", "#33a946"}
	var rows [3]string
	for i, letter := range "substrate" {
		style := r.NewStyle().Foreground(lipgloss.Color(gradient[i]))
		for row := range rows {
			if i > 0 {
				rows[row] += " "
			}
			rows[row] += style.Render(glyphs[letter][row])
		}
	}
	return strings.Join(rows[:], "\n")
}

// setupMark draws an isometric block of stacked strata: an Agent Substrate
// layer on an AKS layer on an Azure base. The shading needs at least 256 colors.
func setupMark(r *lipgloss.Renderer) string {
	// Sixteen-color terminals collapse the shaded faces into solid blocks.
	if r.ColorProfile() > termenv.ANSI256 {
		return ""
	}
	const half, sides = 6, 6
	width := 2 * half
	top := "#d6f7d8"
	bands := []string{"#64d86b", "#9061e3", "#2f7fe0"}
	pixels := make([][]string, half+sides)
	for y := range pixels {
		pixels[y] = make([]string, width)
	}
	center := float64(width) / 2
	for y := range half {
		reach := 2 * (y + 1)
		if y >= half/2 {
			reach = 2 * (half - y)
		}
		for x := range width {
			if abs(float64(x)+0.5-center) <= float64(reach) {
				pixels[y][x] = top
			}
		}
	}
	for x := range width {
		bottom := half - 1 - int(abs(float64(x)+0.5-center)/2)
		for s := range sides {
			color := bands[min(s/(sides/len(bands)), len(bands)-1)]
			if float64(x) >= center {
				color = shadeHex(color, 0.7)
			}
			if y := bottom + 1 + s; y < len(pixels) {
				pixels[y][x] = color
			}
		}
	}
	var lines []string
	for y := 0; y < len(pixels); y += 2 {
		var line strings.Builder
		for x := range width {
			upper, lower := pixels[y][x], pixels[y+1][x]
			switch {
			case upper != "" && lower != "":
				line.WriteString(r.NewStyle().Foreground(lipgloss.Color(upper)).Background(lipgloss.Color(lower)).Render("▀"))
			case upper != "":
				line.WriteString(r.NewStyle().Foreground(lipgloss.Color(upper)).Render("▀"))
			case lower != "":
				line.WriteString(r.NewStyle().Foreground(lipgloss.Color(lower)).Render("▄"))
			default:
				line.WriteString(" ")
			}
		}
		lines = append(lines, line.String())
	}
	return strings.Join(lines, "\n")
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func shadeHex(hex string, factor float64) string {
	var red, green, blue int
	fmt.Sscanf(hex, "#%02x%02x%02x", &red, &green, &blue)
	scale := func(v int) int { return min(255, int(float64(v)*factor)) }
	return fmt.Sprintf("#%02x%02x%02x", scale(red), scale(green), scale(blue))
}

// setupBrand is the full landing mark: strata block, wordmark and tagline.
func setupBrand(r *lipgloss.Renderer) string {
	st := newSetupStyles(r)
	text := setupWordmark(r) + "\n" + st.muted.Render("on ") + st.aks.Render("AKS") + "\n" +
		st.muted.Render("An AKS packaging layer for Agent Substrate.")
	mark := setupMark(r)
	if mark == "" {
		return text
	}
	pad := max(0, (lipgloss.Height(mark)-lipgloss.Height(text))/2)
	return lipgloss.JoinHorizontal(lipgloss.Top, mark, "   ", strings.Repeat("\n", pad)+text)
}

// setupBadge is the compact one-line brand used by headers and receipts.
func setupBadge(r *lipgloss.Renderer) string {
	st := newSetupStyles(r)
	return st.substrate.Render("◆ substrate") + st.muted.Render(" on ") + st.aks.Render("AKS")
}

type setupKeyHint struct{ key, label string }

// setupKeys renders "key label · key label" hints; the trailing hint is right
// aligned when there is room so the stop/exit action has a stable home.
func setupKeys(st setupStyles, width int, hints []setupKeyHint, trailing setupKeyHint) string {
	render := func(h setupKeyHint) string { return st.key.Render(h.key) + " " + st.muted.Render(h.label) }
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, render(hint))
	}
	left := strings.Join(parts, st.muted.Render("  ·  "))
	if trailing.key == "" {
		return ansi.Truncate(left, width, "")
	}
	right := render(trailing)
	if gap := width - ansi.StringWidth(left) - ansi.StringWidth(right) - 1; gap >= 3 {
		return left + strings.Repeat(" ", gap) + right
	}
	if left == "" {
		return right
	}
	return ansi.Truncate(right+st.muted.Render("  ·  ")+left, width, "")
}

// setupOverlay draws box centered on top of background, preserving the
// background on either side of each overlaid row.
func setupOverlay(background, box string, width, height int) string {
	lines := strings.Split(background, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	boxLines := strings.Split(box, "\n")
	boxWidth := lipgloss.Width(box)
	x, y := max(0, (width-boxWidth)/2), max(0, (height-len(boxLines))/2)
	for i, row := range boxLines {
		if y+i >= len(lines) {
			break
		}
		line := lines[y+i]
		left := ansi.Truncate(line, x, "")
		left += strings.Repeat(" ", max(0, x-ansi.StringWidth(left)))
		right := ansi.TruncateLeft(line, x+boxWidth, "")
		lines[y+i] = left + row + right
	}
	return strings.Join(lines, "\n")
}

func setupDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// setupAlign places right at the end of a width-wide row when it fits.
func setupAlign(left, right string, width int) string {
	if ansi.StringWidth(right) == 0 {
		return left
	}
	if gap := width - ansi.StringWidth(left) - ansi.StringWidth(right); gap >= 2 {
		return left + strings.Repeat(" ", gap) + right
	}
	return left + "  " + right
}
