package brandii

import (
	"context"
	"math"
)

// AdaptiveOptions configures adaptive color generation.
type AdaptiveOptions struct {
	// LightBase is the background hex used in light mode (e.g. "#ffffff").
	LightBase string
	// DarkBase is the background hex used in dark mode (e.g. "#0a0a0a").
	DarkBase string
	// MinContrast is the minimum WCAG contrast ratio each variant must meet
	// against its surface. Defaults to 4.5 (WCAG AA for normal text) when 0.
	MinContrast float64
}

// AdaptiveColor pairs an extracted brand color with variants tuned to meet
// contrast requirements against a light surface and a dark surface.
type AdaptiveColor struct {
	// Original is the color as discovered on the page.
	Original string `json:"original"`
	// Usage carries through the role from the extraction (primary, etc.).
	Usage ColorRole `json:"usage,omitempty"`

	// Light is the variant suitable for use on the light surface.
	Light         string  `json:"light"`
	LightContrast float64 `json:"light_contrast"`
	LightAdjusted bool    `json:"light_adjusted"`

	// Dark is the variant suitable for use on the dark surface.
	Dark         string  `json:"dark"`
	DarkContrast float64 `json:"dark_contrast"`
	DarkAdjusted bool    `json:"dark_adjusted"`
}

// AdaptiveResult bundles a normal extraction with adaptive color variants.
type AdaptiveResult struct {
	*Result
	LightBase      string          `json:"light_base"`
	DarkBase       string          `json:"dark_base"`
	MinContrast    float64         `json:"min_contrast"`
	AdaptiveColors []AdaptiveColor `json:"adaptive_colors"`
}

// ExtractAdaptive runs a normal extraction against pageURL and then, for each
// discovered color, derives light- and dark-mode variants that hit
// opts.MinContrast on opts.LightBase and opts.DarkBase respectively.
//
// Convenience wrapper: ExtractAdaptive(ctx, url, opts) ≡ New().ExtractAdaptive.
func ExtractAdaptive(ctx context.Context, pageURL string, opts AdaptiveOptions) (*AdaptiveResult, error) {
	return defaultClient.ExtractAdaptive(ctx, pageURL, opts)
}

// ExtractAdaptive is the Client method behind the package-level helper.
func (c *Client) ExtractAdaptive(ctx context.Context, pageURL string, opts AdaptiveOptions) (*AdaptiveResult, error) {
	res, err := c.Extract(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	return BuildAdaptive(res, opts), nil
}

// BuildAdaptive turns an existing Result into an AdaptiveResult given a pair
// of surface colors. Exposed separately so callers who already have a Result
// (e.g. from cache) can compute variants without re-fetching.
func BuildAdaptive(res *Result, opts AdaptiveOptions) *AdaptiveResult {
	if opts.LightBase == "" {
		opts.LightBase = "#ffffff"
	}
	if opts.DarkBase == "" {
		opts.DarkBase = "#0a0a0a"
	}
	if opts.MinContrast <= 0 {
		opts.MinContrast = 4.5
	}

	out := &AdaptiveResult{
		Result:      res,
		LightBase:   normalizeHex(opts.LightBase),
		DarkBase:    normalizeHex(opts.DarkBase),
		MinContrast: opts.MinContrast,
	}
	for _, c := range res.Colors {
		light, lightCR, lightAdj := AdaptColor(c.Hex, opts.LightBase, opts.MinContrast)
		dark, darkCR, darkAdj := AdaptColor(c.Hex, opts.DarkBase, opts.MinContrast)
		out.AdaptiveColors = append(out.AdaptiveColors, AdaptiveColor{
			Original:      normalizeHex(c.Hex),
			Usage:         c.Usage,
			Light:         light,
			LightContrast: lightCR,
			LightAdjusted: lightAdj,
			Dark:          dark,
			DarkContrast:  darkCR,
			DarkAdjusted:  darkAdj,
		})
	}
	return out
}

// AdaptColor returns a variant of colorHex that meets minContrast against
// baseHex. If the input already passes, it is returned unchanged with
// adjusted=false. Otherwise the function walks lightness in OKLCH (preserving
// hue) and, if lightness alone is insufficient, reduces chroma until contrast
// is satisfied.
//
// Returned values: (hex, achieved contrast ratio, whether adjustment occurred).
func AdaptColor(colorHex, baseHex string, minContrast float64) (string, float64, bool) {
	if minContrast <= 0 {
		minContrast = 4.5
	}
	original := normalizeHex(colorHex)
	if cr := contrastRatio(original, baseHex); cr >= minContrast {
		return original, cr, false
	}

	L, C, H := hexToOKLCH(original)
	baseLum := relativeLuminance(baseHex)
	// On a light background we need a darker color (lower L). On a dark
	// background we need a lighter color (higher L).
	goLighter := baseLum < 0.5

	// Walk L toward the appropriate extreme in small steps. OKLab L is
	// roughly perceptually linear, so this stops at the closest L to the
	// original that satisfies contrast.
	const step = 0.02
	L2 := L
	for i := 0; i < 60; i++ {
		if goLighter {
			L2 += step
			if L2 > 1 {
				L2 = 1
			}
		} else {
			L2 -= step
			if L2 < 0 {
				L2 = 0
			}
		}
		candidate := oklchToHex(L2, C, H)
		if cr := contrastRatio(candidate, baseHex); cr >= minContrast {
			return candidate, cr, true
		}
		if L2 == 0 || L2 == 1 {
			break
		}
	}

	// Lightness alone didn't get us there — drop chroma and try again at the
	// extreme L. This is the fallback for highly saturated colors like pure
	// yellow that can never hit 4.5:1 against white at full chroma.
	for scale := 0.75; scale >= 0; scale -= 0.25 {
		var Lext float64
		if goLighter {
			Lext = 1
		}
		candidate := oklchToHex(Lext, C*scale, H)
		if cr := contrastRatio(candidate, baseHex); cr >= minContrast {
			return candidate, cr, true
		}
	}

	// Last resort: pure black or pure white.
	if goLighter {
		return "#ffffff", contrastRatio("#ffffff", baseHex), true
	}
	return "#000000", contrastRatio("#000000", baseHex), true
}

// ── WCAG contrast ───────────────────────────────────────────────────────────

// relativeLuminance computes the WCAG 2.x relative luminance of a hex color.
func relativeLuminance(hex string) float64 {
	r, g, b := hexToRGB(hex)
	return 0.2126*srgbToLinear(float64(r)/255) +
		0.7152*srgbToLinear(float64(g)/255) +
		0.0722*srgbToLinear(float64(b)/255)
}

func srgbToLinear(c float64) float64 {
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func linearToSRGB(c float64) float64 {
	if c <= 0.0031308 {
		return 12.92 * c
	}
	return 1.055*math.Pow(c, 1.0/2.4) - 0.055
}

// contrastRatio returns the WCAG contrast ratio between two hex colors,
// always in the range [1, 21].
func contrastRatio(a, b string) float64 {
	la := relativeLuminance(a)
	lb := relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// ── OKLab / OKLCH conversion (Björn Ottosson) ───────────────────────────────

// hexToOKLCH converts a hex color to (L, C, H) in OKLCH space. H is in radians.
func hexToOKLCH(hex string) (L, C, H float64) {
	r8, g8, b8 := hexToRGB(hex)
	r := srgbToLinear(float64(r8) / 255)
	g := srgbToLinear(float64(g8) / 255)
	b := srgbToLinear(float64(b8) / 255)

	l := 0.4122214708*r + 0.5363325363*g + 0.0514459929*b
	m := 0.2119034982*r + 0.6806995451*g + 0.1073969566*b
	s := 0.0883024619*r + 0.2817188376*g + 0.6299787005*b

	l_ := math.Cbrt(l)
	m_ := math.Cbrt(m)
	s_ := math.Cbrt(s)

	L = 0.2104542553*l_ + 0.7936177850*m_ - 0.0040720468*s_
	a := 1.9779984951*l_ - 2.4285922050*m_ + 0.4505937099*s_
	bb := 0.0259040371*l_ + 0.7827717662*m_ - 0.8086757660*s_

	C = math.Sqrt(a*a + bb*bb)
	H = math.Atan2(bb, a)
	return
}

// oklchToHex converts (L, C, H) in OKLCH back to a clamped #rrggbb hex string.
func oklchToHex(L, C, H float64) string {
	a := C * math.Cos(H)
	b := C * math.Sin(H)

	l_ := L + 0.3963377774*a + 0.2158037573*b
	m_ := L - 0.1055613458*a - 0.0638541728*b
	s_ := L - 0.0894841775*a - 1.2914855480*b

	l := l_ * l_ * l_
	m := m_ * m_ * m_
	s := s_ * s_ * s_

	r := 4.0767416621*l - 3.3077115913*m + 0.2309699292*s
	g := -1.2684380046*l + 2.6097574011*m - 0.3413193965*s
	bl := -0.0041960863*l - 0.7034186147*m + 1.7076147010*s

	return rgbToHex(
		floatToByte(linearToSRGB(r)),
		floatToByte(linearToSRGB(g)),
		floatToByte(linearToSRGB(bl)),
	)
}

func floatToByte(c float64) uint8 {
	if c < 0 {
		c = 0
	}
	if c > 1 {
		c = 1
	}
	return uint8(math.Round(c * 255))
}
