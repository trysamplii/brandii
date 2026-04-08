// Package brandii extracts brand assets (logos, colors, backdrop images and a
// brand name) from an arbitrary website by parsing the rendered HTML.
//
// It is a Go port of OpenBrand's TypeScript scraper
// (https://github.com/takclark/OpenBrand) and is intended to be embedded in
// other Go programs as a library.
//
// Basic usage:
//
//	res, err := brandii.Extract(ctx, "https://example.com")
//	if err != nil { ... }
//	fmt.Println(res.BrandName, res.Colors)
package brandii

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// LogoAsset is a logo image discovered on the page.
type LogoAsset struct {
	URL        string      `json:"url"`
	Alt        string      `json:"alt,omitempty"`
	Type       LogoType    `json:"type,omitempty"`
	Resolution *Resolution `json:"resolution,omitempty"`
}

// LogoType classifies a discovered logo.
type LogoType string

const (
	LogoTypeImg            LogoType = "img"
	LogoTypeSVG            LogoType = "svg"
	LogoTypeFavicon        LogoType = "favicon"
	LogoTypeAppleTouchIcon LogoType = "apple-touch-icon"
	LogoTypeIcon           LogoType = "icon"
	LogoTypeLogo           LogoType = "logo"
)

// Resolution describes an image's pixel dimensions.
type Resolution struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	AspectRatio float64 `json:"aspect_ratio"`
}

// ColorAsset is a brand color discovered on the page.
type ColorAsset struct {
	Hex   string    `json:"hex"`
	Usage ColorRole `json:"usage,omitempty"`
}

// ColorRole describes how a brand color is used.
type ColorRole string

const (
	ColorRolePrimary    ColorRole = "primary"
	ColorRoleSecondary  ColorRole = "secondary"
	ColorRoleAccent     ColorRole = "accent"
	ColorRoleBackground ColorRole = "background"
	ColorRoleText       ColorRole = "text"
)

// BackdropAsset is a hero / background image found on the page.
type BackdropAsset struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// Result is the output of an extraction.
type Result struct {
	BrandName      string          `json:"brand_name"`
	Logos          []LogoAsset     `json:"logos"`
	Colors         []ColorAsset    `json:"colors"`
	BackdropImages []BackdropAsset `json:"backdrop_images"`
}

// ErrorCode is a machine-readable classification of an extraction failure.
type ErrorCode string

const (
	ErrCodeAccessBlocked ErrorCode = "ACCESS_BLOCKED"
	ErrCodeNotFound      ErrorCode = "NOT_FOUND"
	ErrCodeServerError   ErrorCode = "SERVER_ERROR"
	ErrCodeNetworkError  ErrorCode = "NETWORK_ERROR"
	ErrCodeEmptyContent  ErrorCode = "EMPTY_CONTENT"
)

// Error is the typed error returned by [Client.Extract] / [Extract] when an
// extraction fails. Callers can use errors.As to recover the code and HTTP
// status.
type Error struct {
	Code    ErrorCode
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// DefaultUserAgent is the User-Agent header sent with HTTP requests when no
// override is configured. Mirrors the upstream OpenBrand scraper.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// minBodyLength is the threshold below which a direct fetch is considered
// "too empty" and we retry through the Jina reader fallback.
const minBodyLength = 500

// Client extracts brand assets. The zero value is usable; configure fields
// before calling Extract for non-default behavior.
type Client struct {
	// HTTPClient is the http.Client used for all outbound requests. If nil,
	// a client with a 15s timeout is used.
	HTTPClient *http.Client

	// UserAgent is sent on outbound HTTP requests. If empty, DefaultUserAgent
	// is used.
	UserAgent string

	// DisableJinaFallback turns off the r.jina.ai fallback that would
	// otherwise be used when the direct fetch fails or returns very little
	// content.
	DisableJinaFallback bool
}

// New returns a Client with sensible defaults.
func New() *Client { return &Client{} }

var defaultClient = &Client{}

// Extract is a convenience wrapper around (*Client).Extract using the package
// default Client.
func Extract(ctx context.Context, pageURL string) (*Result, error) {
	return defaultClient.Extract(ctx, pageURL)
}

// Extract fetches pageURL, parses its HTML, and returns the extracted brand
// assets. On failure it returns an error of type *Error.
func (c *Client) Extract(ctx context.Context, pageURL string) (*Result, error) {
	pageURL = normalizeURL(pageURL)

	html, status, fetchErr := c.fetchPage(ctx, pageURL)

	bodyText := ""
	if html != "" {
		if doc, err := goquery.NewDocumentFromReader(strings.NewReader(html)); err == nil {
			bodyText = strings.TrimSpace(doc.Find("body").Text())
		}
	}

	// Fall back to Jina when the direct fetch failed or returned too little.
	if !c.DisableJinaFallback && (fetchErr != nil || status < 200 || status >= 300 || len(bodyText) < minBodyLength) {
		if jina, err := c.fetchViaJina(ctx, pageURL); err == nil && jina != "" {
			html = jina
		} else if fetchErr != nil {
			return nil, &Error{Code: ErrCodeNetworkError, Message: fmt.Sprintf("network error: %v", fetchErr)}
		} else if status < 200 || status >= 300 {
			return nil, classifyHTTPError(status)
		}
	} else if fetchErr != nil {
		return nil, &Error{Code: ErrCodeNetworkError, Message: fmt.Sprintf("network error: %v", fetchErr)}
	} else if status < 200 || status >= 300 {
		return nil, classifyHTTPError(status)
	}

	res, err := c.parseHTML(ctx, html, pageURL)
	if err != nil {
		return nil, err
	}
	if len(res.Logos) == 0 && len(res.Colors) == 0 && len(res.BackdropImages) == 0 {
		return nil, &Error{
			Code:    ErrCodeEmptyContent,
			Message: "the page loaded but no brand assets (logos, colors, or images) were found",
		}
	}
	return res, nil
}

func normalizeURL(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return DefaultUserAgent
}

// fetchPage GETs pageURL and returns the body, status code, and any transport
// error. A non-2xx status is reported via the status return, not as an error.
func (c *Client) fetchPage(ctx context.Context, pageURL string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // cap at 5 MB
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}

// fetchViaJina retrieves the page through r.jina.ai's HTML reader, used as a
// last-resort bypass when bot protection blocks the direct fetch.
func (c *Client) fetchViaJina(ctx context.Context, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://r.jina.ai/"+pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-Return-Format", "html")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("jina fallback returned non-2xx")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func classifyHTTPError(status int) *Error {
	switch {
	case status == http.StatusForbidden:
		return &Error{
			Code:   ErrCodeAccessBlocked,
			Status: status,
			Message: "the website blocked the request — Cloudflare or other bot " +
				"protection is likely active on the target site",
		}
	case status == http.StatusNotFound:
		return &Error{
			Code:    ErrCodeNotFound,
			Status:  status,
			Message: "the page was not found on the target website (404)",
		}
	case status >= 500:
		return &Error{
			Code:    ErrCodeServerError,
			Status:  status,
			Message: fmt.Sprintf("the target website returned a server error (%d)", status),
		}
	default:
		return &Error{
			Code:    ErrCodeAccessBlocked,
			Status:  status,
			Message: fmt.Sprintf("the website returned an error (HTTP %d) and the fallback fetcher also failed", status),
		}
	}
}
