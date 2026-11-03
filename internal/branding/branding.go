// Package branding serves the sidebar logos and favicon from operator-supplied
// files (APP_LOGO_FILE, APP_LOGO_WIDE_FILE, APP_FAVICON_FILE) or a generated
// default badge.
// Images are served as-is; templates scale them with CSS, so any size works.
package branding

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes = 2 << 20

var contentTypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".ico":  "image/x-icon",
}

type asset struct {
	body        []byte
	contentType string
	modTime     time.Time
}

type Handler struct {
	logo, wideLogo, favicon asset
	wide                    bool
}

// New loads every file at startup so a bad path fails boot, not page loads.
// An empty logo or favicon path uses the default badge built from appName's
// first letter; an empty wide logo path keeps the badge plus app name.
func New(appName, logoFile, wideLogoFile, faviconFile string) (*Handler, error) {
	fallback := defaultBadge(appName)
	logo, err := load("APP_LOGO_FILE", logoFile, fallback)
	if err != nil {
		return nil, err
	}
	wideLogo, err := load("APP_LOGO_WIDE_FILE", wideLogoFile, fallback)
	if err != nil {
		return nil, err
	}
	favicon, err := load("APP_FAVICON_FILE", faviconFile, fallback)
	if err != nil {
		return nil, err
	}
	return &Handler{logo: logo, wideLogo: wideLogo, favicon: favicon, wide: wideLogoFile != ""}, nil
}

// HasWideLogo reports whether the expanded sidebar replaces the app name.
func (h *Handler) HasWideLogo() bool { return h.wide }

func load(key, path string, fallback asset) (asset, error) {
	if path == "" {
		return fallback, nil
	}
	contentType, ok := contentTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return asset{}, fmt.Errorf("%s must be an .svg, .png, .jpg, .jpeg, .webp, .gif, or .ico file", key)
	}
	info, err := os.Stat(path)
	if err != nil {
		return asset{}, fmt.Errorf("%s: %w", key, err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return asset{}, fmt.Errorf("%s must be a regular file of at most %d bytes", key, MaxFileBytes)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return asset{}, fmt.Errorf("%s: %w", key, err)
	}
	return asset{body: body, contentType: contentType, modTime: info.ModTime()}, nil
}

func defaultBadge(appName string) asset {
	letter, _ := utf8.DecodeRuneInString(strings.TrimSpace(appName))
	if letter == utf8.RuneError {
		letter = 'A'
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 40 40"><rect width="40" height="40" rx="10" fill="#10b981"/>` +
		`<text x="20" y="27" text-anchor="middle" font-family="system-ui,sans-serif" font-size="20" font-weight="700" fill="#020617">` +
		html.EscapeString(string(unicode.ToUpper(letter))) + `</text></svg>`
	return asset{body: []byte(svg), contentType: "image/svg+xml", modTime: time.Unix(0, 0)}
}

func (h *Handler) Logo(writer http.ResponseWriter, request *http.Request) {
	serve(writer, request, h.logo)
}

func (h *Handler) WideLogo(writer http.ResponseWriter, request *http.Request) {
	serve(writer, request, h.wideLogo)
}

func (h *Handler) Favicon(writer http.ResponseWriter, request *http.Request) {
	serve(writer, request, h.favicon)
}

func serve(writer http.ResponseWriter, request *http.Request, a asset) {
	writer.Header().Set("Content-Type", a.contentType)
	// Revalidate every load so a replaced file shows after restart; 304s are cheap.
	writer.Header().Set("Cache-Control", "no-cache")
	// SVG opened directly must not run scripts or load anything.
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	http.ServeContent(writer, request, "", a.modTime, bytes.NewReader(a.body))
}
