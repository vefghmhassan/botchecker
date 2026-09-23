// Package dashboard renders the server-side HTML views.
package dashboard

import (
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/store"
)

// VerdictOrder is the stacking and legend order, worst last so blocked
// endpoints sit at the top of each bar where they are easiest to read.
var VerdictOrder = []string{"HEALTHY", "PORT_CLOSED", "SERVER_DOWN", "UNKNOWN", "PARTIAL_BLOCK", "BLOCKED_IR"}

// verdictColor maps a verdict to a colour that stays legible in both themes.
var verdictColor = map[string]string{
	"HEALTHY":       "#2f9e5e",
	"PARTIAL_BLOCK": "#d98324",
	"BLOCKED_IR":    "#c8433a",
	"PORT_CLOSED":   "#8a7fb5",
	"SERVER_DOWN":   "#6b7785",
	"UNKNOWN":       "#9aa4b0",
}

// providerColor distinguishes an address we can replace from one we cannot.
var providerColor = map[string]string{
	"hetzner":  "#2f9e5e",
	"external": "#d98324",
	"unknown":  "#6b7785",
}

// ProviderColor maps an ownership label to a colour.
func ProviderColor(p string) string {
	if c, ok := providerColor[p]; ok {
		return c
	}
	return providerColor["unknown"]
}

// ProvisionColor maps a provisioning status to a colour.
func ProvisionColor(status string) string {
	switch status {
	case "verified", "swapped":
		return "#2f9e5e"
	case "dry_run", "triggered", "creating", "booting", "verifying",
		"quieted", "country_chosen":
		return "#2f6fd0"
	case "external_provider", "skipped":
		return "#d98324"
	case "verify_failed", "failed":
		return "#c8433a"
	default:
		return "#6b7785"
	}
}

func VerdictColor(v string) string {
	if c, ok := verdictColor[v]; ok {
		return c
	}
	return verdictColor["UNKNOWN"]
}

// StackedBarSVG draws the daily verdict mix as one stacked bar per day.
func StackedBarSVG(days []store.DayBucket) template.HTML {
	const (
		w, h                     = 900.0, 260.0
		left, right, top, bottom = 38.0, 10.0, 12.0, 34.0
		minBarGap                = 1.0
	)
	plotW, plotH := w-left-right, h-top-bottom

	if len(days) == 0 {
		return emptyChart(w, h, "no history yet")
	}

	maxTotal := 0
	for _, d := range days {
		if d.Total > maxTotal {
			maxTotal = d.Total
		}
	}
	if maxTotal == 0 {
		return emptyChart(w, h, "no endpoints recorded in this window")
	}

	slot := plotW / float64(len(days))
	barW := slot - minBarGap
	if barW < 1 {
		barW = slot
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="daily verdict mix">`, w, h)

	// Horizontal guides and the y scale.
	for i := 0; i <= 4; i++ {
		frac := float64(i) / 4
		y := top + plotH*(1-frac)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, left, y, w-right, y)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%d</text>`,
			left-6, y+4, int(frac*float64(maxTotal)+0.5))
	}

	for i, d := range days {
		x := left + float64(i)*slot
		yCursor := top + plotH

		for _, v := range VerdictOrder {
			n := d.Counts[v]
			if n == 0 {
				continue
			}
			segH := plotH * float64(n) / float64(maxTotal)
			yCursor -= segH
			fmt.Fprintf(&b,
				`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"><title>%s — %s: %d</title></rect>`,
				x, yCursor, barW, segH, VerdictColor(v),
				d.Date.Format("2006-01-02"), html.EscapeString(v), n)
		}

		// Date labels only every fifth day, plus the last one, to avoid overlap.
		if i%5 == 0 || i == len(days)-1 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%s</text>`,
				x+barW/2, h-12, d.Date.Format("01-02"))
		}
	}

	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="axisline"/>`,
		left, top+plotH, w-right, top+plotH)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// LifetimeBarsSVG draws one horizontal bar per healthy run, longest first.
func LifetimeBarsSVG(spans []store.LifetimeSpan) template.HTML {
	const (
		w      = 900.0
		rowH   = 26.0
		labelW = 170.0
		right  = 60.0
		top    = 10.0
	)
	if len(spans) == 0 {
		return emptyChart(w, 120, "no healthy runs recorded yet")
	}

	maxDays := 0.0
	for _, s := range spans {
		if s.Duration > maxDays {
			maxDays = s.Duration
		}
	}
	// Scale to at least one day so a run that is only minutes old draws a
	// sliver rather than filling the row and reading as the longest.
	if maxDays < 1 {
		maxDays = 1
	}

	h := top*2 + rowH*float64(len(spans))
	plotW := w - labelW - right

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="endpoint lifetimes">`, w, h)

	for i, s := range spans {
		y := top + float64(i)*rowH
		barH := rowH - 8
		barW := plotW * s.Duration / maxDays
		if barW < 2 {
			barW = 2
		}

		// An ongoing run is drawn hollow so it reads as "not finished yet".
		fill, extra := VerdictColor("BLOCKED_IR"), ""
		if !s.Ended {
			fill = VerdictColor("HEALTHY")
			extra = ` opacity="0.55"`
		}

		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="rowlabel" text-anchor="end">%s:%d</text>`,
			labelW-8, y+barH-2, html.EscapeString(s.Address), s.Port)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="%s"%s><title>%s</title></rect>`,
			labelW, y, barW, barH, fill, extra, html.EscapeString(spanTitle(s)))
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="rowvalue">%.1f d</text>`,
			labelW+barW+6, y+barH-2, s.Duration)
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func spanTitle(s store.LifetimeSpan) string {
	if s.Ended && s.End != nil {
		return fmt.Sprintf("%s:%d healthy %s → blocked %s (%.1f days)",
			s.Address, s.Port, s.Start.Format(time.DateOnly), s.End.Format(time.DateOnly), s.Duration)
	}
	return fmt.Sprintf("%s:%d healthy since %s, still up (%.1f days)",
		s.Address, s.Port, s.Start.Format(time.DateOnly), s.Duration)
}

// PercentBarsSVG draws the share of failed probes per Iranian operator.
func PercentBarsSVG(stats []store.ASNStat) template.HTML {
	const (
		w      = 900.0
		rowH   = 30.0
		labelW = 210.0
		right  = 70.0
		top    = 10.0
	)
	if len(stats) == 0 {
		return emptyChart(w, 120, "no probe readings in this window")
	}

	h := top*2 + rowH*float64(len(stats))
	plotW := w - labelW - right

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="failure rate per operator">`, w, h)

	for i, s := range stats {
		y := top + float64(i)*rowH
		barH := rowH - 10

		// Full-width track so each row is read as a share of 100%.
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" class="track"/>`,
			labelW, y, plotW, barH)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="%s"><title>%s: %d of %d probes timed out</title></rect>`,
			labelW, y, plotW*s.FailPct/100, barH, VerdictColor("BLOCKED_IR"),
			html.EscapeString(s.ASN), s.Failed, s.Total)

		label := s.ASN
		if s.Nodes != "" {
			label += " (" + s.Nodes + ")"
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="rowlabel" text-anchor="end">%s</text>`,
			labelW-8, y+barH-3, html.EscapeString(label))
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="rowvalue">%.0f%%</text>`,
			labelW+plotW+6, y+barH-3, s.FailPct)
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func emptyChart(w, h float64, msg string) template.HTML {
	return template.HTML(fmt.Sprintf(
		`<svg viewBox="0 0 %.0f %.0f" class="chart"><text x="%.0f" y="%.0f" class="empty" text-anchor="middle">%s</text></svg>`,
		w, h, w/2, h/2, html.EscapeString(msg)))
}
