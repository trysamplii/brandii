// brandii-demo is a tiny local web app for poking at the brandii extractor.
// Run it with `go run ./cmd/brandii-demo` and open http://localhost:8080.
package main

import (
	"flag"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/trysamplii/brandii"
)

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>brandii demo</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.4 -apple-system, system-ui, sans-serif; max-width: 960px; margin: 2rem auto; padding: 0 1rem; }
  h1 { margin-bottom: .25rem; }
  form { display: grid; grid-template-columns: 1fr auto auto auto auto; gap: .5rem; align-items: end; margin: 1rem 0 2rem; }
  form label { display: flex; flex-direction: column; font-size: 11px; color: #666; text-transform: uppercase; letter-spacing: .04em; gap: .2rem; }
  input[type=url], input[type=color], input[type=number] { padding: .55rem .65rem; font-size: 1rem; border: 1px solid #ccc; border-radius: 6px; background: white; }
  input[type=number] { width: 5rem; }
  input[type=color] { padding: 0; height: 2.4rem; width: 3rem; }
  button { padding: .55rem 1rem; font-size: 1rem; border: 0; border-radius: 6px; background: #4f46e5; color: white; cursor: pointer; }
  button:hover { background: #4338ca; }
  .err { padding: .75rem 1rem; border-radius: 6px; background: #fee; color: #900; border: 1px solid #fcc; }
  section { margin: 1.5rem 0; }
  h2 { font-size: 1rem; text-transform: uppercase; letter-spacing: .05em; color: #666; margin-bottom: .5rem; }

  .swatches { display: flex; gap: .75rem; flex-wrap: wrap; }
  .swatch { display: flex; flex-direction: column; align-items: center; }
  .swatch .chip { width: 88px; height: 88px; border-radius: 8px; border: 1px solid #0002; }
  .swatch code { font-size: 12px; margin-top: .25rem; }
  .swatch small { color: #888; font-size: 11px; }

  .adaptive-grid { display: grid; gap: 1rem; }
  .adaptive-row { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: .5rem; border: 1px solid #ddd; border-radius: 8px; overflow: hidden; }
  .adaptive-cell { padding: 1rem; display: flex; flex-direction: column; gap: .35rem; min-height: 120px; }
  .adaptive-cell.original { background: #f6f6f6; }
  .adaptive-cell.light { background: var(--light-base, #ffffff); color: #111; }
  .adaptive-cell.dark { background: var(--dark-base, #0a0a0a); color: #eee; }
  .adaptive-cell .label { font-size: 10px; text-transform: uppercase; letter-spacing: .05em; opacity: .65; }
  .adaptive-cell .preview { font-weight: 600; font-size: 1.2rem; }
  .adaptive-cell code { font-size: 12px; opacity: .85; }
  .adaptive-cell .ratio { font-size: 11px; opacity: .7; }
  .adjusted-tag { display: inline-block; font-size: 10px; padding: .1rem .35rem; border-radius: 3px; background: #eab30844; color: #92400e; margin-left: .25rem; }

  .logos, .backdrops { display: flex; gap: 1rem; flex-wrap: wrap; }
  .logo, .backdrop { border: 1px solid #ddd; border-radius: 8px; padding: .5rem; background: #fafafa; max-width: 220px; }
  .logo img { max-width: 200px; max-height: 120px; display: block; margin: 0 auto; }
  .backdrop img { max-width: 200px; max-height: 140px; display: block; }
  .meta { font-size: 11px; color: #666; margin-top: .35rem; word-break: break-all; }
  .brand-name { font-size: 1.5rem; font-weight: 600; }
  .timing { color: #888; font-size: 12px; }
</style>
</head>
<body>
  <h1>brandii demo</h1>
  <p style="color:#666;margin-top:0">Type a URL, see what the scraper finds.</p>

  <form method="GET" action="/">
    <label>URL
      <input type="url" name="url" value="{{.URL}}" placeholder="https://stripe.com" autofocus required>
    </label>
    <label>Light bg
      <input type="color" name="light" value="{{.LightBase}}">
    </label>
    <label>Dark bg
      <input type="color" name="dark" value="{{.DarkBase}}">
    </label>
    <label>Min contrast
      <input type="number" name="min" min="1" max="21" step="0.5" value="{{.MinContrast}}">
    </label>
    <button type="submit">Extract</button>
  </form>

  {{if .Err}}
    <div class="err"><strong>{{.ErrCode}}</strong> — {{.Err}}</div>
  {{end}}

  {{with .Adaptive}}
    <section>
      <h2>Brand name</h2>
      <div class="brand-name">{{.BrandName}}</div>
      <div class="timing">extracted in {{$.Elapsed}}</div>
    </section>

    {{if .Colors}}
    <section>
      <h2>Original colors</h2>
      <div class="swatches">
        {{range .Colors}}
          <div class="swatch">
            <div class="chip" style="background: {{.Hex}}"></div>
            <code>{{.Hex}}</code>
            <small>{{.Usage}}</small>
          </div>
        {{end}}
      </div>
    </section>
    {{end}}

    {{if .AdaptiveColors}}
    <section>
      <h2>Adaptive variants (min {{printf "%.1f" .MinContrast}}:1)</h2>
      <div class="adaptive-grid" style="--light-base: {{.LightBase}}; --dark-base: {{.DarkBase}};">
        {{range .AdaptiveColors}}
          <div class="adaptive-row">
            <div class="adaptive-cell original">
              <span class="label">original {{with .Usage}}· {{.}}{{end}}</span>
              <span class="preview" style="color: {{.Original}}">Sample text</span>
              <code>{{.Original}}</code>
            </div>
            <div class="adaptive-cell light">
              <span class="label">light variant
                {{if .LightAdjusted}}<span class="adjusted-tag">adjusted</span>{{end}}
              </span>
              <span class="preview" style="color: {{.Light}}">Sample text</span>
              <code>{{.Light}}</code>
              <span class="ratio">{{printf "%.2f" .LightContrast}}:1 vs {{$.Adaptive.LightBase}}</span>
            </div>
            <div class="adaptive-cell dark">
              <span class="label">dark variant
                {{if .DarkAdjusted}}<span class="adjusted-tag">adjusted</span>{{end}}
              </span>
              <span class="preview" style="color: {{.Dark}}">Sample text</span>
              <code>{{.Dark}}</code>
              <span class="ratio">{{printf "%.2f" .DarkContrast}}:1 vs {{$.Adaptive.DarkBase}}</span>
            </div>
          </div>
        {{end}}
      </div>
    </section>
    {{end}}

    {{if .Logos}}
    <section>
      <h2>Logos ({{len .Logos}})</h2>
      <div class="logos">
        {{range .Logos}}
          <div class="logo">
            <img src="{{.URL}}" alt="{{.Alt}}" loading="lazy">
            <div class="meta">
              <strong>{{.Type}}</strong>
              {{with .Resolution}} · {{.Width}}×{{.Height}}{{end}}
              <br><a href="{{.URL}}" target="_blank" rel="noopener">source</a>
            </div>
          </div>
        {{end}}
      </div>
    </section>
    {{end}}

    {{if .BackdropImages}}
    <section>
      <h2>Backdrops ({{len .BackdropImages}})</h2>
      <div class="backdrops">
        {{range .BackdropImages}}
          <div class="backdrop">
            <img src="{{.URL}}" alt="" loading="lazy">
            <div class="meta">
              {{.Description}}<br>
              <a href="{{.URL}}" target="_blank" rel="noopener">source</a>
            </div>
          </div>
        {{end}}
      </div>
    </section>
    {{end}}
  {{end}}
</body>
</html>`))

type pageData struct {
	URL         string
	LightBase   string
	DarkBase    string
	MinContrast float64
	Adaptive    *brandii.AdaptiveResult
	Err         string
	ErrCode     string
	Elapsed     string
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	client := brandii.New()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := pageData{
			URL:         r.URL.Query().Get("url"),
			LightBase:   firstNonEmpty(r.URL.Query().Get("light"), "#ffffff"),
			DarkBase:    firstNonEmpty(r.URL.Query().Get("dark"), "#0a0a0a"),
			MinContrast: parseFloat(r.URL.Query().Get("min"), 4.5),
		}
		if data.URL != "" {
			start := time.Now()
			res, err := client.ExtractAdaptive(r.Context(), data.URL, brandii.AdaptiveOptions{
				LightBase:   data.LightBase,
				DarkBase:    data.DarkBase,
				MinContrast: data.MinContrast,
			})
			data.Elapsed = time.Since(start).Round(time.Millisecond).String()
			if err != nil {
				data.Err = err.Error()
				if be, ok := err.(*brandii.Error); ok {
					data.ErrCode = string(be.Code)
				} else {
					data.ErrCode = "ERROR"
				}
			} else {
				data.Adaptive = res
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, data); err != nil {
			log.Printf("template: %v", err)
		}
	})

	log.Printf("brandii demo listening on http://localhost%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func firstNonEmpty(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}

func parseFloat(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return f
}
