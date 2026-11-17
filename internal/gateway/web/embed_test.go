package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedAssetsAreAvailableAndUnknownPathsStayPrivate(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/", "/assets/app.css", "/assets/app.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 || response.Body.Len() < 100 {
			t.Fatalf("missing embedded UI asset %s: %d", path, response.Code)
		}
	}
	for _, path := range []string{"/assets/", "/embed.go", "/assets/../../embed.go", "/unknown"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 404 || strings.Contains(response.Body.String(), "package web") {
			t.Fatalf("unexpected embedded filesystem disclosure on %s", path)
		}
	}
}
