package brandii

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
)

// logoColorPriority is the order in which logos are visited when extracting
// dominant colors. Apple touch icons typically use the brand color directly so
// they're sampled first.
var logoColorPriority = map[LogoType]int{
	LogoTypeAppleTouchIcon: 0,
	LogoTypeFavicon:        1,
	LogoTypeLogo:           2,
	LogoTypeIcon:           3,
	LogoTypeImg:            4,
	LogoTypeSVG:            5,
}

// extractColors derives up to 3 brand colors from <meta theme-color>, the web
// app manifest, and dominant colors sampled from the discovered logos.
func (c *Client) extractColors(
	ctx context.Context,
	doc *goquery.Document,
	baseURL string,
	logos []LogoAsset,
) []ColorAsset {
	// Signal 1: theme-color and msapplication-TileColor meta tags.
	var themeColors []string
	for _, sel := range []string{`meta[name="theme-color"]`, `meta[name="msapplication-TileColor"]`} {
		doc.Find(sel).Each(func(_ int, s *goquery.Selection) {
			content, _ := s.Attr("content")
			if hex := normalizeToHex(strings.TrimSpace(content)); hex != "" {
				themeColors = append(themeColors, hex)
			}
		})
	}

	// manifest.json theme_color / background_color.
	if href, ok := doc.Find(`link[rel="manifest"]`).Attr("href"); ok {
		if manifestURL := resolveURL(href, baseURL); manifestURL != "" {
			if extra := c.fetchManifestColors(ctx, manifestURL); len(extra) > 0 {
				themeColors = append(themeColors, extra...)
			}
		}
	}

	// Signal 2: dominant colors sampled from logo images.
	sortedLogos := make([]LogoAsset, 0, len(logos))
	for _, l := range logos {
		if !strings.HasPrefix(l.URL, "data:") {
			sortedLogos = append(sortedLogos, l)
		}
	}
	sort.SliceStable(sortedLogos, func(i, j int) bool {
		ai, ok := logoColorPriority[sortedLogos[i].Type]
		if !ok {
			ai = 99
		}
		bi, ok := logoColorPriority[sortedLogos[j].Type]
		if !ok {
			bi = 99
		}
		return ai < bi
	})

	logoColors := make(map[string]float64)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, logo := range sortedLogos {
		wg.Add(1)
		go func(logo LogoAsset) {
			defer wg.Done()
			c.sampleLogoColors(ctx, logo.URL, logoColors, &mu)
		}(logo)
	}
	wg.Wait()

	// Split into chromatic vs achromatic and sort by weight.
	type weighted struct {
		hex    string
		weight float64
	}
	var chromatic, achromatic []weighted
	for hex, w := range logoColors {
		r, g, b := hexToRGB(hex)
		max := maxOf(r, g, b)
		min := minOf(r, g, b)
		var saturation float64
		if max > 0 {
			saturation = float64(max-min) / float64(max)
		}
		if saturation > 0.1 {
			chromatic = append(chromatic, weighted{hex, w})
		} else {
			achromatic = append(achromatic, weighted{hex, w})
		}
	}
	sort.SliceStable(chromatic, func(i, j int) bool { return chromatic[i].weight > chromatic[j].weight })
	sort.SliceStable(achromatic, func(i, j int) bool { return achromatic[i].weight > achromatic[j].weight })

	sorted := make([]string, 0, len(chromatic)+len(achromatic))
	for _, w := range chromatic {
		sorted = append(sorted, w.hex)
	}
	for _, w := range achromatic {
		sorted = append(sorted, w.hex)
	}
	deduped := deduplicateColors(sorted)

	// Combine: theme-color first, then logo-derived colors, capped at 3.
	var ranked []string
	for _, hex := range themeColors {
		norm := normalizeHex(hex)
		if !containsSimilar(ranked, norm) {
			ranked = append(ranked, norm)
		}
	}
	for _, hex := range deduped {
		if len(ranked) >= 3 {
			break
		}
		if !containsSimilar(ranked, hex) {
			ranked = append(ranked, hex)
		}
	}

	roles := []ColorRole{ColorRolePrimary, ColorRoleSecondary, ColorRoleAccent}
	out := make([]ColorAsset, 0, len(ranked))
	for i, hex := range ranked {
		if i >= 3 {
			break
		}
		out = append(out, ColorAsset{Hex: hex, Usage: roles[i]})
	}
	return out
}

// sampleLogoColors fetches a logo, downsamples it to 16×16 with nearest
// neighbor, and accumulates per-pixel color weights into colors. Pixels with
// higher saturation get up to 4× the weight, since brand colors tend to be
// more saturated than incidental backgrounds.
func (c *Client) sampleLogoColors(
	ctx context.Context,
	logoURL string,
	colors map[string]float64,
	mu *sync.Mutex,
) {
	body, err := c.fetchBytes(ctx, logoURL, 5<<20)
	if err != nil {
		return
	}
	img, _, err := image.Decode(strings.NewReader(string(body)))
	if err != nil {
		return
	}
	resized := resizeNearest(img, 16, 16)

	mu.Lock()
	defer mu.Unlock()
	bounds := resized.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r32, g32, b32, _ := resized.At(x, y).RGBA()
			r, g, b := uint8(r32>>8), uint8(g32>>8), uint8(b32>>8)
			hex := rgbToHex(r, g, b)
			max := maxOf(r, g, b)
			min := minOf(r, g, b)
			var saturation float64
			if max > 0 {
				saturation = float64(max-min) / float64(max)
			}
			weight := 1 + saturation*3 // 1×–4× based on saturation
			colors[hex] += weight
		}
	}
}

// resizeNearest downscales src to (w, h) using nearest-neighbor sampling.
// Sufficient for color histogram extraction; we deliberately avoid pulling in
// a real image scaling library.
func resizeNearest(src image.Image, w, h int) *image.NRGBA {
	bounds := src.Bounds()
	sw, sh := bounds.Dx(), bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	if sw == 0 || sh == 0 {
		return dst
	}
	for y := 0; y < h; y++ {
		sy := y*sh/h + bounds.Min.Y
		for x := 0; x < w; x++ {
			sx := x*sw/w + bounds.Min.X
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// fetchBytes is a small GET helper used by image probing and color sampling.
// max caps the read to avoid pulling huge images into memory.
func (c *Client) fetchBytes(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

func (c *Client) fetchManifestColors(ctx context.Context, manifestURL string) []string {
	body, err := c.fetchBytes(ctx, manifestURL, 256<<10)
	if err != nil {
		return nil
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil
	}
	var out []string
	for _, key := range []string{"theme_color", "background_color"} {
		if v, ok := manifest[key].(string); ok {
			if hex := normalizeToHex(v); hex != "" {
				out = append(out, hex)
			}
		}
	}
	return out
}

// ── Color helpers ───────────────────────────────────────────────────────────

var rgbFuncRE = regexp.MustCompile(`(?i)rgb\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)`)
var hex36RE = regexp.MustCompile(`^#([0-9a-f]{3}){1,2}$`)

func normalizeToHex(color string) string {
	trimmed := strings.ToLower(strings.TrimSpace(color))
	if trimmed == "" {
		return ""
	}
	if hex36RE.MatchString(trimmed) {
		return normalizeHex(trimmed)
	}
	if m := rgbFuncRE.FindStringSubmatch(trimmed); m != nil {
		r, _ := strconv.Atoi(m[1])
		g, _ := strconv.Atoi(m[2])
		b, _ := strconv.Atoi(m[3])
		return rgbToHex(uint8(r), uint8(g), uint8(b))
	}
	return ""
}

func normalizeHex(hex string) string {
	h := strings.ToLower(hex)
	if len(h) == 4 { // #abc → #aabbcc
		return fmt.Sprintf("#%c%c%c%c%c%c", h[1], h[1], h[2], h[2], h[3], h[3])
	}
	return h
}

func rgbToHex(r, g, b uint8) string {
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

func hexToRGB(hex string) (uint8, uint8, uint8) {
	h := normalizeHex(hex)
	if len(h) != 7 {
		return 0, 0, 0
	}
	n, err := strconv.ParseUint(h[1:], 16, 32)
	if err != nil {
		return 0, 0, 0
	}
	return uint8((n >> 16) & 0xff), uint8((n >> 8) & 0xff), uint8(n & 0xff)
}

func deduplicateColors(colors []string) []string {
	var out []string
	for _, hex := range colors {
		if !containsSimilar(out, hex) {
			out = append(out, hex)
		}
	}
	return out
}

// areColorsSimilar reports whether two hex colors are within ~50 RGB-distance
// units of each other (matches the upstream JS threshold).
func areColorsSimilar(a, b string) bool {
	const threshold = 50.0
	r1, g1, b1 := hexToRGB(a)
	r2, g2, b2 := hexToRGB(b)
	dr := float64(r1) - float64(r2)
	dg := float64(g1) - float64(g2)
	db := float64(b1) - float64(b2)
	return math.Sqrt(dr*dr+dg*dg+db*db) < threshold
}

func containsSimilar(haystack []string, hex string) bool {
	for _, h := range haystack {
		if areColorsSimilar(h, hex) {
			return true
		}
	}
	return false
}

func maxOf(a, b, c uint8) uint8 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

func minOf(a, b, c uint8) uint8 {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
