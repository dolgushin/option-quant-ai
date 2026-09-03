package main

// Server-side payoff chart for a spread candidate, rendered to a PNG.
// Light theme with grid, tick labels, title, legend and breakeven dots so the
// picture stays readable as a small Telegram preview. Text labels use
// golang.org/x/image basicfont (ASCII only — numbers and Latin names).

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Payoff chart palette (light theme).
var (
	chartBG       = color.RGBA{255, 255, 255, 255} // white
	chartPlotBG   = color.RGBA{249, 250, 251, 255} // gray-50
	chartGrid     = color.RGBA{229, 231, 235, 255} // gray-200
	chartAxis     = color.RGBA{107, 114, 128, 255} // gray-500
	chartInk      = color.RGBA{17, 24, 39, 255}    // gray-900 (text, curve accents)
	chartCurve    = color.RGBA{79, 70, 235, 255}   // indigo-600
	chartZero     = color.RGBA{156, 163, 175, 255} // gray-400
	chartShort    = color.RGBA{220, 38, 38, 255}   // red-600
	chartLong     = color.RGBA{37, 99, 235, 255}   // blue-600
	chartSpot     = color.RGBA{217, 119, 6, 255}   // amber-600
	chartPositive = color.RGBA{22, 163, 74, 255}   // green-600
	chartNegative = color.RGBA{220, 38, 38, 255}   // red-600
)

const (
	chartW       = 680
	chartH       = 380
	chartMarginL = 72
	chartMarginR = 16
	chartMarginT = 36
	chartMarginB = 32
	chartCurveN  = 140
	chartYTicks  = 5
	chartXTicks  = 6
)

// payoffPoint is one point of the at-expiry P&L curve: underlying S and the
// position P&L in rubles.
type payoffPoint struct {
	S   float64
	PnL float64
}

// candidatePayoff builds the at-expiry P&L curve for a spread candidate using
// its plan legs (SELL of the short option, BUY of the long option, each at its
// entry premium). scale = multiplier × qty. The plotted spot range covers both
// strikes and the entry spot.
func candidatePayoff(plan *spreadPlan) []payoffPoint {
	if plan == nil || len(plan.Legs) == 0 {
		return nil
	}
	scale := plan.Multiplier
	if scale <= 0 {
		scale = contractMultiplier(plan.Symbol)
	}
	q := float64(plan.Qty)
	if q < 1 {
		q = 1
	}
	scale *= q

	lo, hi := plan.ShortStrike, plan.LongStrike
	if lo > hi {
		lo, hi = hi, lo
	}
	if plan.Spot > 0 {
		lo = math.Min(lo, plan.Spot)
		hi = math.Max(hi, plan.Spot)
	}
	pad := math.Max((hi-lo)*0.6, hi*0.04)
	lo -= pad
	hi += pad

	pts := make([]payoffPoint, 0, chartCurveN+1)
	for i := 0; i <= chartCurveN; i++ {
		S := lo + (hi-lo)*float64(i)/chartCurveN
		var pnl float64
		for _, l := range plan.Legs {
			dir := 1.0
			if l.Side == "SELL" {
				dir = -1
			}
			var intr float64
			if l.IsCall {
				intr = math.Max(S-l.Strike, 0)
			} else {
				intr = math.Max(l.Strike-S, 0)
			}
			pnl += dir * (intr - l.Price) * scale
		}
		pts = append(pts, payoffPoint{S: S, PnL: pnl})
	}
	return pts
}

// chartGeom maps values into image pixel coordinates.
type chartGeom struct {
	plotW, plotH int
	lo, hi, span float64
	minY, maxY   float64
	yScale       float64
}

func newChartGeom(pts []payoffPoint) chartGeom {
	plotW := chartW - chartMarginL - chartMarginR
	plotH := chartH - chartMarginT - chartMarginB
	lo := pts[0].S
	hi := pts[len(pts)-1].S
	span := hi - lo
	if span <= 0 {
		span = 1
	}
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		minY = math.Min(minY, p.PnL)
		maxY = math.Max(maxY, p.PnL)
	}
	padY := (maxY - minY) * 0.08
	if padY < 1 {
		padY = 1
	}
	minY -= padY
	maxY += padY
	if minY > 0 {
		minY = 0
	}
	if maxY < 0 {
		maxY = 0
	}
	ySpan := maxY - minY
	if ySpan <= 0 {
		ySpan = 1
	}
	return chartGeom{plotW, plotH, lo, hi, span, minY, maxY, float64(plotH) / ySpan}
}

func (g chartGeom) x(S float64) int {
	t := (S - g.lo) / g.span
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return chartMarginL + int(math.Round(t*float64(g.plotW)))
}

func (g chartGeom) y(pnl float64) int {
	t := (pnl - g.minY) * g.yScale
	if t < 0 {
		t = 0
	}
	if t > float64(g.plotH) {
		t = float64(g.plotH)
	}
	return chartMarginT + g.plotH - int(math.Round(t))
}

func (g chartGeom) plotTop() int    { return chartMarginT }
func (g chartGeom) plotBottom() int { return chartH - chartMarginB }
func (g chartGeom) plotRight() int  { return chartW - chartMarginR }

// setPixel writes a 1x1 pixel (clipped to the image).
func setPixel(img *image.RGBA, x, y int, c color.Color) {
	if x >= 0 && x < chartW && y >= 0 && y < chartH {
		img.Set(x, y, c)
	}
}

// blendPixel mixes c over the existing pixel with the given alpha (0-255).
func blendPixel(img *image.RGBA, x, y int, c color.RGBA, alpha uint8) {
	if x < 0 || x >= chartW || y < 0 || y >= chartH {
		return
	}
	a := int(alpha)
	dst := img.RGBAAt(x, y)
	r := uint8((int(dst.R)*(255-a) + int(c.R)*a) / 255)
	gg := uint8((int(dst.G)*(255-a) + int(c.G)*a) / 255)
	b := uint8((int(dst.B)*(255-a) + int(c.B)*a) / 255)
	img.SetRGBA(x, y, color.RGBA{r, gg, b, 255})
}

// line draws a 1px segment between two pixels (Bresenham walk).
func line(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx := 1
	if x0 > x1 {
		sx = -1
	}
	sy := 1
	if y0 > y1 {
		sy = -1
	}
	err := dx - dy
	for {
		setPixel(img, x0, y0, c)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

// hline draws a horizontal line across the plot area.
func hline(img *image.RGBA, y int, c color.RGBA) {
	for x := chartMarginL; x < chartW-chartMarginR; x++ {
		setPixel(img, x, y, c)
	}
}

// dashVline draws a dashed vertical marker across the plot area.
func dashVline(img *image.RGBA, x int, c color.RGBA) {
	for y := chartMarginT; y < chartH-chartMarginB; y++ {
		if ((y - chartMarginT) % 7) < 4 {
			setPixel(img, x, y, c)
		}
	}
}

// disc draws a filled disc of radius r (breakeven dots).
func disc(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				setPixel(img, cx+dx, cy+dy, c)
			}
		}
	}
}

// drawText renders an ASCII string with basicfont (7x13); x is the left edge,
// y the text baseline.
func drawText(img *image.RGBA, x, y int, s string, c color.RGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{C: c},
		Face: basicfont.Face7x13,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(asciiOnly(s))
}

// asciiOnly replaces non-ASCII runes (basicfont has no glyphs for them).
func asciiOnly(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r > 127 }) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r > 127 {
			b.WriteByte('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// trimFloat formats v with dec decimals, trimming trailing zeros:
// 1500 -> "1500", 1220.50 -> "1220.5".
func trimFloat(v float64, dec int) string {
	s := strconv.FormatFloat(v, 'f', dec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// formatTickY compacts ruble axis labels: 1500 -> "1.5k".
func formatTickY(v float64) string {
	if math.Abs(v) >= 1000 {
		return trimFloat(v/1000, 1) + "k"
	}
	return trimFloat(v, 0)
}

// payoffAt linearly interpolates the curve P&L at spot S.
func payoffAt(pts []payoffPoint, S float64) float64 {
	if len(pts) == 0 {
		return 0
	}
	if S <= pts[0].S {
		return pts[0].PnL
	}
	if S >= pts[len(pts)-1].S {
		return pts[len(pts)-1].PnL
	}
	for i := 0; i < len(pts)-1; i++ {
		a, b := pts[i], pts[i+1]
		if S >= a.S && S <= b.S {
			if b.S == a.S {
				return b.PnL
			}
			t := (S - a.S) / (b.S - a.S)
			return a.PnL + t*(b.PnL-a.PnL)
		}
	}
	return pts[0].PnL
}

// chartTitle builds the ASCII title line: "Si Bull Put Spread 2026-09-24".
func chartTitle(symbol, display, expiry string) string {
	t := strings.TrimSpace(symbol + " " + display + " " + expiry)
	if t == "" {
		return "Spread payoff"
	}
	return t
}

// drawPayoffChart renders the spread payoff PNG: light plot with grid and tick
// labels, profit/loss zone fill, dashed strike markers, spot line, breakeven
// dots, title and legend.
func drawPayoffChart(title string, pts []payoffPoint, spot, shortStrike, longStrike float64) ([]byte, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	g := newChartGeom(pts)

	img := image.NewRGBA(image.Rect(0, 0, chartW, chartH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: chartBG}, image.Point{}, draw.Src)
	plot := image.Rect(chartMarginL, chartMarginT, chartW-chartMarginR, chartH-chartMarginB)
	draw.Draw(img, plot, &image.Uniform{C: chartPlotBG}, image.Point{}, draw.Src)

	zeroY := g.y(0)

	// Profit/loss zone fill: tint each column from the curve to the zero line.
	for px := chartMarginL; px < chartW-chartMarginR; px++ {
		t := float64(px-chartMarginL) / float64(g.plotW)
		Sv := g.lo + t*g.span
		pnl := payoffAt(pts, Sv)
		yC := g.y(pnl)
		fill := chartPositive
		if pnl < 0 {
			fill = chartNegative
		}
		y0, y1 := yC, zeroY
		if y0 > y1 {
			y0, y1 = y1, y0
		}
		for y := y0; y <= y1; y++ {
			blendPixel(img, px, y, fill, 30)
		}
	}

	// Horizontal grid + Y tick labels.
	for i := 0; i <= chartYTicks; i++ {
		v := g.minY + (g.maxY-g.minY)*float64(i)/chartYTicks
		y := g.y(v)
		if y == zeroY {
			continue
		}
		hline(img, y, chartGrid)
		label := formatTickY(v)
		drawText(img, chartMarginL-6-len(label)*7, y+4, label, chartAxis)
	}

	// Vertical grid + X tick labels.
	for i := 0; i <= chartXTicks; i++ {
		Sv := g.lo + g.span*float64(i)/chartXTicks
		x := g.x(Sv)
		for y := chartMarginT; y < chartH-chartMarginB; y++ {
			if ((y - chartMarginT) % 7) < 1 {
				setPixel(img, x, y, chartGrid)
			}
		}
		label := trimFloat(Sv, 0)
		drawText(img, x-len(label)*7/2, chartH-chartMarginB+17, label, chartAxis)
	}

	// Zero reference line on top of the grid.
	hline(img, zeroY, chartZero)
	drawText(img, chartMarginL-6-len("0")*7, zeroY+4, "0", chartAxis)

	// Strike markers (dashed) and spot line (solid, 2px).
	if shortStrike > 0 {
		x := g.x(shortStrike)
		dashVline(img, x, chartShort)
		dashVline(img, x+1, chartShort)
	}
	if longStrike > 0 {
		x := g.x(longStrike)
		dashVline(img, x, chartLong)
		dashVline(img, x+1, chartLong)
	}
	if spot > 0 {
		x := g.x(spot)
		for y := chartMarginT; y < chartH-chartMarginB; y++ {
			setPixel(img, x, y, chartSpot)
			setPixel(img, x+1, y, chartSpot)
		}
	}

	// Payoff curve, 3px thick.
	for pass := 0; pass < 3; pass++ {
		ox, oy := 0, 0
		if pass == 1 {
			oy = 1
		} else if pass == 2 {
			ox = 1
		}
		for i := 0; i < len(pts)-1; i++ {
			line(img,
				g.x(pts[i].S)+ox, g.y(pts[i].PnL)+oy,
				g.x(pts[i+1].S)+ox, g.y(pts[i+1].PnL)+oy,
				chartCurve)
		}
	}

	// Breakeven rings where the curve crosses zero.
	for i := 0; i < len(pts)-1; i++ {
		a, b := pts[i].PnL, pts[i+1].PnL
		if (a < 0 && b > 0) || (a > 0 && b < 0) {
			t := math.Abs(a) / (math.Abs(a) + math.Abs(b))
			Sbe := pts[i].S + t*(pts[i+1].S-pts[i].S)
			disc(img, g.x(Sbe), zeroY, 4, chartInk)
			disc(img, g.x(Sbe), zeroY, 2, chartBG)
		}
	}

	// Title.
	drawText(img, chartMarginL, 22, title, chartInk)

	// Legend (bottom-right inside the plot).
	type legRow struct {
		label string
		c     color.RGBA
		dash  bool
	}
	rows := []legRow{}
	if shortStrike > 0 {
		rows = append(rows, legRow{"SHORT " + trimFloat(shortStrike, 0), chartShort, true})
	}
	if longStrike > 0 {
		rows = append(rows, legRow{"LONG " + trimFloat(longStrike, 0), chartLong, true})
	}
	if spot > 0 {
		rows = append(rows, legRow{"SPOT " + trimFloat(spot, 0), chartSpot, false})
	}
	if len(rows) > 0 {
		maxLen := 0
		for _, r := range rows {
			maxLen = max(maxLen, len(r.label))
		}
		boxW := maxLen*7 + 30
		boxH := len(rows)*16 + 12
		bx := g.plotRight() - boxW - 8
		by := g.plotBottom() - boxH - 8
		draw.Draw(img, image.Rect(bx, by, bx+boxW, by+boxH), &image.Uniform{C: color.RGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)
		// Border.
		for x := bx; x < bx+boxW; x++ {
			setPixel(img, x, by, chartAxis)
			setPixel(img, x, by+boxH-1, chartAxis)
		}
		for y := by; y < by+boxH; y++ {
			setPixel(img, bx, y, chartAxis)
			setPixel(img, bx+boxW-1, y, chartAxis)
		}
		for i, r := range rows {
			ry := by + 8 + i*16
			for sx := 0; sx < 14; sx++ {
				if !r.dash || (sx%4) < 2 {
					setPixel(img, bx+8+sx, ry+4, r.c)
					setPixel(img, bx+8+sx, ry+5, r.c)
				}
			}
			drawText(img, bx+26, ry+11, r.label, chartInk)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
