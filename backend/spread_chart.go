package main

// Server-side payoff chart for a spread candidate, rendered to a PNG with the
// standard library only (no external image/font deps). The Telegram alert for
// an auto-scan candidate carries this chart (curve + zero line + spot/strike
// markers, no text labels) plus a caption with every parameter of the found
// construction (see notifyCandidateSpread).

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// Payoff chart palette (matches the admin UI's dark theme).
var (
	chartBG       = color.RGBA{15, 23, 42, 255}    // slate-900
	chartGrid     = color.RGBA{30, 41, 59, 255}    // slate-800
	chartLine     = color.RGBA{129, 140, 248, 255} // indigo-400
	chartZero     = color.RGBA{148, 163, 184, 255} // slate-400
	chartSpot     = color.RGBA{245, 158, 11, 255}  // amber-500
	chartStrike   = color.RGBA{34, 211, 238, 255}  // cyan-400
	chartPositive = color.RGBA{52, 211, 153, 255}  // emerald-400
	chartNegative = color.RGBA{248, 113, 113, 255} // rose-400
)

const (
	chartW       = 620
	chartH       = 300
	chartMarginL = 12
	chartMarginR = 12
	chartMarginT = 12
	chartMarginB = 12
	chartCurve   = 120
)

// payoffPoint is one point of the at-expiry P&L curve: underlying S and the
// position P&L in rubles.
type payoffPoint struct {
	S   float64
	PnL float64
}

// candidatePayoff builds the at-expiry P&L curve for a spread candidate using
// its plan legs (SELL of the short option, BUY of the long option, each at its
// entry premium). scale = multiplier × qty.
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
	pad := math.Max((hi-lo)*0.7, hi*0.05)
	lo -= pad
	hi += pad

	pts := make([]payoffPoint, 0, chartCurve+1)
	for i := 0; i <= chartCurve; i++ {
		S := lo + (hi-lo)*float64(i)/chartCurve
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
	padY := (maxY - minY) * 0.06
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
	if t > 1 {
		t = 1
	}
	return chartMarginT + g.plotH - int(math.Round(t*float64(g.plotH)))
}

// setPixel writes a 1x1 pixel (used for curve rasterisation).
func setPixel(img *image.RGBA, x, y int, c color.Color) {
	if x >= 0 && x < chartW && y >= 0 && y < chartH {
		img.Set(x, y, c)
	}
}

// line draws a vertical or interpolated line between two pixels using a
// Bresenham-style walk.
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

// vline draws a vertical grid/zero reference line across the plot area.
func vline(img *image.RGBA, x int, c color.RGBA) {
	for y := chartMarginT; y < chartH-chartMarginB; y++ {
		setPixel(img, x, y, c)
	}
}

// drawPayoffChart renders the spread payoff PNG (curve + zero line + spot and
// strike marker columns) into a byte slice.
func drawPayoffChart(pts []payoffPoint, spot, shortStrike, longStrike float64) ([]byte, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	g := newChartGeom(pts)

	img := image.NewRGBA(image.Rect(0, 0, chartW, chartH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: chartBG}, image.Point{}, draw.Src)

	// Plot-area background + border.
	plot := image.Rect(chartMarginL, chartMarginT, chartW-chartMarginR, chartH-chartMarginB)
	draw.Draw(img, plot, &image.Uniform{C: chartGrid}, image.Point{}, draw.Src)

	// Zero reference line.
	vline(img, g.x(0), chartZero)

	// Strike marker columns.
	if shortStrike > 0 {
		vline(img, g.x(shortStrike), chartStrike)
	}
	if longStrike > 0 {
		vline(img, g.x(longStrike), chartStrike)
	}

	// Spot marker column (translucent amber).
	if spot > 0 {
		sx := g.x(spot)
		col := color.RGBA{chartSpot.R, chartSpot.G, chartSpot.B, 110}
		blendColumn(img, sx, col)
	}

	// Payoff curve with a 2-px thickness pass for legibility.
	for i := 0; i < len(pts)-1; i++ {
		pr := lineColor(pts[i].PnL)
		x0, y0 := g.x(pts[i].S), g.y(pts[i].PnL)
		x1, y1 := g.x(pts[i+1].S), g.y(pts[i+1].PnL)
		line(img, x0, y0, x1, y1, pr)
	}
	for i := 0; i < len(pts)-1; i++ {
		pr := lineColor(pts[i].PnL)
		line(img, g.x(pts[i].S), g.y(pts[i].PnL)+1, g.x(pts[i+1].S), g.y(pts[i+1].PnL)+1, pr)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// lineColor tints the payoff segment by its P&L sign.
func lineColor(pnl float64) color.RGBA {
	if pnl >= 0 {
		return chartPositive
	}
	return chartNegative
}

// blendColumn tints a whole pixel column toward col (used for the spot marker).
func blendColumn(img *image.RGBA, x int, col color.RGBA) {
	for y := chartMarginT; y < chartH-chartMarginB; y++ {
		if x >= 0 && x < chartW {
			c := img.RGBAAt(x, y)
			r := uint8((int(c.R)*(255-int(col.A)) + int(col.R)*int(col.A)) / 255)
			gc := uint8((int(c.G)*(255-int(col.A)) + int(col.G)*int(col.A)) / 255)
			b := uint8((int(c.B)*(255-int(col.A)) + int(col.B)*int(col.A)) / 255)
			img.SetRGBA(x, y, color.RGBA{r, gc, b, 255})
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
