package gateway

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionCookieName = "suno_gateway_admin"

func (gateway *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if request.URL.Path == "/health" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	isApplicationRoute := strings.HasPrefix(request.URL.Path, "/admin/api/") || strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/v1/") || request.URL.Path == "/download"
	if !safeOrigin(request, isApplicationRoute) {
		writeError(writer, http.StatusForbidden, "cross_origin", "禁止跨站请求")
		return
	}
	if request.URL.Path == "/admin/api/login" {
		gateway.login(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/admin/api/") {
		admin, ok := gateway.authenticatedAdmin(request)
		if !ok {
			writeError(writer, http.StatusUnauthorized, "admin_login_required", "请先登录管理界面")
			return
		}
		if request.Method != "GET" && request.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(request.Header.Get("X-CSRF-Token")), []byte(admin.CSRF)) != 1 {
			writeError(writer, http.StatusForbidden, "csrf_required", "管理操作缺少有效CSRF令牌")
			return
		}
		gateway.mux.ServeHTTP(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/v1/") || request.URL.Path == "/download" {
		if !gateway.authenticatedAPI(request) {
			writeError(writer, http.StatusUnauthorized, "api_key_required", "请提供网关 API Key")
			return
		}
		gateway.mux.ServeHTTP(writer, request)
		return
	}
	if gateway.config.WebHandler != nil {
		gateway.config.WebHandler.ServeHTTP(writer, request)
		return
	}
	http.NotFound(writer, request)
}

func safeOrigin(request *http.Request, protected bool) bool {
	host := request.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if !strings.EqualFold(host, "localhost") && net.ParseIP(host) == nil {
		return false
	}
	if protected && strings.EqualFold(request.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Host, request.Host) || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return false
		}
	}
	if strings.ContainsAny(request.Host, "/\\\r\n\t ") {
		return false
	}
	return true
}

func (gateway *Gateway) authenticatedAdmin(request *http.Request) (adminSession, bool) {
	cookie, err := request.Cookie(sessionCookieName)
	if err != nil {
		return adminSession{}, false
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	session, found := gateway.adminSessions[digest(cookie.Value)]
	if !found || !session.ExpiresAt.After(time.Now()) {
		delete(gateway.adminSessions, digest(cookie.Value))
		return adminSession{}, false
	}
	return session, true
}
func (gateway *Gateway) authenticatedAPI(request *http.Request) bool {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	hash := digest(strings.TrimSpace(strings.TrimPrefix(value, "Bearer ")))
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	for _, key := range gateway.state.Keys {
		if !key.Revoked && subtle.ConstantTimeCompare([]byte(hash), []byte(key.Hash)) == 1 {
			now := time.Now().UTC()
			persist := key.LastUsedAt == nil || now.Sub(*key.LastUsedAt) > time.Minute
			key.LastUsedAt = &now
			if persist {
				_ = gateway.persistKeyLocked(key.ID)
			}
			return true
		}
	}
	return false
}

func (gateway *Gateway) login(writer http.ResponseWriter, request *http.Request) {
	if request.Method != "POST" {
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "请使用POST登录")
		return
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	gateway.mu.Lock()
	if len(gateway.loginAttempts) > 1024 {
		for key, value := range gateway.loginAttempts {
			if time.Since(value.Since) > time.Minute {
				delete(gateway.loginAttempts, key)
			}
		}
		if len(gateway.loginAttempts) > 1024 {
			gateway.mu.Unlock()
			writeError(writer, 429, "login_rate_limited", "登录尝试过多，请稍后重试")
			return
		}
	}
	attempt := gateway.loginAttempts[host]
	if time.Since(attempt.Since) > time.Minute {
		attempt = loginAttempt{Since: time.Now()}
	}
	if attempt.Count >= 10 {
		gateway.mu.Unlock()
		writeError(writer, http.StatusTooManyRequests, "login_rate_limited", "登录尝试过多，请稍后重试")
		return
	}
	attempt.Count++
	gateway.loginAttempts[host] = attempt
	gateway.mu.Unlock()
	var body struct {
		Key string `json:"key"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(digest(body.Key)), []byte(gateway.adminKey)) != 1 {
		writeError(writer, http.StatusUnauthorized, "invalid_admin_key", "管理员密钥无效")
		return
	}
	token, err := randomSecret()
	if err != nil {
		writeError(writer, 500, "internal_error", "无法创建登录会话")
		return
	}
	csrf, err := randomSecret()
	if err != nil {
		writeError(writer, 500, "internal_error", "无法创建登录会话")
		return
	}
	gateway.mu.Lock()
	for key, session := range gateway.adminSessions {
		if !session.ExpiresAt.After(time.Now()) {
			delete(gateway.adminSessions, key)
		}
	}
	if len(gateway.adminSessions) >= 1024 {
		gateway.mu.Unlock()
		writeError(writer, 429, "session_limit", "登录会话数量达到上限，请稍后重试")
		return
	}
	gateway.adminSessions[digest(token)] = adminSession{CSRF: csrf, ExpiresAt: time.Now().Add(12 * time.Hour)}
	delete(gateway.loginAttempts, host)
	gateway.mu.Unlock()
	http.SetCookie(writer, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: request.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	writeJSON(writer, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": csrf})
}
