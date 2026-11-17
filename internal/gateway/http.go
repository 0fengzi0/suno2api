package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	"suno2api/internal/generate"
	"suno2api/internal/session"
)

func (gateway *Gateway) routes() {
	gateway.mux.HandleFunc("GET /admin/api/session", gateway.adminSession)
	gateway.mux.HandleFunc("POST /admin/api/logout", gateway.logout)
	gateway.mux.HandleFunc("GET /admin/api/overview", gateway.overview)
	gateway.mux.HandleFunc("GET /admin/api/accounts", gateway.listAccounts)
	gateway.mux.HandleFunc("POST /admin/api/accounts", gateway.addAccountHTTP)
	gateway.mux.HandleFunc("PATCH /admin/api/accounts/{id}", gateway.updateAccountHTTP)
	gateway.mux.HandleFunc("DELETE /admin/api/accounts/{id}", gateway.deleteAccountHTTP)
	gateway.mux.HandleFunc("POST /admin/api/accounts/{id}/restore", gateway.restoreAccountHTTP)
	gateway.mux.HandleFunc("POST /admin/api/accounts/{id}/check", gateway.checkAccountHTTP)
	gateway.mux.HandleFunc("POST /admin/api/accounts/{id}/refresh", gateway.refreshAccountHTTP)
	gateway.mux.HandleFunc("GET /admin/api/keys", gateway.listKeys)
	gateway.mux.HandleFunc("POST /admin/api/keys", gateway.createKey)
	gateway.mux.HandleFunc("DELETE /admin/api/keys/{id}", gateway.revokeKey)
	gateway.mux.HandleFunc("GET /admin/api/jobs", gateway.listJobs)
	gateway.mux.HandleFunc("GET /admin/api/jobs/{id}", gateway.getJobHTTP)
	gateway.mux.HandleFunc("POST /admin/api/jobs", gateway.generateHTTP)
	gateway.mux.HandleFunc("POST /admin/api/downloads", gateway.downloadJSON)
	gateway.mux.HandleFunc("GET /admin/api/downloads/{id}/audio", gateway.fileHTTP)
	gateway.mux.HandleFunc("POST /api/generate", gateway.generateHTTP)
	gateway.mux.HandleFunc("GET /api/jobs", gateway.listJobs)
	gateway.mux.HandleFunc("GET /api/jobs/{id}", gateway.getJobHTTP)
	gateway.mux.HandleFunc("GET /api/songs", gateway.songsHTTP)
	gateway.mux.HandleFunc("GET /download", gateway.downloadFileHTTP)
	gateway.mux.HandleFunc("POST /api/download", gateway.downloadJSON)
	gateway.mux.HandleFunc("GET /api/downloads/{id}/audio", gateway.fileHTTP)
}

func (gateway *Gateway) adminSession(writer http.ResponseWriter, request *http.Request) {
	session, _ := gateway.authenticatedAdmin(request)
	writeJSON(writer, 200, map[string]any{"authenticated": true, "csrf_token": session.CSRF})
}
func (gateway *Gateway) logout(writer http.ResponseWriter, request *http.Request) {
	cookie, err := request.Cookie(sessionCookieName)
	if err == nil {
		gateway.mu.Lock()
		delete(gateway.adminSessions, digest(cookie.Value))
		gateway.mu.Unlock()
	}
	http.SetCookie(writer, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(writer, 200, map[string]bool{"ok": true})
}

func (gateway *Gateway) overview(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	enabled, inflight := 0, 0
	for id, account := range gateway.state.Accounts {
		if account.Enabled && account.DeletedAt == nil {
			enabled++
		}
		if runtime := gateway.accounts[id]; runtime != nil {
			inflight += runtime.inflight
		}
	}
	writeJSON(writer, 200, map[string]any{"accounts": gateway.accountViewsLocked(), "jobs": gateway.jobViewsLocked(), "keys": gateway.keyViewsLocked(), "stats": map[string]int{"accounts": len(gateway.state.Accounts), "enabled_accounts": enabled, "jobs": len(gateway.state.Jobs), "inflight": inflight}})
}
func (gateway *Gateway) listAccounts(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	writeJSON(writer, 200, map[string]any{"accounts": gateway.accountViewsLocked()})
}
func (gateway *Gateway) addAccountHTTP(writer http.ResponseWriter, request *http.Request) {
	var input AccountInput
	if !decodeJSON(writer, request, &input) {
		return
	}
	view, err := gateway.AddAccount(request.Context(), input)
	if err != nil {
		writeError(writer, 400, "invalid_account", safeInputError(err))
		return
	}
	writeJSON(writer, 201, map[string]any{"account": view})
}
func (gateway *Gateway) updateAccountHTTP(writer http.ResponseWriter, request *http.Request) {
	var input AccountInput
	if !decodeJSON(writer, request, &input) {
		return
	}
	gateway.mu.Lock()
	view, err := gateway.updateAccountLocked(request.PathValue("id"), input)
	gateway.mu.Unlock()
	if err != nil {
		writeError(writer, 400, "invalid_account", safeInputError(err))
		return
	}
	writeJSON(writer, 200, map[string]any{"account": view})
}

func (gateway *Gateway) deleteAccountHTTP(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	id := request.PathValue("id")
	record := gateway.state.Accounts[id]
	if record == nil {
		writeError(writer, 404, "not_found", "账号不存在")
		return
	}
	now := time.Now().UTC()
	record.Enabled = false
	record.DeletedAt = &now
	record.State = "archived"
	record.UpdatedAt = now
	if runtime := gateway.accounts[id]; runtime != nil {
		runtime.cancel()
		delete(gateway.accounts, id)
	}
	if gateway.persistLocked() != nil {
		writeError(writer, 500, "storage_error", "账号归档保存失败")
		return
	}
	writeJSON(writer, 200, map[string]any{"account": gateway.viewAccountLocked(id)})
}
func (gateway *Gateway) restoreAccountHTTP(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	record := gateway.state.Accounts[request.PathValue("id")]
	if record == nil {
		writeError(writer, 404, "not_found", "账号不存在")
		return
	}
	record.DeletedAt = nil
	record.Enabled = false
	record.State = "disabled"
	record.UpdatedAt = time.Now().UTC()
	if gateway.persistLocked() != nil {
		writeError(writer, 500, "storage_error", "恢复保存失败")
		return
	}
	writeJSON(writer, 200, map[string]any{"account": gateway.viewAccountLocked(record.ID)})
}

func (gateway *Gateway) accountForOperation(id string) (*runtimeAccount, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	record := gateway.state.Accounts[id]
	runtime := gateway.accounts[id]
	if record == nil || record.DeletedAt != nil {
		return nil, errors.New("account not found")
	}
	if runtime == nil {
		return nil, errors.New("account disabled")
	}
	return runtime, nil
}
func (gateway *Gateway) checkAccountHTTP(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	runtime, err := gateway.accountForOperation(id)
	if err != nil {
		writeError(writer, 409, "account_unavailable", "请先启用账号")
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	status, statusErr := runtime.client.Status(ctx)
	billing, billingErr := runtime.client.Billing(ctx)
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	record := gateway.state.Accounts[id]
	if record == nil || gateway.accounts[id] != runtime {
		writeError(writer, 409, "account_changed", "账号已更新，请重试检查")
		return
	}
	record.Session = &status
	record.UpdatedAt = time.Now().UTC()
	if statusErr != nil || billingErr != nil {
		record.State = "attention"
		record.LastError = "会话或余额检查失败，请更新凭据后重试"
	} else {
		record.State = "ready"
		record.LastError = ""
		record.Billing = &BillingView{Credits: billing.Credits, TotalCreditsLeft: billing.TotalCreditsLeft, Models: billing.Models}
	}
	_ = gateway.persistLocked()
	writeJSON(writer, 200, map[string]any{"account": gateway.viewAccountLocked(id)})
}
func (gateway *Gateway) refreshAccountHTTP(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	runtime, err := gateway.accountForOperation(id)
	if err != nil {
		writeError(writer, 409, "account_unavailable", "请先启用账号")
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	err = runtime.client.Refresh(ctx)
	status, _ := runtime.client.Status(ctx)
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	record := gateway.state.Accounts[id]
	if record == nil || gateway.accounts[id] != runtime {
		writeError(writer, 409, "account_changed", "账号已更新")
		return
	}
	record.Session = &status
	if err != nil {
		record.LastError = "续期未完成，请检查认证 Cookie 或浏览器验证"
	} else {
		record.LastError = ""
		record.State = "ready"
	}
	_ = gateway.persistLocked()
	if err != nil {
		writeJSON(writer, 502, map[string]any{"error": record.LastError, "code": "refresh_failed", "account": gateway.viewAccountLocked(id)})
		return
	}
	writeJSON(writer, 200, map[string]any{"account": gateway.viewAccountLocked(id)})
}

func (gateway *Gateway) keyViewsLocked() []APIKeyView {
	views := make([]APIKeyView, 0, len(gateway.state.Keys))
	for _, record := range gateway.state.Keys {
		views = append(views, record.APIKeyView)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.After(views[j].CreatedAt) })
	return views
}
func (gateway *Gateway) listKeys(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	writeJSON(writer, 200, map[string]any{"keys": gateway.keyViewsLocked()})
}
func (gateway *Gateway) createKey(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	body.Label = strings.TrimSpace(body.Label)
	if body.Label == "" || len(body.Label) > 100 {
		writeError(writer, 400, "invalid_label", "请填写有效密钥名称")
		return
	}
	secret, err := randomSecret()
	if err != nil {
		writeError(writer, 500, "internal_error", "密钥创建失败")
		return
	}
	secret = "sgw_" + secret
	id, err := randomID()
	if err != nil {
		writeError(writer, 500, "internal_error", "密钥创建失败")
		return
	}
	record := &keyRecord{APIKeyView: APIKeyView{ID: id, Label: body.Label, Prefix: secret[:12], CreatedAt: time.Now().UTC()}, Hash: digest(secret)}
	gateway.mu.Lock()
	gateway.state.Keys[id] = record
	err = gateway.persistLocked()
	gateway.mu.Unlock()
	if err != nil {
		writeError(writer, 500, "storage_error", "密钥保存失败")
		return
	}
	writeJSON(writer, 201, map[string]any{"id": id, "label": body.Label, "prefix": record.Prefix, "key": secret})
}
func (gateway *Gateway) revokeKey(writer http.ResponseWriter, request *http.Request) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	key := gateway.state.Keys[request.PathValue("id")]
	if key == nil {
		writeError(writer, 404, "not_found", "密钥不存在")
		return
	}
	key.Revoked = true
	if gateway.persistLocked() != nil {
		writeError(writer, 500, "storage_error", "密钥已在当前进程停用，但持久化失败，请修复存储后重试")
		return
	}
	writeJSON(writer, 200, map[string]bool{"ok": true})
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, 415, "json_required", "请使用application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 256<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil {
		writeError(writer, 400, "invalid_json", "JSON无效或超过256KiB")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(writer, 400, "invalid_json", "只接受一个JSON对象")
		return false
	}
	return true
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"error": message, "code": code})
}
func safeInputError(err error) string {
	for _, value := range []string{"账号名称须为1至100字符", "Cookie 文件过大", "权重须为1至100，并发须为1至20", "协议须为web或android", "请填写账号登录 Cookie", "账号不存在", "请提供自己的有效Suno登录Cookie", "账号凭据不能替换成其他账号，请新增账号"} {
		if err.Error() == value {
			return value
		}
	}
	return "账号保存失败，请检查本地存储和账号配置"
}
func operationError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 504, "upstream_timeout", "上游响应未确认，请检查任务，不要自动重提"
	case errors.Is(err, generate.ErrCaptchaRequired):
		return 403, "verification_required", "Suno要求在官方页面完成人机验证"
	case errors.Is(err, generate.ErrInsufficientCredits):
		return 402, "insufficient_credits", "账号额度不足"
	case errors.Is(err, generate.ErrRateLimited), errors.Is(err, session.ErrRateLimited):
		return 429, "rate_limited", "上游请求限速，请稍后重试"
	case errors.Is(err, session.ErrChallenge):
		return 403, "verification_required", "认证服务要求浏览器验证，请在官方会话中完成"
	case errors.Is(err, session.ErrRefreshRejected):
		return 403, "refresh_blocked", "认证服务拒绝续期，原因尚未确认"
	case errors.Is(err, generate.ErrAuthentication), errors.Is(err, session.ErrAuthentication):
		return 401, "authentication_required", "账号登录已过期，请更新凭据"
	default:
		return 502, "upstream_error", "上游操作失败，请检查账号状态"
	}
}
