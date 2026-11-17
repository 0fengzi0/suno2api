package netconfig

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func NewHTTPClient(explicitProxy string) (*http.Client, string, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	address := strings.TrimSpace(explicitProxy)
	source := "direct"
	if address != "" {
		source = "explicit proxy"
	}
	if address == "" {
		address = strings.TrimSpace(os.Getenv("SUNO_PROXY"))
		if address != "" {
			source = "SUNO_PROXY"
		}
	}
	if address == "" && hasStandardProxy() {
		transport.Proxy = http.ProxyFromEnvironment
		source = "proxy environment"
	}
	if address == "" && source == "direct" {
		address = os.Getenv("ALL_PROXY")
		if address == "" {
			address = os.Getenv("all_proxy")
		}
		if address != "" {
			source = "ALL_PROXY"
		}
	}
	if address == "" && source == "direct" {
		configured, err := systemProxy()
		if err != nil {
			return nil, "", err
		}
		address = configured
		if address != "" {
			source = "Windows system proxy"
		}
	}
	if strings.EqualFold(address, "direct") {
		address = ""
		source = "direct"
		transport.Proxy = nil
	}
	if address != "" {
		parsed, err := url.Parse(address)
		if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5" && parsed.Scheme != "socks5h") {
			return nil, "", errors.New("invalid proxy URL; use http://, https://, socks5:// or direct")
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute}, source, nil
}

func hasStandardProxy() bool {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func parseSystemProxy(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.Contains(value, "=") {
		entries := make(map[string]string)
		for _, entry := range strings.Split(value, ";") {
			key, address, found := strings.Cut(entry, "=")
			if found {
				entries[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(address)
			}
		}
		value = entries["https"]
		if value == "" {
			value = entries["http"]
		}
		if value == "" && entries["socks"] != "" {
			return "socks5://" + strings.TrimPrefix(entries["socks"], "socks5://")
		}
	}
	if value != "" && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	return value
}
