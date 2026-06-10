// Package logo renders a Trapeze wordmark in a stylized way.
package logo

import (
	"fmt"
	"image/color"
	"math/rand/v2"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/amarbel-llc/trapeze/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// letterform represents a letterform. It can be stretched horizontally by
// a given amount via the boolean argument.
type letterform func(bool) string

const diag = `╱`

// Opts are the options for rendering the Trapeze title art.
type Opts struct {
	FieldColor   color.Color // diagonal lines
	TitleColorA  color.Color // left gradient ramp point
	TitleColorB  color.Color // right gradient ramp point
	CharmColor   color.Color // Charm™ text color
	VersionColor color.Color // version text color
	Width        int         // width of the rendered logo, used for truncation
	Hyper        bool        // whether it is Trapeze or Hypertrapeze

	// When true, stretch a random letterform on each render. Has no effect in
	// compact mode. Mainly for testing. In production you will want to cache
	// the stretched letterform to keep the logo from jittering on resize.
	Unstable bool
}

// Render renders the Trapeze logo. Set the argument to true to render the narrow
// version, intended for use in a sidebar.
//
// The compact argument determines whether it renders compact for the sidebar
// or wider for the main pane.
func Render(base lipgloss.Style, version string, compact bool, o Opts) string {
	charm := "Charm™"
	if !o.Hyper {
		charm = " " + charm
	}

	fg := func(c color.Color, s string) string {
		return lipgloss.NewStyle().Foreground(c).Render(s)
	}

	// Title.
	const spacing = 1
	var hyperLetterforms []letterform
	if o.Hyper {
		hyperLetterforms = []letterform{
			LetterH,
			LetterYAlt,
			LetterP,
			LetterE,
			LetterR,
		}
	}
	trapezeLetterforms := []letterform{
		LetterT,
		LetterR,
		LetterA,
		LetterP,
		LetterE,
		LetterZ,
		LetterEAlt,
	}
	if o.Hyper && !compact {
		trapezeLetterforms = append(hyperLetterforms, trapezeLetterforms...)
	}

	stretchIndex := -1 // -1 means no stretching.
	if !compact && !o.Unstable {
		// Always stretch the same letterform, which is picked once at random.
		stretchIndex = cachedRandN(len(trapezeLetterforms))
	} else if !compact && o.Unstable {
		// Stretch a random letterform on every render.
		stretchIndex = rand.IntN(len(trapezeLetterforms))
	}
	trapeze := renderWord(spacing, stretchIndex, trapezeLetterforms...)
	if o.Hyper && compact {
		trapeze = renderWord(spacing, stretchIndex, hyperLetterforms...) + "\n" + trapeze
	}
	trapezeWidth := lipgloss.Width(trapeze)
	b := new(strings.Builder)
	for r := range strings.SplitSeq(trapeze, "\n") {
		fmt.Fprintln(b, styles.ApplyForegroundGrad(base, r, o.TitleColorA, o.TitleColorB))
	}
	trapeze = b.String()

	// Charm and version.
	metaRowGap := 1
	maxVersionWidth := trapezeWidth - lipgloss.Width(charm) - metaRowGap
	version = ansi.Truncate(version, maxVersionWidth, "…") // truncate version if too long.
	if o.Hyper && compact {
		version += " "
	}
	gap := max(0, trapezeWidth-lipgloss.Width(charm)-lipgloss.Width(version))
	metaRow := fg(o.CharmColor, charm) + strings.Repeat(" ", gap) + fg(o.VersionColor, version)

	// Join the meta row and big Trapeze title.
	trapeze = strings.TrimSpace(metaRow + "\n" + trapeze)

	// Narrow version. If this is Hypertrapeze, this is also a stacked version.
	if compact {
		field := fg(o.FieldColor, strings.Repeat(diag, trapezeWidth))
		return strings.Join([]string{field, field, trapeze, field, ""}, "\n")
	}

	fieldHeight := lipgloss.Height(trapeze)

	// Left field.
	const leftWidth = 6
	leftFieldRow := fg(o.FieldColor, strings.Repeat(diag, leftWidth))
	leftField := new(strings.Builder)
	for range fieldHeight {
		fmt.Fprintln(leftField, leftFieldRow)
	}

	// Right field.
	rightWidth := max(15, o.Width-trapezeWidth-leftWidth-2) // 2 for the gap.
	const stepDownAt = 0
	rightField := new(strings.Builder)
	for i := range fieldHeight {
		width := rightWidth
		if i >= stepDownAt {
			width = rightWidth - (i - stepDownAt)
		}
		fmt.Fprint(rightField, fg(o.FieldColor, strings.Repeat(diag, width)), "\n")
	}

	// Return the wide version.
	const hGap = " "
	logo := lipgloss.JoinHorizontal(lipgloss.Top, leftField.String(), hGap, trapeze, hGap, rightField.String())
	if o.Width > 0 {
		// Truncate the logo to the specified width.
		lines := strings.Split(logo, "\n")
		for i, line := range lines {
			lines[i] = ansi.Truncate(line, o.Width, "")
		}
		logo = strings.Join(lines, "\n")
	}
	return logo
}

// SmallRender renders a smaller version of the Trapeze logo, suitable for
// smaller windows or sidebar usage.
func SmallRender(t *styles.Styles, width int, o Opts) string {
	name := "Trapeze"
	if o.Hyper {
		name = "HYPERTRAPEZE"
	}
	charm := "Charm™"
	if !o.Hyper {
		charm = " " + charm
	}
	title := t.Logo.SmallCharm.Render(charm)
	title = fmt.Sprintf("%s %s", title, styles.ApplyBoldForegroundGrad(t.Logo.GradCanvas, name, t.Logo.SmallGradFromColor, t.Logo.SmallGradToColor))
	remainingWidth := width - lipgloss.Width(title) - 1 // 1 for the space after the name
	if remainingWidth > 0 {
		lines := strings.Repeat("╱", remainingWidth)
		title = fmt.Sprintf("%s %s", title, t.Logo.SmallDiagonals.Render(lines))
	}
	return title
}
