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

// setupWordmark draws "agent substrate" with thin rounded box-drawing letters
// in Agent Substrate's green, lightening toward the start.
func setupWordmark(r *lipgloss.Renderer, words ...string) string {
	glyphs := map[rune][3]string{
		's': {"╭─╴", "╰─╮", "╶─╯"}, 'u': {"╷ ╷", "│ │", "╰─╯"}, 'b': {"┌─╮", "├─┤", "└─╯"},
		't': {"╶┬╴", " │ ", " ╵ "}, 'r': {"┌─╮", "├┬╯", "╵╰╴"}, 'a': {"╭─╮", "├─┤", "╵ ╵"},
		'e': {"┌─╴", "├─ ", "└─╴"}, 'g': {"╭─╴", "│╶╮", "╰─╯"}, 'n': {"┌─╮", "│ │", "╵ ╵"},
		' ': {" ", " ", " "},
	}
	if len(words) == 0 {
		words = []string{"agent substrate"}
	}
	text := strings.Join(words, " ")
	letters := len(strings.ReplaceAll(text, " ", ""))
	var rows [3]string
	i := 0
	for n, letter := range text {
		color := setupSubstrate
		if letters > 1 {
			color = blendHex("#b9f0bc", "#33a946", float64(i)/float64(letters-1))
		}
		style := r.NewStyle().Foreground(lipgloss.Color(color))
		for row := range rows {
			if n > 0 {
				rows[row] += " "
			}
			rows[row] += style.Render(glyphs[letter][row])
		}
		if letter != ' ' {
			i++
		}
	}
	return strings.Join(rows[:], "\n")
}

// setupAKS draws "AKS" two rows tall in half-block pixels, a step up from body
// text without competing with the wordmark.
func setupAKS(r *lipgloss.Renderer) string {
	glyphs := [][4]string{
		{".##.", "#..#", "####", "#..#"},
		{"#..#", "#.#.", "###.", "#..#"},
		{".###", "##..", "..##", "###."},
	}
	pixels := make([][]string, 4)
	for y := range pixels {
		for i, glyph := range glyphs {
			if i > 0 {
				pixels[y] = append(pixels[y], "")
			}
			for _, bit := range glyph[y] {
				color := ""
				if bit == '#' {
					color = setupAccent
				}
				pixels[y] = append(pixels[y], color)
			}
		}
	}
	return setupHalfBlocks(r, pixels)
}

// Actor colors and states follow ate.dev's live actor grid.
var setupActorColors = []string{"#61d76b", "#64b9de", "#f3f17b", "#e77cb2"}

const (
	setupMarkCells = 5
	setupMarkFace  = "#222b25"
	setupMarkIdle  = "#6b7079"
)

// setupMarkCell reports a top-face agent's color at an animation frame. Each
// agent cycles independently: idle, resuming, running, suspending.
func setupMarkCell(cell, frame int) string {
	hash := func(v int) int {
		v = (v ^ 61) ^ (v >> 16)
		v *= 9
		v ^= v >> 4
		v *= 0x27d4eb2d
		v ^= v >> 15
		return v & 0x7fffffff
	}
	period := 26 + hash(cell*7+1)%22
	t := frame + hash(cell*13+5)%period
	cycle, phase := t/period, t%period
	running := 6 + hash(cell*31+cycle)%8
	color := setupActorColors[hash(cell*17+cycle*5)%len(setupActorColors)]
	// Agents fade in from the idle grey layer and back out.
	switch {
	case phase < 2:
		return blendHex(setupMarkIdle, color, float64(phase+1)/3)
	case phase < 2+running:
		return color
	case phase < 4+running:
		return blendHex(setupMarkIdle, color, float64(4+running-phase)/3)
	default:
		return setupMarkIdle
	}
}

// setupMark draws the isometric stack: agents lighting up on the top face, an
// Agent Substrate layer, and an AKS base. The shading needs at least 256 colors.
func setupMark(r *lipgloss.Renderer, frame int) string {
	// Sixteen-color terminals collapse the shaded faces into solid blocks.
	if r.ColorProfile() > termenv.ANSI256 {
		return ""
	}
	const cells, layer = setupMarkCells, 3
	// The top face is exactly the agent lattice: two-pixel agents in a
	// checkerboard whose outermost agents form the face's edges.
	width, face, center := 4*cells-2, 2*cells-1, 2*cells-1
	pixels := make([][]string, face+2*layer)
	for y := range pixels {
		pixels[y] = make([]string, width)
	}
	bottom := make([]int, width)
	for y := range face {
		reach := 2 * min(y, face-1-y)
		for x := center - reach - 1; x <= center+reach; x++ {
			pixels[y][x] = setupMarkFace
			bottom[x] = y
		}
	}
	for x := range width {
		for s := range 2 * layer {
			color := setupSubstrate
			if s >= layer {
				color = "#9b6cf0"
			}
			if x >= center {
				color = shadeHex(color, 0.68)
			}
			if y := bottom[x] + 1 + s; y < len(pixels) {
				pixels[y][x] = color
			}
		}
	}
	for i := range cells {
		for j := range cells {
			x, y := center+2*(i-j)-1, i+j
			color := setupMarkCell(i*cells+j, frame)
			pixels[y][x], pixels[y][x+1] = color, color
			// Every agent is a voxel: its lit and shaded sides fill the row
			// below, so idle agents form a grey layer over Agent Substrate.
			pixels[y+1][x], pixels[y+1][x+1] = shadeHex(color, 0.72), shadeHex(color, 0.5)
		}
	}
	return setupHalfBlocks(r, pixels)
}

// setupHalfBlocks renders square pixels two to a cell; "" is transparent.
func setupHalfBlocks(r *lipgloss.Renderer, pixels [][]string) string {
	if len(pixels)%2 == 1 {
		pixels = append(pixels, make([]string, len(pixels[0])))
	}
	var lines []string
	for y := 0; y < len(pixels); y += 2 {
		var line strings.Builder
		for x := range pixels[y] {
			upper, lower := pixels[y][x], pixels[y+1][x]
			switch {
			case upper != "" && upper == lower:
				line.WriteString(r.NewStyle().Foreground(lipgloss.Color(upper)).Render("█"))
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

func hexRGB(hex string) (red, green, blue int) {
	fmt.Sscanf(hex, "#%02x%02x%02x", &red, &green, &blue)
	return
}

func shadeHex(hex string, factor float64) string {
	red, green, blue := hexRGB(hex)
	scale := func(v int) int { return min(255, int(float64(v)*factor)) }
	return fmt.Sprintf("#%02x%02x%02x", scale(red), scale(green), scale(blue))
}

// blendHex mixes from toward to; amount 0 is from and 1 is to.
func blendHex(from, to string, amount float64) string {
	r1, g1, b1 := hexRGB(from)
	r2, g2, b2 := hexRGB(to)
	mix := func(a, b int) int { return a + int(float64(b-a)*amount+0.5) }
	return fmt.Sprintf("#%02x%02x%02x", mix(r1, r2), mix(g1, g2), mix(b1, b2))
}

// setupOn is the plain, muted "on" between the wordmark and AKS.
func setupOn(r *lipgloss.Renderer) string {
	return newSetupStyles(r).muted.Render("on")
}

// setupStack centers brand lines on one axis with "on" spaced evenly between
// the wordmark and AKS. Joining them at once keeps odd widths from drifting.
func setupStack(r *lipgloss.Renderer, wordmark string, rest ...string) string {
	return lipgloss.JoinVertical(lipgloss.Center, append([]string{wordmark, "", setupOn(r), "", setupAKS(r)}, rest...)...)
}

// setupBrand is the full landing mark: the animated block beside the wordmark,
// "on AKS" and the tagline. Frame drives the agents on the block's top face.
func setupBrand(r *lipgloss.Renderer, frame int) string {
	st := newSetupStyles(r)
	text := setupStack(r, setupWordmark(r), "", st.muted.Render("An AKS packaging layer for Agent Substrate."))
	mark := setupMark(r, frame)
	if mark == "" {
		return text
	}
	center := func(block string, height int) string {
		return strings.Repeat("\n", max(0, (height-lipgloss.Height(block))/2)) + block
	}
	height := max(lipgloss.Height(mark), lipgloss.Height(text))
	return lipgloss.JoinHorizontal(lipgloss.Top, center(mark, height), "  ", center(text, height))
}

// setupBadge is the compact one-line brand used by headers and receipts.
func setupBadge(r *lipgloss.Renderer) string {
	st := newSetupStyles(r)
	return st.substrate.Render("◆ Agent Substrate") + st.muted.Render(" on ") + st.aks.Render("AKS")
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
