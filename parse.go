package brandii

import (
	"context"
	"encoding/base64"
	"image"
	"net/url"
	"regexp"
	"strings"
	"sync"

	// Register image format decoders so DecodeConfig can read them.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"github.com/PuerkitoBio/goquery"
)

// parseHTML walks an HTML document and assembles a Result. baseURL is the
// canonical page URL used to resolve relative asset paths.
func (c *Client) parseHTML(ctx context.Context, html, baseURL string) (*Result, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, &Error{Code: ErrCodeEmptyContent, Message: "failed to parse HTML"}
	}

	domain := domainName(baseURL)

	logos, imgBackdrops := c.extractImages(ctx, doc, baseURL, domain)
	colors := c.extractColors(ctx, doc, baseURL, logos)
	cssBackdrops := extractCSSBackdrops(doc, baseURL)

	return &Result{
		BrandName:      extractBrandName(doc, domain),
		Logos:          logos,
		Colors:         colors,
		BackdropImages: append(cssBackdrops, imgBackdrops...),
	}, nil
}

// ── Image extraction & classification ───────────────────────────────────────

// imageCandidate is a single image found in the HTML, before classification.
type imageCandidate struct {
	url             string
	alt             string
	source          string // "favicon" | "apple-touch-icon" | "img" | "svg"
	location        string // "header" | "footer" | "body"
	hasLogoHint     bool
	hasDomainMatch  bool
	isInHeroSection bool
	resolution      *Resolution
}

func (c *Client) extractImages(
	ctx context.Context,
	doc *goquery.Document,
	baseURL, domain string,
) ([]LogoAsset, []BackdropAsset) {
	var candidates []*imageCandidate
	seen := make(map[string]struct{})

	add := func(c *imageCandidate) {
		if c.url == "" {
			return
		}
		c.url = stripQueryParams(c.url)
		if _, ok := seen[c.url]; ok {
			return
		}
		seen[c.url] = struct{}{}
		candidates = append(candidates, c)
	}

	// Favicons
	doc.Find(`link[rel="icon"], link[rel="shortcut icon"]`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if u := resolveURL(href, baseURL); u != "" {
			add(&imageCandidate{url: u, source: "favicon", location: "header"})
		}
	})

	// Apple touch icons
	doc.Find(`link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if u := resolveURL(href, baseURL); u != "" {
			add(&imageCandidate{url: u, source: "apple-touch-icon", location: "header"})
		}
	})

	// All <img> tags
	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		src, _ := s.Attr("src")
		u := resolveURL(src, baseURL)
		if u == "" {
			return
		}
		alt, _ := s.Attr("alt")
		cls, _ := s.Attr("class")
		id, _ := s.Attr("id")
		combined := strings.ToLower(src + " " + alt + " " + cls + " " + id)

		location := "body"
		if s.Closest(`header, nav, [role="banner"]`).Length() > 0 {
			location = "header"
		} else if s.Closest("footer").Length() > 0 {
			location = "footer"
		}

		hasLogoHint := strings.Contains(combined, "logo")
		hasDomainMatch := domain != "" &&
			(strings.Contains(strings.ToLower(src), domain) || strings.Contains(strings.ToLower(alt), domain))

		isInHero := s.Closest(`[class*="hero"], [class*="banner"], [class*="backdrop"], [class*="jumbotron"], [class*="splash"]`).Length() > 0

		add(&imageCandidate{
			url:             u,
			alt:             alt,
			source:          "img",
			location:        location,
			hasLogoHint:     hasLogoHint,
			hasDomainMatch:  hasDomainMatch,
			isInHeroSection: isInHero,
		})
	})

	// Inline SVGs in logo-like containers in header/nav
	doc.Find(`header, nav, [role="banner"]`).
		Find(`[class*="logo"], [id*="logo"], [aria-label*="logo"]`).
		Each(func(_ int, s *goquery.Selection) {
			svg := s.Find("svg").First()
			if svg.Length() == 0 {
				return
			}
			html, err := goquery.OuterHtml(svg)
			if err != nil {
				return
			}
			dataURI := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(html))
			add(&imageCandidate{
				url:         dataURI,
				source:      "svg",
				location:    "header",
				hasLogoHint: true,
			})
		})

	// Probe dimensions in parallel for non-data URLs.
	var wg sync.WaitGroup
	for _, cand := range candidates {
		if strings.HasPrefix(cand.url, "data:") {
			continue
		}
		wg.Add(1)
		go func(cand *imageCandidate) {
			defer wg.Done()
			if res, ok := c.probeImage(ctx, cand.url); ok {
				cand.resolution = res
			}
		}(cand)
	}
	wg.Wait()

	// ── Pass 1: high-confidence logos + all backdrops ──
	var logos []LogoAsset
	var backdrops []BackdropAsset
	for _, cand := range candidates {
		var width, height int
		var ar float64
		if cand.resolution != nil {
			width, height, ar = cand.resolution.Width, cand.resolution.Height, cand.resolution.AspectRatio
		}

		switch cand.source {
		case "favicon":
			logos = append(logos, newLogo(cand, LogoTypeFavicon))
			continue
		case "apple-touch-icon":
			logos = append(logos, newLogo(cand, LogoTypeAppleTouchIcon))
			continue
		case "svg":
			logos = append(logos, newLogo(cand, LogoTypeSVG))
			continue
		}

		// Header/nav <img> with logo hint or domain match, width ≤ 500 → logo.
		if cand.source == "img" && cand.location == "header" && (cand.hasLogoHint || cand.hasDomainMatch) {
			if width == 0 || width <= 500 {
				logos = append(logos, newLogo(cand, classifyLogoType(width, height, ar)))
				continue
			}
		}

		// Hero/banner section, width ≥ 400 → backdrop.
		if cand.isInHeroSection && width >= 400 {
			backdrops = append(backdrops, BackdropAsset{URL: cand.url, Description: "Hero/banner image"})
			continue
		}
	}

	// ── Pass 2: low-confidence logos (only if none found in pass 1) ──
	if len(logos) == 0 {
		for _, cand := range candidates {
			if cand.source != "img" {
				continue
			}
			var width, height int
			var ar float64
			if cand.resolution != nil {
				width, height, ar = cand.resolution.Width, cand.resolution.Height, cand.resolution.AspectRatio
			}

			if (cand.location == "footer" || cand.location == "body") && cand.hasLogoHint && cand.hasDomainMatch {
				if width == 0 || width <= 500 {
					logos = append(logos, newLogo(cand, classifyLogoType(width, height, ar)))
				}
			}
		}
	}

	return logos, backdrops
}

func newLogo(c *imageCandidate, t LogoType) LogoAsset {
	return LogoAsset{
		URL:        c.url,
		Alt:        c.alt,
		Type:       t,
		Resolution: c.resolution,
	}
}

func classifyLogoType(width, height int, aspectRatio float64) LogoType {
	if width == 0 || height == 0 || aspectRatio == 0 {
		return LogoTypeImg
	}
	if width <= 64 && height <= 64 {
		return LogoTypeIcon
	}
	if width <= 256 && aspectRatio >= 0.8 && aspectRatio <= 1.2 {
		return LogoTypeIcon
	}
	if width <= 500 && aspectRatio > 1.5 {
		return LogoTypeLogo
	}
	return LogoTypeImg
}

// probeImage downloads just enough of an image to read its dimensions via
// image.DecodeConfig. SVGs and other unsupported formats yield (nil, false).
func (c *Client) probeImage(ctx context.Context, imgURL string) (*Resolution, bool) {
	body, err := c.fetchBytes(ctx, imgURL, 256<<10) // 256 KB is enough for headers
	if err != nil {
		return nil, false
	}
	cfg, _, err := image.DecodeConfig(strings.NewReader(string(body)))
	if err != nil {
		return nil, false
	}
	ar := 0.0
	if cfg.Height > 0 {
		ar = roundTo2(float64(cfg.Width) / float64(cfg.Height))
	}
	return &Resolution{Width: cfg.Width, Height: cfg.Height, AspectRatio: ar}, true
}

func roundTo2(f float64) float64 {
	// Match the upstream `+(width / height).toFixed(2)` formatting.
	return float64(int(f*100+0.5)) / 100
}

// ── Backdrops (CSS / og:image) ──────────────────────────────────────────────

var bgImageRE = regexp.MustCompile(`(?i)background(?:-image)?\s*:[^;]*url\(["']?([^"')]+)["']?\)`)

func extractCSSBackdrops(doc *goquery.Document, baseURL string) []BackdropAsset {
	var backdrops []BackdropAsset
	seen := make(map[string]struct{})

	add := func(rawURL, description string) {
		if rawURL == "" {
			return
		}
		u := stripQueryParams(rawURL)
		if _, ok := seen[u]; ok {
			return
		}
		if strings.HasPrefix(u, "data:") || strings.HasSuffix(u, ".svg") {
			return
		}
		seen[u] = struct{}{}
		backdrops = append(backdrops, BackdropAsset{URL: u, Description: description})
	}

	doc.Find(`meta[property="og:image"]`).Each(func(_ int, s *goquery.Selection) {
		content, _ := s.Attr("content")
		add(resolveURL(content, baseURL), "Open Graph image")
	})

	doc.Find("style").Each(func(_ int, s *goquery.Selection) {
		for _, m := range bgImageRE.FindAllStringSubmatch(s.Text(), -1) {
			add(resolveURL(m[1], baseURL), "CSS background image")
		}
	})

	doc.Find("[style]").Each(func(_ int, s *goquery.Selection) {
		style, _ := s.Attr("style")
		if !strings.Contains(style, "url(") {
			return
		}
		for _, m := range bgImageRE.FindAllStringSubmatch(style, -1) {
			add(resolveURL(m[1], baseURL), "Background image")
		}
	})

	return backdrops
}

// ── Brand name ──────────────────────────────────────────────────────────────

var logoCleanupRE = regexp.MustCompile(`(?i)\s*(logo|icon|image|img)\s*`)
var titleSeparatorRE = regexp.MustCompile(`\s*[|\-—–•·]\s*`)

func extractBrandName(doc *goquery.Document, domain string) string {
	if v, ok := doc.Find(`meta[property="og:site_name"]`).Attr("content"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := doc.Find(`meta[name="application-name"]`).Attr("content"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}

	// Logo alt text in header/nav.
	var logoAlt string
	doc.Find("header img, nav img").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		src, _ := s.Attr("src")
		alt, _ := s.Attr("alt")
		cls, _ := s.Attr("class")
		combined := strings.ToLower(src + " " + alt + " " + cls)
		if strings.Contains(combined, "logo") {
			logoAlt = alt
			return false
		}
		return true
	})
	if logoAlt != "" {
		cleaned := strings.TrimSpace(logoCleanupRE.ReplaceAllString(logoAlt, ""))
		if cleaned != "" {
			return cleaned
		}
	}

	if title := strings.TrimSpace(doc.Find("title").Text()); title != "" {
		segments := titleSeparatorRE.Split(title, -1)
		if len(segments) > 1 {
			shortest := segments[0]
			for _, seg := range segments[1:] {
				if len(seg) < len(shortest) {
					shortest = seg
				}
			}
			return strings.TrimSpace(shortest)
		}
		return strings.TrimSpace(segments[0])
	}

	if domain != "" {
		return strings.ToUpper(domain[:1]) + domain[1:]
	}
	return ""
}

// ── URL helpers ─────────────────────────────────────────────────────────────

func stripQueryParams(s string) string {
	if strings.HasPrefix(s, "data:") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.RawQuery = ""
	return u.String()
}

func resolveURL(href, baseURL string) string {
	if href == "" {
		return ""
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// domainName returns the leftmost label of a hostname (minus "www."), used to
// match site-owned assets — e.g. "https://www.klarity.ai" → "klarity".
func domainName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	parts := strings.Split(host, ".")
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(parts[0])
}
