package styles

import "github.com/charmbracelet/x/exp/charmtone"

// ThemeForProvider returns the Styles associated with the given provider
// ID. Unknown or empty provider IDs yield the default Charmtone Pantera
// theme.
func ThemeForProvider(providerID string) Styles {
	switch providerID {
	case "hyper":
		return HypertrapezeObsidiana()
	default:
		return CharmtonePantera()
	}
}

// CharmtonePantera returns the Charmtone dark theme. It's the default style
// for the UI.
func CharmtonePantera() Styles {
	return quickStyle(quickStyleOpts{
		primary:   charmtone.Charple,
		secondary: charmtone.Dolly,
		accent:    charmtone.Bok,
		keyword:   charmtone.Blush,

		fgBase:       charmtone.Sash,
		fgMoreSubtle: charmtone.Squid,
		fgSubtle:     charmtone.Smoke,
		fgMostSubtle: charmtone.Oyster,

		onPrimary: charmtone.Butter,

		bgBase:         charmtone.Pepper,
		bgLeastVisible: charmtone.BBQ,
		bgLessVisible:  charmtone.Char,
		bgMostVisible:  charmtone.Iron,

		separator: charmtone.Char,

		destructive:       charmtone.Coral,
		error:             charmtone.Sriracha,
		warningSubtle:     charmtone.Zest,
		warning:           charmtone.Mustard,
		denied:            charmtone.Tang,
		busy:              charmtone.Citron,
		info:              charmtone.Malibu,
		infoMoreSubtle:    charmtone.Sardine,
		infoMostSubtle:    charmtone.Damson,
		success:           charmtone.Julep,
		successMoreSubtle: charmtone.Bok,
		successMostSubtle: charmtone.Guac,
	})
}

// HypertrapezeObsidiana returns the Hypertrapeze dark theme.
func HypertrapezeObsidiana() Styles {
	return quickStyle(quickStyleOpts{
		primary:   charmtone.Charple,
		secondary: charmtone.Dolly,
		accent:    charmtone.Bok,

		fgBase:       charmtone.Sash,
		fgMoreSubtle: charmtone.Squid,
		fgSubtle:     charmtone.Smoke,
		fgMostSubtle: charmtone.Oyster,

		onPrimary: charmtone.Butter,

		bgBase:         charmtone.Pepper,
		bgLeastVisible: charmtone.BBQ,
		bgLessVisible:  charmtone.Char,
		bgMostVisible:  charmtone.Iron,

		separator: charmtone.Char,

		destructive:       charmtone.Coral,
		error:             charmtone.Sriracha,
		warningSubtle:     charmtone.Zest,
		warning:           charmtone.Mustard,
		denied:            charmtone.Tang,
		busy:              charmtone.Citron,
		info:              charmtone.Malibu,
		infoMoreSubtle:    charmtone.Sardine,
		infoMostSubtle:    charmtone.Damson,
		success:           charmtone.Julep,
		successMoreSubtle: charmtone.Bok,
		successMostSubtle: charmtone.Guac,
	})
}
