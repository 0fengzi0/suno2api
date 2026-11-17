package session

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	SessionID string `json:"sid"`
	Issued    int64  `json:"iat"`
	Expiry    int64  `json:"exp"`
}

type cookiePair struct{ name, value string }

type credential struct {
	raw         []byte
	fingerprint [32]byte
	token       string
	claims      claims
	cookies     []cookiePair
	sessionName string
	canRefresh  bool
	auth        *authCredential
	authError   error
}

type authCredential struct {
	raw     []byte
	cookies []cookiePair
}

func (manager *Manager) loadCredential() (credential, error) {
	source, err := readCredential(manager.path)
	if err != nil {
		return credential{}, err
	}
	if manager.authPath == "" {
		return source, nil
	}
	raw, err := readSnapshot(manager.authPath)
	if err != nil {
		source.canRefresh = false
		source.authError = &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
		source.fingerprint = sha256.Sum256(append(append([]byte{}, source.raw...), []byte("\x00missing-auth-cookie-file")...))
		return source, nil
	}
	request := http.Request{Header: http.Header{"Cookie": []string{cookieHeaderValue(raw)}}}
	provider := &authCredential{raw: raw}
	for _, cookie := range request.Cookies() {
		provider.cookies = append(provider.cookies, cookiePair{name: cookie.Name, value: cookie.Value})
		if sessionCookie(cookie.Name) {
			claims, err := parseClaims(cookie.Value)
			if err != nil || claims.Subject != source.claims.Subject || claims.SessionID != source.claims.SessionID {
				source.authError = &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
			}
		}
	}
	source.auth = provider
	source.fingerprint = sha256.Sum256(append(append(append([]byte{}, source.raw...), 0), raw...))
	validCookie := false
	for _, cookie := range provider.cookies {
		if authCookie(cookie.name) && cookie.value != "" {
			validCookie = true
		}
	}
	source.canRefresh = source.authError == nil && validCookie
	return source, nil
}

func readCredential(path string) (credential, error) {
	file, err := os.Open(path)
	if err != nil {
		return credential{}, &Error{Code: "authentication_required", cause: ErrAuthentication}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return credential{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return credential{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	result := credential{raw: raw, fingerprint: sha256.Sum256(raw)}
	value := cookieHeaderValue(raw)
	if strings.Contains(value, "__session=") || strings.Contains(value, "__session_") {
		request := http.Request{Header: http.Header{"Cookie": []string{value}}}
		for _, cookie := range request.Cookies() {
			result.cookies = append(result.cookies, cookiePair{name: cookie.Name, value: cookie.Value})
			if sessionCookie(cookie.Name) && (result.token == "" || cookie.Name == "__session") {
				result.token = cookie.Value
				result.sessionName = cookie.Name
			}
		}
		result.canRefresh = result.token != ""
	} else {
		if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
			value = strings.TrimSpace(value[7:])
		}
		result.token = value
	}
	parsed, err := parseClaims(result.token)
	if err != nil {
		return credential{}, err
	}
	result.claims = parsed
	return result, nil
}

func cookieHeaderValue(raw []byte) string {
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "\ufeff"))
	if len(value) >= 7 && strings.EqualFold(value[:7], "Cookie:") {
		value = strings.TrimSpace(value[7:])
	}
	return value
}

func parseClaims(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" || len(token) > 16384 || strings.ContainsAny(token, "\r\n\t ;") {
		return claims{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	var object map[string]json.RawMessage
	var parsed claims
	if json.Unmarshal(payload, &object) != nil || json.Unmarshal(payload, &parsed) != nil {
		return claims{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	if _, found := object["x-ably-capability"]; found {
		return claims{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	if parsed.Issuer != "https://auth.suno.com" || parsed.Subject == "" || parsed.SessionID == "" || len(parsed.SessionID) > 256 || len(parsed.Subject) > 256 || parsed.Issued <= 0 || parsed.Expiry <= parsed.Issued || strings.ContainsAny(parsed.SessionID, "/?#\r\n\t ") {
		return claims{}, &Error{Code: "unsupported_credential", cause: ErrUnsupportedCredential}
	}
	return parsed, nil
}

func sessionCookie(name string) bool {
	if name == "__session" {
		return true
	}
	if !strings.HasPrefix(name, "__session_") || len(name) > 128 {
		return false
	}
	for _, character := range name[len("__session_"):] {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return len(name) > len("__session_")
}

func (source credential) requestCookies() string {
	var pairs []cookiePair
	names := make(map[string]bool)
	if source.auth != nil {
		for _, cookie := range source.auth.cookies {
			if authCookie(cookie.name) {
				pairs = append(pairs, cookie)
				names[cookie.name] = true
			}
		}
	}
	for _, cookie := range source.cookies {
		if cookie.name == source.sessionName || (sessionCookie(cookie.name) && cookie.value == source.token) || cookie.name == "__client" {
			if !names[cookie.name] {
				pairs = append(pairs, cookie)
				names[cookie.name] = true
			}
		}
	}
	if source.sessionName == "" && !names["__session"] {
		pairs = append(pairs, cookiePair{name: "__session", value: source.token})
	}
	var values []string
	for _, cookie := range pairs {
		values = append(values, cookie.name+"="+cookie.value)
	}
	return strings.Join(values, "; ")
}

func authCookie(name string) bool { return sessionCookie(name) || name == "__client" }

func (source credential) replaced(token string, received []*http.Cookie) []byte {
	var values []string
	names := make(map[string]bool)
	for _, cookie := range source.cookies {
		value := cookie.value
		if cookie.name == source.sessionName || (sessionCookie(cookie.name) && value == source.token) {
			value = token
		}
		if cookie.name == "__client" {
			for _, receivedCookie := range received {
				if receivedCookie.Name == "__client" {
					value = receivedCookie.Value
				}
			}
		}
		names[cookie.name] = true
		values = append(values, cookie.name+"="+value)
	}
	for _, cookie := range received {
		if authCookie(cookie.Name) && !names[cookie.Name] {
			values = append(values, cookie.Name+"="+cookie.Value)
			names[cookie.Name] = true
		}
	}
	return []byte(strings.Join(values, "; ") + "\n")
}

func (source authCredential) replaced(received []*http.Cookie, token string) []byte {
	rotations := make(map[string]string)
	for _, cookie := range received {
		if authCookie(cookie.Name) {
			rotations[cookie.Name] = cookie.Value
		}
	}
	var values []string
	for _, cookie := range source.cookies {
		value := cookie.value
		if sessionCookie(cookie.name) {
			value = token
		}
		if updated, ok := rotations[cookie.name]; ok {
			value = updated
			delete(rotations, cookie.name)
		}
		values = append(values, cookie.name+"="+value)
	}
	for _, cookie := range received {
		if updated, ok := rotations[cookie.Name]; ok {
			values = append(values, cookie.Name+"="+updated)
			delete(rotations, cookie.Name)
		}
	}
	return []byte(strings.Join(values, "; ") + "\n")
}

func validateRenewed(source credential, token string, now time.Time) (claims, error) {
	parsed, err := parseClaims(token)
	if err != nil || parsed.Subject != source.claims.Subject || parsed.SessionID != source.claims.SessionID || parsed.Issuer != source.claims.Issuer || parsed.Expiry <= source.claims.Expiry || parsed.Expiry <= now.Unix() || parsed.Issued < source.claims.Issued || parsed.Issued > now.Add(time.Minute).Unix() {
		return claims{}, &Error{Code: "invalid_response", cause: ErrInvalidResponse}
	}
	return parsed, nil
}
