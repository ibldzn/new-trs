package render_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ibldzn/trs/internal/render"
	webfiles "github.com/ibldzn/trs/web"
)

func TestRendererParsesEmbeddedAndDevelopmentTrees(t *testing.T) {
	if _, err := render.New(webfiles.Files, false); err != nil {
		t.Fatalf("embedded templates: %v", err)
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test")
	}
	webRoot := filepath.Join(filepath.Dir(filename), "..", "..", "web")
	if _, err := render.New(os.DirFS(webRoot), true); err != nil {
		t.Fatalf("development templates: %v", err)
	}
}
