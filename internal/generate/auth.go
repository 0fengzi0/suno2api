package generate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var ErrAuthentication = errors.New("Suno authentication required")

func NormalizeToken(value string) (string, error) { return normalizeToken(value) }

func normalizeToken(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[7:])
	}
	if strings.Contains(value, "__session=") || strings.Contains(value, "__session_") {
		request := &http.Request{Header: http.Header{"Cookie": []string{value}}}
		var session string
		for _, cookie := range request.Cookies() {
			if cookie.Name == "__session" {
				session = cookie.Value
				break
			}
			if session == "" && strings.HasPrefix(cookie.Name, "__session_") {
				session = cookie.Value
			}
		}
		if session == "" {
			return "", fmt.Errorf("%w: Cookie does not contain a Suno __session value", ErrAuthentication)
		}
		value = session
	}
	if strings.EqualFold(value, "Bearer") {
		value = ""
	}
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		value = strings.TrimSpace(value[7:])
	}
	if value == "" {
		return "", fmt.Errorf("%w: set SUNO_TOKEN or SUNO_TOKEN_FILE to your own current Suno bearer token", ErrAuthentication)
	}
	if len(value) > 16384 || strings.ContainsAny(value, "\r\n\t ;") {
		return "", errors.New("Suno token must be a single bearer token, not cookies or a JSON document")
	}
	parts := strings.Split(value, ".")
	if len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err == nil {
			var claims struct {
				Expiry int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Expiry > 0 && time.Now().Unix() >= claims.Expiry {
				return "", fmt.Errorf("%w: Suno session token has expired; refresh the Suno session and replace the token file", ErrAuthentication)
			}
		}
	}
	return value, nil
}

func readTokenFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read Suno token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect Suno token file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("Suno token file must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return "", fmt.Errorf("read Suno token file: %w", err)
	}
	if len(data) > 65536 {
		return "", errors.New("Suno token or Cookie file exceeds 64 KiB")
	}
	return normalizeToken(string(data))
}
