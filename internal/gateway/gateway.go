package gateway

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"suno2api/internal/generate"
	"suno2api/internal/netconfig"
	"suno2api/internal/session"
)

type runtimeAccount struct {
	client   AccountClient
	cancel   context.CancelFunc
	ctx      context.Context
	inflight int
	cursor   float64
	started  bool
}
type Gateway struct {
	mu                             sync.Mutex
	state                          persistent
	accounts                       map[string]*runtimeAccount
	config                         Config
	dataDir, downloadDir, adminKey string
	adminSessions                  map[string]adminSession
	loginAttempts                  map[string]loginAttempt
	ctx                            context.Context
	cancel                         context.CancelFunc
	wait                           sync.WaitGroup
	mux                            *http.ServeMux
	running, closed                bool
	pollCursor                     int
	logs                           *logStore
	httpClient                     *http.Client
	proxyValue, proxySource        string
}

func New(config Config) (*Gateway, error) {
	if config.DataDir == "" {
		config.DataDir = "gateway-data"
	}
	root, err := filepath.Abs(config.DataDir)
	if err != nil {
		return nil, err
	}
	for _, path := range []string{root, filepath.Join(root, "accounts"), filepath.Join(root, "downloads"), filepath.Join(root, "keys"), filepath.Join(root, "jobs"), filepath.Join(root, "files"), filepath.Join(root, "logs")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	downloadDir := config.DownloadDir
	if downloadDir == "" {
		downloadDir = filepath.Join(root, "downloads")
	}
	downloadDir, err = filepath.Abs(downloadDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(downloadDir, 0700); err != nil {
		return nil, err
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 10 * time.Minute
	}
	if config.DefaultModel == "" {
		config.DefaultModel = "v6-mini"
	}
	defaultProtocol := strings.ToLower(strings.TrimSpace(config.DefaultProtocol))
	if defaultProtocol == "" {
		// Android is the primary protocol; web stays as the explicit fallback.
		defaultProtocol = "android"
	}
	if defaultProtocol != "android" && defaultProtocol != "web" {
		return nil, errors.New("gateway default protocol must be android or web")
	}
	config.DefaultProtocol = defaultProtocol
	apiStyle, err := normalizeAPIStyle(config.APIStyle)
	if err != nil {
		return nil, err
	}
	config.APIStyle = apiStyle
	if config.AccountClientFactory == nil {
		config.AccountClientFactory = defaultFactory
	}
	adminKey := strings.TrimSpace(config.AdminKey)
	keyPath := filepath.Join(root, "admin.key")
	if adminKey == "" {
		data, err := os.ReadFile(keyPath)
		if err == nil {
			adminKey = strings.TrimSpace(string(data))
		} else if os.IsNotExist(err) {
			adminKey, err = randomSecret()
			if err != nil {
				return nil, err
			}
			if err := atomicFile(keyPath, []byte(adminKey+"\n")); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if len(adminKey) < 32 {
		return nil, errors.New("gateway administrator key must contain at least 32 characters")
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Gateway{config: config, dataDir: root, downloadDir: downloadDir, adminKey: digest(adminKey), accounts: make(map[string]*runtimeAccount), adminSessions: make(map[string]adminSession), loginAttempts: make(map[string]loginAttempt), ctx: ctx, cancel: cancel, mux: http.NewServeMux(), logs: newLogStore(filepath.Join(root, "logs"))}
	service.state = normalizePersistent(persistent{})

	// The global proxy is operator-editable at runtime: an explicit CLI value
	// wins and is persisted, otherwise the saved settings.json value applies.
	proxyValue, err := service.initialProxyValue(config)
	if err != nil {
		cancel()
		return nil, err
	}
	client, proxySource, err := netconfig.NewHTTPClient(proxyValue)
	if err != nil {
		cancel()
		return nil, err
	}
	service.httpClient, service.proxyValue, service.proxySource = client, strings.TrimSpace(proxyValue), proxySource
	if err := service.loadStateLocked(); err != nil {
		cancel()
		return nil, err
	}
	for _, job := range service.state.Jobs {
		if job.State == "submitting" {
			job.State = "submission_unknown"
			job.Error = "服务重启时提交结果未确认，请查询原任务，不要自动重提"
		} else if job.State == "queued" {
			job.State = "interrupted"
			job.Error = "服务重启前尚未提交，请明确创建新任务"
		}
	}
	for id, record := range service.state.Accounts {
		record.Inflight = 0
		if record.DeletedAt != nil {
			continue
		}
		// The enabled switch only controls generation scheduling. Disabled accounts
		// keep a client so balance checks, renewal, and keepalive still work.
		if err := service.buildAccountLocked(id); err != nil {
			record.State = "configuration_error"
			record.LastError = "账号客户端配置不可用"
			service.logf("error", "account", "账号客户端初始化失败", map[string]any{"account": record.Name, "error": record.LastError})
		}
	}
	if err := service.persistLocked(); err != nil {
		cancel()
		return nil, err
	}
	service.routes()
	service.logf("info", "system", "网关已初始化", map[string]any{"data_dir": root, "proxy_source": service.proxySource})
	return service, nil
}

// initialProxyValue resolves the startup proxy: an explicit CLI/config flag
// wins and is persisted; otherwise the saved settings.json value applies; a
// fresh data directory persists the configured default so the management page
// always shows the effective value.
func (gateway *Gateway) initialProxyValue(config Config) (string, error) {
	path := gatewaySettingsPath(gateway.dataDir)
	value := strings.TrimSpace(config.Proxy)
	if config.ProxyExplicit {
		if _, err := validateProxyInput(value); err != nil {
			return "", err
		}
		if err := saveGatewaySettings(path, gatewaySettings{Proxy: value}); err != nil {
			return "", err
		}
		return value, nil
	}
	saved, found, err := loadGatewaySettings(path)
	if err != nil {
		return "", err
	}
	if found {
		if _, err := validateProxyInput(saved.Proxy); err != nil {
			return "", err
		}
		return saved.Proxy, nil
	}
	if _, err := validateProxyInput(value); err != nil {
		return "", err
	}
	if err := saveGatewaySettings(path, gatewaySettings{Proxy: value}); err != nil {
		return "", err
	}
	return value, nil
}

func (gateway *Gateway) accountNameLocked(id string) string {
	if record := gateway.state.Accounts[id]; record != nil {
		return record.Name
	}
	return id
}

func (gateway *Gateway) accountName(id string) string {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.accountNameLocked(id)
}

func (gateway *Gateway) logf(level, typ, message string, fields map[string]any) {
	if gateway.logs != nil {
		gateway.logs.write(level, typ, message, fields)
	}
}

// logCaptchaCheck records every generation preflight outcome, so the moment an
// account first gets asked for human verification stays traceable afterwards.
func (gateway *Gateway) logCaptchaCheck(accountID string, check generate.CaptchaCheck) {
	fields := map[string]any{"account_id": accountID, "account": gateway.accountName(accountID), "required": check.Required, "captcha_version": check.CaptchaVersion}
	if check.Required {
		gateway.logf("warn", "captcha", "Suno生成预检要求人机验证，提交前需要官方验证令牌", fields)
		return
	}
	gateway.logf("info", "captcha", "Suno生成预检未要求人机验证", fields)
}

// ProxySource reports the human-readable source of the effective proxy.
func (gateway *Gateway) ProxySource() string { return gateway.proxySource }

// ProxyValue reports the configured proxy value ("" means automatic).
func (gateway *Gateway) ProxyValue() string { return gateway.proxyValue }

func normalizeAPIStyle(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "both":
		return "both", nil
	case "suno":
		return "suno", nil
	case "openai":
		return "openai", nil
	}
	return "", errors.New("gateway API style must be suno, openai, or both")
}

func (gateway *Gateway) AdminKeyPath() string  { return filepath.Join(gateway.dataDir, "admin.key") }
func (gateway *Gateway) APIStyle() string      { return gateway.config.APIStyle }
func (gateway *Gateway) DataDir() string       { return gateway.dataDir }
func (gateway *Gateway) Handler() http.Handler { return gateway }
func (gateway *Gateway) Close() error {
	gateway.mu.Lock()
	if gateway.closed {
		gateway.mu.Unlock()
		return nil
	}
	gateway.closed = true
	gateway.cancel()
	for _, runtime := range gateway.accounts {
		runtime.cancel()
	}
	gateway.mu.Unlock()
	gateway.wait.Wait()
	gateway.logf("info", "system", "网关已停止", nil)
	gateway.logs.close()
	return nil
}
func (gateway *Gateway) Run(ctx context.Context) error {
	gateway.mu.Lock()
	if gateway.closed {
		gateway.mu.Unlock()
		return errors.New("gateway is closed")
	}
	gateway.running = true
	gateway.logf("info", "system", "网关开始运行，账号保活已启动", map[string]any{"accounts": len(gateway.accounts)})
	for id, runtime := range gateway.accounts {
		gateway.startAccountLocked(id, runtime)
	}
	gateway.mu.Unlock()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return gateway.Close()
		case <-gateway.ctx.Done():
			return gateway.Close()
		case <-ticker.C:
			gateway.pollJobs(ctx)
		}
	}
}

func (gateway *Gateway) buildAccountLocked(id string) error {
	record := gateway.state.Accounts[id]
	auth := gateway.authPath(id)
	if _, err := os.Stat(auth); os.IsNotExist(err) {
		auth = ""
	}
	client, err := gateway.config.AccountClientFactory(AccountClientConfig{ID: id, TokenFile: gateway.credentialPath(id), AuthCookieFile: auth, Protocol: record.Protocol, Model: record.Model, HTTPClient: gateway.httpClient, OnCaptchaCheck: func(check generate.CaptchaCheck) {
		gateway.logCaptchaCheck(id, check)
	}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(gateway.ctx)
	runtime := &runtimeAccount{client: client, ctx: ctx, cancel: cancel}
	if old := gateway.accounts[id]; old != nil {
		old.cancel()
		runtime.inflight = old.inflight
		runtime.cursor = old.cursor
	}
	gateway.accounts[id] = runtime
	status, statusError := client.Status(context.Background())
	record.Session = &status
	if statusError != nil {
		record.State = "attention"
		record.LastError = "登录凭据不可用，请更新账号Cookie"
	}
	if gateway.running {
		gateway.startAccountLocked(id, runtime)
	}
	return nil
}

func (gateway *Gateway) startAccountLocked(id string, runtime *runtimeAccount) {
	if runtime.started {
		return
	}
	runtime.started = true
	gateway.wait.Add(1)
	go func() {
		defer gateway.wait.Done()
		err := runtime.client.Run(runtime.ctx, func(event session.Event) {
			gateway.mu.Lock()
			defer gateway.mu.Unlock()
			if gateway.accounts[id] != runtime {
				return
			}
			record := gateway.state.Accounts[id]
			status := event.Status
			record.Session = &status
			if event.ErrorCode != "" {
				record.LastError = "保活状态：" + event.ErrorCode
			}
			switch event.State {
			case "refreshed":
				gateway.logf("info", "account", "会话续期成功", map[string]any{"account": record.Name})
			case "retrying":
				gateway.logf("warn", "account", "会话续期重试中", map[string]any{"account": record.Name, "error": event.ErrorCode})
			case "stopped":
				gateway.logf("warn", "account", "自动保活已停止", map[string]any{"account": record.Name, "error": event.ErrorCode})
			}
			_ = gateway.persistAccountLocked(id)
		})
		if err != nil && runtime.ctx.Err() == nil {
			gateway.mu.Lock()
			if gateway.accounts[id] == runtime {
				record := gateway.state.Accounts[id]
				record.LastError = "自动保活已停止，请检查会话状态"
				gateway.logf("warn", "account", "自动保活已停止，请检查会话状态", map[string]any{"account": record.Name, "error": err.Error()})
				_ = gateway.persistAccountLocked(id)
			}
			gateway.mu.Unlock()
		}
	}()
}

func (gateway *Gateway) AddAccount(ctx context.Context, input AccountInput) (AccountView, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return gateway.addAccountLocked(input)
}
func (gateway *Gateway) ImportAccount(ctx context.Context, input AccountInput) (AccountView, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	input, err := normalizeAccountCookies(input, "")
	if err != nil {
		return AccountView{}, err
	}
	identity := credentialIdentity(input.Cookie)
	if identity == "" {
		return AccountView{}, errors.New("导入的 Cookie 不包含有效 Suno 登录会话")
	}
	for id, record := range gateway.state.Accounts {
		if record.IdentityHash == identity {
			return gateway.updateAccountLocked(id, input)
		}
	}
	return gateway.addAccountLocked(input)
}

func parseGatewayCookies(value string) map[string]string {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "\ufeff"))
	if len(value) >= len("Cookie:") && strings.EqualFold(value[:len("Cookie:")], "Cookie:") {
		value = strings.TrimSpace(value[len("Cookie:"):])
	}
	request := &http.Request{Header: http.Header{"Cookie": []string{value}}}
	cookies := make(map[string]string)
	for _, cookie := range request.Cookies() {
		cookies[cookie.Name] = cookie.Value
	}
	return cookies
}

func sessionCookieValue(cookies map[string]string) (string, string) {
	if value := cookies["__session"]; value != "" {
		return "__session", value
	}
	names := make([]string, 0, len(cookies))
	for name, value := range cookies {
		if strings.HasPrefix(name, "__session_") && len(name) > len("__session_") && value != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0], cookies[names[0]]
	}
	return "", ""
}

func normalizeAccountCookies(input AccountInput, previousCookie string) (AccountInput, error) {
	input.Cookie = strings.TrimSpace(input.Cookie)
	input.AuthCookie = strings.TrimSpace(input.AuthCookie)
	if input.Cookie == "" {
		if input.AuthCookie == "" {
			return input, nil
		}
		authCookies := parseGatewayCookies(input.AuthCookie)
		name, token := sessionCookieValue(authCookies)
		if token != "" {
			identity := credentialIdentity(name + "=" + token)
			if identity == "" {
				return input, errors.New("认证Cookie中的登录会话无效")
			}
			device := authCookies["ajs_anonymous_id"]
			if device == "" && previousCookie != "" {
				device = parseGatewayCookies(previousCookie)["ajs_anonymous_id"]
			}
			if device == "" {
				return input, errors.New("认证Cookie未包含ajs_anonymous_id；请同时填写Suno网站Cookie")
			}
			input.Cookie = name + "=" + token + "; ajs_anonymous_id=" + device
		} else if previousCookie != "" {
			input.Cookie = previousCookie
		} else {
			return input, errors.New("认证Cookie未包含__session登录会话；请同时填写Suno网站Cookie")
		}
	}
	identity := credentialIdentity(input.Cookie)
	if identity == "" {
		return input, errors.New("请提供自己的有效Suno登录Cookie")
	}
	if input.AuthCookie != "" {
		authCookies := parseGatewayCookies(input.AuthCookie)
		if authIdentity := credentialIdentity(input.AuthCookie); authIdentity != "" && authIdentity != identity {
			return input, errors.New("网站Cookie与认证Cookie属于不同账号")
		}
		if parseGatewayCookies(input.Cookie)["ajs_anonymous_id"] == "" && authCookies["ajs_anonymous_id"] != "" {
			input.Cookie = strings.TrimSpace(input.Cookie) + "; ajs_anonymous_id=" + authCookies["ajs_anonymous_id"]
		}
	}
	return input, nil
}

func normalizeAccount(input AccountInput, model, defaultProtocol string) (AccountInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		return input, errors.New("账号名称须为1至100字符")
	}
	if len(input.Cookie) > 65536 || len(input.AuthCookie) > 65536 {
		return input, errors.New("Cookie 文件过大")
	}
	if input.Weight == 0 {
		input.Weight = 1
	}
	if input.MaxConcurrency == 0 {
		input.MaxConcurrency = 1
	}
	if input.Weight < 1 || input.Weight > 100 || input.MaxConcurrency < 1 || input.MaxConcurrency > 20 {
		return input, errors.New("权重须为1至100，并发须为1至20")
	}
	if input.Protocol == "" {
		input.Protocol = defaultProtocol
	}
	if input.Protocol != "web" && input.Protocol != "android" {
		return input, errors.New("协议须为android或web")
	}
	if input.Model == "" {
		input.Model = model
	}
	return input, nil
}

func (gateway *Gateway) addAccountLocked(input AccountInput) (AccountView, error) {
	if gateway.closed {
		return AccountView{}, errors.New("gateway is closed")
	}
	input, err := normalizeAccount(input, gateway.config.DefaultModel, gateway.config.DefaultProtocol)
	if err != nil {
		return AccountView{}, err
	}
	input, err = normalizeAccountCookies(input, "")
	if err != nil {
		return AccountView{}, err
	}
	if strings.TrimSpace(input.Cookie) == "" {
		return AccountView{}, errors.New("请填写账号登录 Cookie")
	}
	if credentialIdentity(input.Cookie) == "" {
		return AccountView{}, errors.New("请提供自己的有效Suno登录Cookie")
	}
	id, err := randomID()
	if err != nil {
		return AccountView{}, err
	}
	directory := filepath.Dir(gateway.credentialPath(id))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return AccountView{}, err
	}
	if err := atomicFile(gateway.credentialPath(id), []byte(input.Cookie)); err != nil {
		return AccountView{}, err
	}
	if input.AuthCookie != "" {
		if err := atomicFile(gateway.authPath(id), []byte(input.AuthCookie)); err != nil {
			return AccountView{}, err
		}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	now := time.Now().UTC()
	record := &accountRecord{AccountView: AccountView{ID: id, Name: input.Name, Enabled: enabled, Weight: input.Weight, MaxConcurrency: input.MaxConcurrency, Protocol: input.Protocol, Model: input.Model, State: "unchecked", CreatedAt: now, UpdatedAt: now}, IdentityHash: credentialIdentity(input.Cookie)}
	gateway.state.Accounts[id] = record
	if err := gateway.buildAccountLocked(id); err != nil {
		delete(gateway.state.Accounts, id)
		return AccountView{}, errors.New("账号客户端无法创建")
	}
	if err := gateway.persistAccountLocked(id); err != nil {
		return AccountView{}, err
	}
	return gateway.viewAccountLocked(id), nil
}

func (gateway *Gateway) updateAccountLocked(id string, input AccountInput) (AccountView, error) {
	record := gateway.state.Accounts[id]
	if record == nil {
		return AccountView{}, errors.New("账号不存在")
	}
	if input.Enabled != nil && strings.TrimSpace(input.Name) == "" && input.Cookie == "" && input.AuthCookie == "" && input.Weight == 0 && input.MaxConcurrency == 0 && input.Protocol == "" && input.Model == "" {
		// Enabled only decides whether the account may receive new generations.
		// Keep the runtime, keepalive, billing checks, and renewal untouched.
		if record.DeletedAt != nil {
			return AccountView{}, errors.New("账号已归档，请先恢复")
		}
		record.Enabled = *input.Enabled
		record.UpdatedAt = time.Now().UTC()
		if err := gateway.persistAccountLocked(id); err != nil {
			return AccountView{}, err
		}
		return gateway.viewAccountLocked(id), nil
	}
	var err error
	if input.Name == "" {
		input.Name = record.Name
	}
	if input.Protocol == "" {
		input.Protocol = record.Protocol
	}
	if input.Model == "" {
		input.Model = record.Model
	}
	if input.Weight == 0 {
		input.Weight = record.Weight
	}
	if input.MaxConcurrency == 0 {
		input.MaxConcurrency = record.MaxConcurrency
	}
	if input.Cookie == "" && input.AuthCookie != "" {
		previous, err := os.ReadFile(gateway.credentialPath(id))
		if err != nil {
			return AccountView{}, errors.New("现有账号Cookie无法读取")
		}
		input, err = normalizeAccountCookies(input, string(previous))
		if err != nil {
			return AccountView{}, err
		}
	} else {
		input, err = normalizeAccountCookies(input, "")
		if err != nil {
			return AccountView{}, err
		}
	}
	input, err = normalizeAccount(input, gateway.config.DefaultModel, gateway.config.DefaultProtocol)
	if err != nil {
		return AccountView{}, err
	}
	if input.Cookie != "" {
		identity := credentialIdentity(input.Cookie)
		if identity == "" || record.IdentityHash != "" && identity != record.IdentityHash {
			return AccountView{}, errors.New("账号凭据不能替换成其他账号，请新增账号")
		}
		if err := atomicFile(gateway.credentialPath(id), []byte(input.Cookie)); err != nil {
			return AccountView{}, err
		}
		record.IdentityHash = credentialIdentity(input.Cookie)
	}
	if input.AuthCookie != "" {
		if err := atomicFile(gateway.authPath(id), []byte(input.AuthCookie)); err != nil {
			return AccountView{}, err
		}
	}
	if old := gateway.accounts[id]; old != nil {
		old.cancel()
		delete(gateway.accounts, id)
	}
	record.Name = input.Name
	record.Protocol = input.Protocol
	record.Model = input.Model
	record.Weight = input.Weight
	record.MaxConcurrency = input.MaxConcurrency
	if input.Enabled != nil {
		record.Enabled = *input.Enabled
	}
	record.UpdatedAt = time.Now().UTC()
	record.State = "unchecked"
	record.LastError = ""
	record.Billing = nil
	record.Session = nil
	if record.DeletedAt == nil {
		if err := gateway.buildAccountLocked(id); err != nil {
			record.State = "configuration_error"
			record.LastError = "账号客户端不可用"
		}
	}
	if err := gateway.persistAccountLocked(id); err != nil {
		return AccountView{}, err
	}
	return gateway.viewAccountLocked(id), nil
}

func accountContext(parent context.Context, runtime *runtimeAccount, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	stop := context.AfterFunc(runtime.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (gateway *Gateway) viewAccountLocked(id string) AccountView {
	record := gateway.state.Accounts[id]
	view := record.AccountView
	return view
}
func (gateway *Gateway) accountViewsLocked() []AccountView {
	views := make([]AccountView, 0, len(gateway.state.Accounts))
	for id := range gateway.state.Accounts {
		views = append(views, gateway.viewAccountLocked(id))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.Before(views[j].CreatedAt) })
	return views
}
