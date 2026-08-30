package report

import (
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
)

// chartPoint is one X/Y coordinate to plot, in data units (not pixels).
type chartPoint struct {
	X float64
	Y float64
}

const (
	lineChartWidth  = 480
	lineChartHeight = 220
	lineChartPadL   = 50
	lineChartPadR   = 46
	lineChartPadTop = 30
	lineChartPadBot = 30
)

// renderLineChart renders points (assumed already sorted by X) as an SVG
// line chart, scaling data units to pixels. Each chart is a single series,
// so its title stands in for a legend (no swatch box needed). It degrades
// gracefully for 0 or 1 points, and for a flat series (min == max on either
// axis), rather than dividing by zero.
func renderLineChart(title string, points []chartPoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg">`, lineChartWidth, lineChartHeight)
	fmt.Fprintf(&b, `<text x="%d" y="16" class="chart-title">%s</text>`, lineChartPadL, html.EscapeString(title))

	if len(points) == 0 {
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="chart-empty">no data</text>`, lineChartWidth/2-25, lineChartHeight/2)
		b.WriteString("</svg>")
		return b.String()
	}

	minX, maxX := points[0].X, points[0].X
	minY, maxY := points[0].Y, points[0].Y
	for _, p := range points {
		minX, maxX = math.Min(minX, p.X), math.Max(maxX, p.X)
		minY, maxY = math.Min(minY, p.Y), math.Max(maxY, p.Y)
	}

	xRange := maxX - minX
	if xRange == 0 {
		xRange = 1
	}
	yRange := maxY - minY
	if yRange == 0 {
		yRange = 1
	}

	plotW := float64(lineChartWidth - lineChartPadL - lineChartPadR)
	plotH := float64(lineChartHeight - lineChartPadTop - lineChartPadBot)

	toPx := func(p chartPoint) (float64, float64) {
		x := float64(lineChartPadL) + (p.X-minX)/xRange*plotW
		y := float64(lineChartPadTop) + plotH - (p.Y-minY)/yRange*plotH
		return x, y
	}

	// Baseline axis: a single hairline, recessive, never dashed.
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="chart-axis" />`,
		lineChartPadL, lineChartHeight-lineChartPadBot, lineChartWidth-lineChartPadR, lineChartHeight-lineChartPadBot)

	var lastX, lastY float64
	if len(points) == 1 {
		lastX, lastY = toPx(points[0])
	} else {
		var poly strings.Builder
		for i, p := range points {
			x, y := toPx(p)
			if i > 0 {
				poly.WriteByte(' ')
			}
			fmt.Fprintf(&poly, "%.1f,%.1f", x, y)
			lastX, lastY = x, y
		}
		fmt.Fprintf(&b, `<polyline points="%s" class="chart-line" />`, poly.String())
	}

	// End marker: a surface-color ring keeps the dot legible where it sits
	// on the line, then the value is labeled at the end per the line spec.
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="6" class="chart-end-ring" />`, lastX, lastY)
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" class="chart-end-dot" />`, lastX, lastY)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="chart-end-label" text-anchor="end">%s</text>`,
		lastX-8, lastY-8, html.EscapeString(formatNum(points[len(points)-1].Y)))

	fmt.Fprintf(&b, `<text x="%d" y="%d" class="chart-axis-label">min %s</text>`,
		lineChartPadL, lineChartHeight-8, html.EscapeString(formatNum(minY)))
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="chart-axis-label" text-anchor="end">max %s, over %ss</text>`,
		lineChartWidth-lineChartPadR, lineChartHeight-8, html.EscapeString(formatNum(maxY)), html.EscapeString(formatNum(maxX-minX)))

	b.WriteString("</svg>")
	return b.String()
}

const (
	barChartWidth     = 480
	barChartLabelW    = 90
	barChartValueW    = 80
	barChartBarH      = 20
	barChartBarGap    = 10
	barChartTopPad    = 30
	barChartBottomPad = 16
	barChartRadius    = 4
)

// renderBarChart renders one horizontal bar per key of values (sorted by
// key for stable output), scaled against the largest absolute value. All
// bars share one series color: they are different statistics of the same
// metric, not separate identities, so no legend or per-bar hue is needed.
func renderBarChart(title string, values map[string]float64) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	height := barChartTopPad + barChartBottomPad
	if len(keys) > 0 {
		height += len(keys) * (barChartBarH + barChartBarGap)
	} else {
		height += barChartBarH
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg">`, barChartWidth, height)
	fmt.Fprintf(&b, `<text x="4" y="16" class="chart-title">%s</text>`, html.EscapeString(title))

	if len(keys) == 0 {
		fmt.Fprintf(&b, `<text x="4" y="%d" class="chart-empty">no data</text>`, barChartTopPad+16)
		b.WriteString("</svg>")
		return b.String()
	}

	maxAbs := 0.0
	for _, v := range values {
		if abs := math.Abs(v); abs > maxAbs {
			maxAbs = abs
		}
	}
	if maxAbs == 0 {
		maxAbs = 1
	}

	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" class="chart-axis" />`,
		barChartLabelW, barChartTopPad-6, barChartLabelW, height-barChartBottomPad)

	barAreaW := float64(barChartWidth - barChartLabelW - barChartValueW)
	for i, k := range keys {
		y := barChartTopPad + i*(barChartBarH+barChartBarGap)
		v := values[k]
		w := math.Abs(v) / maxAbs * barAreaW
		fmt.Fprintf(&b, `<text x="4" y="%d" class="chart-bar-label">%s</text>`, y+barChartBarH-6, html.EscapeString(k))
		b.WriteString(barPath(barChartLabelW, y, w, barChartBarH, barChartRadius))
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="chart-bar-value">%s</text>`,
			barChartLabelW+int(w)+6, y+barChartBarH-6, html.EscapeString(formatNum(v)))
	}

	b.WriteString("</svg>")
	return b.String()
}

// barPath draws a horizontal bar of width w and height h at (x, y), with
// the baseline (left) edge square and the data-end (right) edge rounded by
// r, per the bar/column mark spec. It degrades to a plain rectangle when w
// is too small to fit the rounding.
func barPath(x, y int, w float64, h, r int) string {
	rr := float64(r)
	if w < rr || float64(h)/2 < rr {
		rr = 0
	}
	fx := float64(x)
	fy := float64(y)
	fh := float64(h)

	if rr == 0 {
		return fmt.Sprintf(`<rect x="%d" y="%d" width="%.1f" height="%d" class="chart-bar" />`, x, y, w, h)
	}

	d := fmt.Sprintf(
		"M%.1f,%.1f H%.1f Q%.1f,%.1f %.1f,%.1f V%.1f Q%.1f,%.1f %.1f,%.1f H%.1f Z",
		fx, fy, fx+w-rr,
		fx+w, fy, fx+w, fy+rr,
		fy+fh-rr,
		fx+w, fy+fh, fx+w-rr, fy+fh,
		fx,
	)
	return fmt.Sprintf(`<path d="%s" class="chart-bar" />`, d)
}

// formatNum formats a float64 compactly, matching the %g formatting used
// elsewhere in report output.
func formatNum(v float64) string {
	return fmt.Sprintf("%g", v)
}
