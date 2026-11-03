package branding

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranding(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "logo.PNG")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nfake"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := New("<acme>", png, "", "")
	if err != nil {
		t.Fatal(err)
	}
	logo := httptest.NewRecorder()
	h.Logo(logo, httptest.NewRequest("GET", "/branding/logo", nil))
	if h.HasWideLogo() {
		t.Fatal("wide logo reported without a file")
	}
	if logo.Code != 200 || logo.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(logo.Body.String(), "\x89PNG") {
		t.Fatalf("custom logo: %d %q", logo.Code, logo.Header().Get("Content-Type"))
	}
	favicon := httptest.NewRecorder()
	h.Favicon(favicon, httptest.NewRequest("GET", "/branding/favicon", nil))
	body := favicon.Body.String()
	if favicon.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(body, ">&lt;</text>") || !strings.Contains(favicon.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("default favicon must be an escaped letter badge: %s", body)
	}

	exe := filepath.Join(dir, "logo.exe")
	big := filepath.Join(dir, "big.png")
	if err := os.WriteFile(exe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{exe, big, filepath.Join(dir, "missing.svg"), dir + "/"} {
		if _, err := New("Acme", "", bad, ""); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if h, err := New("Acme", "", png, ""); err != nil || !h.HasWideLogo() {
		t.Fatalf("wide logo not enabled: %v", err)
	}
}
