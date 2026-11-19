package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type renewal struct {
	token   string
	cookies []*http.Cookie
}

func (manager *Manager) requestRefresh(ctx context.Context, source credential) (renewal, error) {
	endpoint := manager.baseURL.ResolveReference(&url.URL{Path: "v1/client/sessions/" + source.claims.SessionID + "/tokens"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(""))
	if err != nil {
		return renewal{}, &Error{Code: "temporary", cause: ErrTemporary}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", "https://suno.com")
	request.Header.Set("Cookie", source.requestCookies())
	response, err := manager.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return renewal{}, ctx.Err()
		}
		return renewal{}, &Error{Code: "temporary", cause: ErrTemporary}
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return renewal{}, &Error{Code: "authentication_required", StatusCode: response.StatusCode, cause: ErrAuthentication}
	case response.StatusCode == http.StatusForbidden:
		body, _ := boundedBody(response.Body)
		if strings.EqualFold(response.Header.Get("CF-Mitigated"), "challenge") || challengeBody(body) {
			return renewal{}, &Error{Code: "verification_required", StatusCode: response.StatusCode, cause: ErrChallenge}
		}
		var payload struct {
			Code   string `json:"code"`
			Errors []struct {
				Code string `json:"code"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(body, &payload)
		codes := []string{payload.Code}
		for _, failure := range payload.Errors {
			codes = append(codes, failure.Code)
		}
		for _, code := range codes {
			switch code {
			case "session_not_found", "session_revoked", "session_expired", "client_not_found", "client_invalid", "authentication_invalid", "invalid_authentication":
				return renewal{}, &Error{Code: "authentication_required", StatusCode: response.StatusCode, cause: ErrAuthentication}
			}
		}
		return renewal{}, &Error{Code: "http_403_unclassified", StatusCode: response.StatusCode, cause: ErrRefreshRejected}
	case response.StatusCode == http.StatusTooManyRequests:
		return renewal{}, &Error{Code: "rate_limited", StatusCode: response.StatusCode, RetryAfter: retryAfter(response.Header.Get("Retry-After")), cause: ErrRateLimited}
	case response.StatusCode >= 500:
		return renewal{}, &Error{Code: "temporary", StatusCode: response.StatusCode, RetryAfter: retryAfter(response.Header.Get("Retry-After")), cause: ErrTemporary}
	case response.StatusCode != http.StatusOK:
		return renewal{}, &Error{Code: "invalid_response", StatusCode: response.StatusCode, cause: ErrInvalidResponse}
	}
	data, err := boundedBody(response.Body)
	if err != nil {
		return renewal{}, err
	}
	var payload struct {
		JWT string `json:"jwt"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.JWT == "" {
		return renewal{}, &Error{Code: "invalid_response", cause: ErrInvalidResponse}
	}
	if _, err := validateRenewed(source, payload.JWT, time.Now()); err != nil {
		return renewal{}, err
	}
	cookies, err := manager.acceptSessionCookies(response, payload.JWT)
	if err != nil {
		return renewal{}, err
	}
	return renewal{token: payload.JWT, cookies: cookies}, nil
}

func (manager *Manager) acceptSessionCookies(response *http.Response, token string) ([]*http.Cookie, error) {
	var accepted []*http.Cookie
	host := strings.ToLower(manager.baseURL.Hostname())
	for _, cookie := range response.Cookies() {
		if !authCookie(cookie.Name) {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
		domainValid := domain == "" || domain == host || (domain == "suno.com" && strings.HasSuffix(host, ".suno.com"))
		pathValid := cookie.Path == "/"
		secureValid := cookie.Secure || manager.baseURL.Scheme == "http"
		if !domainValid || !pathValid || !secureValid || cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(time.Now())) || cookie.Value == "" || (sessionCookie(cookie.Name) && cookie.Value != token) {
			return nil, &Error{Code: "invalid_response", cause: ErrInvalidResponse}
		}
		accepted = append(accepted, cookie)
	}
	hasSession := false
	for _, cookie := range accepted {
		if sessionCookie(cookie.Name) {
			hasSession = true
		}
	}
	if !hasSession {
		return nil, &Error{Code: "invalid_response", cause: ErrInvalidResponse}
	}
	return accepted, nil
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 && seconds <= 86400 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		delay := time.Until(deadline)
		if delay > 0 && delay <= 24*time.Hour {
			return delay
		}
	}
	return 0
}

// challengeBody reports whether an error body carries common Cloudflare
// challenge-page markers. Observed challenge responses do not always include
// the CF-Mitigated header, so the body is the remaining signal; hard block
// pages without these markers intentionally stay unclassified.
func challengeBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lowered := strings.ToLower(string(body))
	for _, marker := range []string{"just a moment", "cf-chl", "challenge-platform", "__cf_chl", "checking your browser", "enable javascript and cookies"} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
