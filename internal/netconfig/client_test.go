package netconfig

import (
	"net/http"
	"net/url"
	"testing"
)

func TestProxySelectionAndDirectOverride(t *testing.T) {
	t.Setenv("SUNO_PROXY", "http://127.0.0.1:9999")
	client, source, err := NewHTTPClient("direct")
	if err != nil || source != "direct" {
		t.Fatalf("direct override failed: %s %v", source, err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("direct still uses a proxy")
	}
	client, source, err = NewHTTPClient("http://127.0.0.1:8888")
	if err != nil || source != "explicit proxy" {
		t.Fatal("explicit proxy selection failed")
	}
	proxy, err := client.Transport.(*http.Transport).Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "suno.com"}})
	if err != nil || proxy.Port() != "8888" {
		t.Fatal("environment overrode explicit proxy")
	}
}

func TestProxyValidationAndWindowsPerSchemeSelection(t *testing.T) {
	if _, _, err := NewHTTPClient("file:///private"); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	if actual := parseSystemProxy("http=127.0.0.1:8080;https=127.0.0.1:8443"); actual != "http://127.0.0.1:8443" {
		t.Fatalf("wrong HTTPS proxy: %s", actual)
	}
}
