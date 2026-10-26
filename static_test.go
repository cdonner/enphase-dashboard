package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticFiles(t *testing.T) {
	s := NewServer(Config{DataDir: t.TempDir()})
	h := s.routes(fstest.MapFS{
		"index.html":         {Data: []byte("<html>app</html>")},
		"assets/index-a1.js": {Data: []byte("console.log(1)")},
	})
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := get("/appliances"); w.Code != 200 || w.Body.String() != "<html>app</html>" {
		t.Errorf("app route: %d %q", w.Code, w.Body.String())
	}
	if w := get("/assets/index-a1.js"); w.Code != 200 || w.Header().Get("Cache-Control") == "no-cache" {
		t.Errorf("asset: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	// A script from before an update: 404, not the app's HTML.
	if w := get("/assets/index-old.js"); w.Code != http.StatusNotFound {
		t.Errorf("old asset: %d", w.Code)
	}
}
