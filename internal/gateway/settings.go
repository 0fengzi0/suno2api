package gateway

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"suno2api/internal/netconfig"
)

// gatewaySettings is the operator-editable runtime configuration persisted in
// settings.json. Today it holds the global proxy used by every account
// client (generation, renewal, billing, downloads).
type gatewaySettings struct {
	Proxy string `json:"proxy"`
}

func gatewaySettingsPath(dataDir string) string {
	return filepath.Join(dataDir, "settings.json")
}

func loadGatewaySettings(path string) (gatewaySettings, bool, error) {
	var settings gatewaySettings
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return settings, false, nil
	}
	if err != nil {
		return settings, false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&settings); err != nil {
		return settings, false, errors.New("gateway settings file is not valid JSON")
	}
	settings.Proxy = strings.TrimSpace(settings.Proxy)
	return settings, true, nil
}

func saveGatewaySettings(path string, settings gatewaySettings) error {
	settings.Proxy = strings.TrimSpace(settings.Proxy)
	return writeJSONFile(path, settings)
}

// applyProxyLocked rebuilds the shared HTTP client and every account client
// with the new proxy. In-flight requests are cancelled by the rebuild; the
// caller must hold gateway.mu.
func (gateway *Gateway) applyProxyLocked(value string) (string, error) {
	client, source, err := netconfig.NewHTTPClient(value)
	if err != nil {
		return "", err
	}
	gateway.httpClient = client
	gateway.proxyValue = value
	gateway.proxySource = source
	for id, record := range gateway.state.Accounts {
		if record.DeletedAt != nil {
			continue
		}
		if err := gateway.buildAccountLocked(id); err != nil {
			record.State = "configuration_error"
			record.LastError = "账号客户端配置不可用"
			gateway.logf("error", "account", "账号客户端初始化失败", map[string]any{"account": record.Name, "error": record.LastError})
		}
	}
	return source, nil
}

// validateProxyInput accepts the same values as netconfig: empty for automatic
// detection, "direct" to force no proxy, or an http/https/socks5 URL.
func validateProxyInput(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "direct") {
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5" && parsed.Scheme != "socks5h") {
		return "", errors.New("代理地址须为 http://、https://、socks5://、socks5h://、direct 或留空自动检测")
	}
	return value, nil
}
