package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"suno2api/internal/download"
	"suno2api/internal/generate"
	"suno2api/internal/session"
)

const testAdminKey = "test-admin-key-with-at-least-32-characters"
const testClip = "12345678-1234-4567-8901-123456789012"

type fakeClient struct {
	mu          sync.Mutex
	config      AccountClientConfig
	calls, gets int
	response    generate.Response
	failure     error
	block       chan struct{}
	runStarted  chan struct{}
	runStopped  chan struct{}
	lastIDs     []string
	lastRequest generate.Request
	getBlock    bool
	getStarted  chan struct{}
}

func (client *fakeClient) Generate(ctx context.Context, input generate.Request) (generate.Response, error) {
	client.mu.Lock()
	client.calls++
	client.lastRequest = input
	response, err := client.response, client.failure
	block := client.block
	client.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return generate.Response{}, ctx.Err()
		case <-block:
		}
	}
	return response, err
}
func (client *fakeClient) Get(ctx context.Context, ids []string) ([]generate.Clip, error) {
	client.mu.Lock()
	client.gets++
	client.lastIDs = append([]string{}, ids...)
	block, started := client.getBlock, client.getStarted
	client.mu.Unlock()
	if block {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []generate.Clip{{ID: ids[0], Status: "complete", ModelName: "chirp-goose", AudioURL: "https://signed.invalid/private?secret=do-not-return"}}, nil
}

func TestSchedulerUsesWeightsAndNeverExceedsPerAccountLimits(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	two := fixture.add(t, "two")
	fixture.gateway.mu.Lock()
	fixture.gateway.state.Accounts[one.ID].Weight = 2
	fixture.gateway.state.Accounts[two.ID].Weight = 1
	fixture.gateway.mu.Unlock()
	counts := map[string]int{}
	for index := 0; index < 6; index++ {
		fixture.gateway.mu.Lock()
		id, runtime, err := fixture.gateway.pickAccountLocked("", "v6-mini")
		fixture.gateway.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		counts[id]++
		fixture.gateway.releaseAccount(id, runtime)
	}
	if counts[one.ID] != 4 || counts[two.ID] != 2 {
		t.Fatalf("weighted scheduling mismatch: %#v", counts)
	}
}

func TestMixedModelsJudgeOnlyRequestedClips(t *testing.T) {
	job := &JobView{RequestedModel: "chirp-goose", RequestedIDs: []string{"wanted"}, Clips: []ClipView{{ID: "other", Status: "failed", Model: "chirp-hawk"}, {ID: "wanted", Status: "complete", Model: "chirp-goose", MediaReady: true}}}
	if jobState(job) != "complete" {
		t.Fatal("an unrelated model failed the requested v6-mini result")
	}
	job.RequestedIDs = nil
	job.Clips = job.Clips[:1]
	if jobState(job) != "no_requested_model" {
		t.Fatal("a known unrelated model falsely satisfied the requested model")
	}
}

func TestDisableOnlyStopsNewGenerationsWhileMaintenanceContinues(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	client := fixture.clients[one.ID]
	client.getBlock = true
	client.getStarted = make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		runtime, err := fixture.gateway.accountForOperation(one.ID)
		if err != nil {
			finished <- err
			return
		}
		ctx, cancel := accountContext(context.Background(), runtime, time.Second)
		defer cancel()
		_, err = runtime.client.Get(ctx, []string{testClip})
		finished <- err
	}()
	select {
	case <-client.getStarted:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	disabled := false
	fixture.gateway.mu.Lock()
	_, err := fixture.gateway.updateAccountLocked(one.ID, AccountInput{Enabled: &disabled})
	_, _, pickErr := fixture.gateway.pickAccountLocked("", "v6-mini")
	runtime := fixture.gateway.accounts[one.ID]
	fixture.gateway.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pickErr == nil {
		t.Fatal("disabled account was still selected for generation")
	}
	if runtime == nil {
		t.Fatal("disable removed the maintenance runtime and stopped renewal")
	}
	select {
	case <-finished:
		t.Fatal("disable canceled an in-flight query instead of only new generations")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight query did not finish")
	}
	fixture.gateway.mu.Lock()
	fixture.gateway.state.Jobs["interrupted-submit"] = &JobView{ID: "interrupted-submit", AccountID: one.ID, State: "submitting", CreatedAt: time.Now()}
	fixture.gateway.persistLocked()
	fixture.gateway.mu.Unlock()
	fixture.gateway.Close()
	restarted, err := New(Config{DataDir: fixture.gateway.dataDir, AdminKey: testAdminKey, AccountClientFactory: fixture.gateway.config.AccountClientFactory})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.state.Jobs["interrupted-submit"].State != "submission_unknown" {
		t.Fatal("restart did not preserve ambiguous submission")
	}
	if restarted.accounts[one.ID] == nil {
		t.Fatal("disabled account did not keep its maintenance runtime after restart")
	}
	for _, backend := range fixture.clients {
		if backend.calls != 0 {
			t.Fatal("restart resubmitted a generation")
		}
	}
}
func (client *fakeClient) Billing(ctx context.Context) (generate.BillingInfo, error) {
	return generate.BillingInfo{Credits: 0, TotalCreditsLeft: 20, Models: []generate.Model{{ExternalKey: "chirp-goose", CanUse: true}}}, nil
}
func (client *fakeClient) Download(ctx context.Context, address, directory string) (download.Result, error) {
	path := filepath.Join(directory, "local-song.mp3")
	if err := os.WriteFile(path, []byte("ID3test-audio"), 0600); err != nil {
		return download.Result{}, err
	}
	return download.Result{Path: path, Bytes: 12, SHA256: "fake-sha256", Track: download.Track{ID: testClip, Title: "Local song", AudioURL: "https://signed.invalid/private?secret=private"}}, nil
}
func (client *fakeClient) Status(ctx context.Context) (session.Status, error) {
	return session.Status{State: "active", ExpiresAt: time.Now().Add(time.Hour), CanRefresh: true}, nil
}
func (client *fakeClient) Refresh(ctx context.Context) error { return nil }
func (client *fakeClient) Run(ctx context.Context, onEvent func(session.Event)) error {
	if client.runStarted != nil {
		close(client.runStarted)
	}
	<-ctx.Done()
	if client.runStopped != nil {
		close(client.runStopped)
	}
	return ctx.Err()
}

type fixture struct {
	gateway *Gateway
	clients map[string]*fakeClient
	mu      sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fixture := &fixture{clients: map[string]*fakeClient{}}
	factory := func(config AccountClientConfig) (AccountClient, error) {
		client := &fakeClient{config: config, response: generate.Response{ID: "upstream-request", RequestedModel: "chirp-goose", Clips: []generate.Clip{{ID: testClip, ModelName: "chirp-goose", Status: "submitted"}}}, runStarted: make(chan struct{}), runStopped: make(chan struct{})}
		fixture.mu.Lock()
		fixture.clients[config.ID] = client
		fixture.mu.Unlock()
		return client, nil
	}
	gateway, err := New(Config{DataDir: t.TempDir(), AdminKey: testAdminKey, AccountClientFactory: factory, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fixture.gateway = gateway
	t.Cleanup(func() { gateway.Close() })
	return fixture
}
func cookieFixture(subject, sid string) string {
	payload, _ := json.Marshal(map[string]any{"iss": "https://auth.suno.com", "sub": subject, "sid": sid, "iat": time.Now().Unix() - 100, "exp": time.Now().Unix() + 3600})
	return "__session=header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature; ajs_anonymous_id=current-device; private-marker=must-not-return"
}
func (fixture *fixture) add(t *testing.T, name string) AccountView {
	t.Helper()
	view, err := fixture.gateway.AddAccount(context.Background(), AccountInput{Name: name, Cookie: cookieFixture(name, "session-"+name), AuthCookie: "__client=private-auth"})
	if err != nil {
		t.Fatal(err)
	}
	return view
}
func request(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	request.Host = "localhost"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}
func adminLogin(t *testing.T, gateway *Gateway) (*http.Cookie, string) {
	t.Helper()
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request("POST", "/admin/api/login", `{"key":"`+testAdminKey+`"}`))
	if response.Code != 200 {
		t.Fatalf("login failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(response.Body.Bytes(), &body)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || body.CSRF == "" {
		t.Fatal("admin login Cookie and CSRF protections missing")
	}
	return cookies[0], body.CSRF
}
func adminRequest(gateway *Gateway, cookie *http.Cookie, csrf, method, path, body string) *httptest.ResponseRecorder {
	req := request(method, path, body)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, req)
	return response
}
func jobFrom(t *testing.T, response *httptest.ResponseRecorder) JobView {
	t.Helper()
	if response.Code != 202 && response.Code != 200 {
		t.Fatalf("job response failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Job JobView `json:"job"`
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Job.ID == "" {
		t.Fatal("job response missing ID")
	}
	return body.Job
}
func waitJob(t *testing.T, gateway *Gateway, id string) JobView {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gateway.mu.Lock()
		job := *gateway.state.Jobs[id]
		gateway.mu.Unlock()
		if job.State != "submitting" {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job submission did not settle")
	return JobView{}
}

func TestAdminAuthenticationCSRFAndSecretSafePersistence(t *testing.T) {
	fixture := newFixture(t)
	account := fixture.add(t, "one")
	gateway := fixture.gateway
	cookie, csrf := adminLogin(t, gateway)
	blocked := httptest.NewRecorder()
	req := request("POST", "/admin/api/keys", `{"label":"App"}`)
	req.AddCookie(cookie)
	gateway.ServeHTTP(blocked, req)
	if blocked.Code != 403 {
		t.Fatal("admin write without CSRF accepted")
	}
	created := adminRequest(gateway, cookie, csrf, "POST", "/admin/api/keys", `{"label":"App"}`)
	var key map[string]any
	json.Unmarshal(created.Body.Bytes(), &key)
	secret, _ := key["key"].(string)
	if created.Code != 201 || secret == "" {
		t.Fatal("API key not created")
	}
	list := adminRequest(gateway, cookie, csrf, "GET", "/admin/api/overview", "")
	metadata := managementMetadata(t, gateway.dataDir)
	for _, private := range []string{secret, "private-auth", "must-not-return", "current-device", "header."} {
		if strings.Contains(list.Body.String(), private) || strings.Contains(metadata, private) {
			t.Fatalf("management metadata exposed secret %q", private)
		}
	}
	public := httptest.NewRecorder()
	publicRequest := request("GET", "/api/jobs", "")
	publicRequest.Header.Set("Authorization", "Bearer "+secret)
	gateway.ServeHTTP(public, publicRequest)
	if public.Code != 200 {
		t.Fatal("new API key could not call gateway")
	}
	revoked := adminRequest(gateway, cookie, csrf, "DELETE", "/admin/api/keys/"+key["id"].(string), "")
	if revoked.Code != 200 {
		t.Fatal("key revoke failed")
	}
	public = httptest.NewRecorder()
	gateway.ServeHTTP(public, publicRequest)
	if public.Code != 401 {
		t.Fatal("revoked API key remained usable")
	}
	if _, err := os.Stat(gateway.credentialPath(account.ID)); err != nil {
		t.Fatal("managed credentials missing")
	}
	cross := request("POST", "/admin/api/login", `{"key":"`+testAdminKey+`"}`)
	cross.Header.Set("Origin", "https://unrelated.invalid")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, cross)
	if response.Code != 403 {
		t.Fatal("cross-site login accepted")
	}
}

func managementMetadata(t *testing.T, root string) string {
	t.Helper()
	var combined strings.Builder
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasSuffix(entry.Name(), ".cookie") || strings.HasSuffix(entry.Name(), ".refresh.lock") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			combined.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return combined.String()
}

func TestTopLevelStaticNavigationAllowsBrowserEntryButProtectsAPI(t *testing.T) {
	fixture := newFixture(t)
	fixture.gateway.config.WebHandler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, "gateway-ui") })
	navigation := request("GET", "/", "")
	navigation.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(response, navigation)
	if response.Code != 200 || response.Body.String() != "gateway-ui" {
		t.Fatal("browser top-level entry was rejected")
	}
	api := request("POST", "/admin/api/login", `{"key":"`+testAdminKey+`"}`)
	api.Header.Set("Sec-Fetch-Site", "cross-site")
	response = httptest.NewRecorder()
	fixture.gateway.ServeHTTP(response, api)
	if response.Code != 403 {
		t.Fatal("cross-site administrator API became accessible")
	}
}

func TestAccountsUseIsolatedCredentialsAndRejectDifferentSubjectUpdates(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	two := fixture.add(t, "two")
	first, second := fixture.clients[one.ID], fixture.clients[two.ID]
	if first.config.TokenFile == second.config.TokenFile || first.config.AuthCookieFile == second.config.AuthCookieFile {
		t.Fatal("accounts share credential paths")
	}
	oneContent, _ := os.ReadFile(first.config.TokenFile)
	twoContent, _ := os.ReadFile(second.config.TokenFile)
	if string(oneContent) == string(twoContent) {
		t.Fatal("accounts share actual credentials")
	}
	fixture.gateway.mu.Lock()
	_, err := fixture.gateway.updateAccountLocked(one.ID, AccountInput{Cookie: cookieFixture("two", "new-session")})
	fixture.gateway.mu.Unlock()
	if err == nil {
		t.Fatal("account ID was rebound to a different Suno identity")
	}
	fixture.gateway.mu.Lock()
	updated, err := fixture.gateway.updateAccountLocked(one.ID, AccountInput{Cookie: cookieFixture("one", "new-login-session")})
	fixture.gateway.mu.Unlock()
	if err != nil || updated.ID != one.ID {
		t.Fatalf("same account relogin rejected: %v", err)
	}
	imported, err := fixture.gateway.ImportAccount(context.Background(), AccountInput{Name: "one updated", Cookie: cookieFixture("one", "third-session")})
	if err != nil || imported.ID != one.ID {
		t.Fatal("explicit import duplicated the same account after relogin")
	}
}

func TestAccountCanBeAddedWithCompleteAuthenticationCookieOnly(t *testing.T) {
	fixture := newFixture(t)
	token := cookieFixture("auth-only", "same-session")
	cookies := "__client=auth-client; " + token
	account, err := fixture.gateway.AddAccount(context.Background(), AccountInput{
		Name: "auth only", AuthCookie: cookies, Enabled: boolPointer(false),
	})
	if err != nil {
		t.Fatalf("complete authentication Cookie was not accepted: %v", err)
	}
	stored, err := os.ReadFile(fixture.gateway.credentialPath(account.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "__session=") || !strings.Contains(string(stored), "ajs_anonymous_id=") {
		t.Fatal("the account API Cookie did not retain the session and device ID")
	}
	if storedAuth, err := os.ReadFile(fixture.gateway.authPath(account.ID)); err != nil || !strings.Contains(string(storedAuth), "__client=auth-client") {
		t.Fatal("the original authentication Cookie was not stored separately")
	}
}

func TestAdminAPIAddsAccountFromAuthenticationCookieOnly(t *testing.T) {
	fixture := newFixture(t)
	cookie, csrf := adminLogin(t, fixture.gateway)
	authCookie := "__client=auth-client; " + cookieFixture("auth-only-api", "session-from-auth-cookie")
	body, err := json.Marshal(AccountInput{
		Name: "auth-cookie account", AuthCookie: authCookie, Enabled: boolPointer(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(fixture.gateway, cookie, csrf, http.MethodPost, "/admin/api/accounts", string(body))
	if response.Code != http.StatusCreated {
		t.Fatalf("admin API rejected complete authentication Cookie: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "auth-client") || strings.Contains(response.Body.String(), "session-from-auth-cookie") || strings.Contains(response.Body.String(), "current-device") {
		t.Fatal("account response exposed a Cookie, session token, or device ID")
	}
	var result struct {
		Account AccountView `json:"account"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(fixture.gateway.credentialPath(result.Account.ID))
	if err != nil || credentialIdentity(string(stored)) != credentialIdentity(cookieFixture("auth-only-api", "session-from-auth-cookie")) || !strings.Contains(string(stored), "ajs_anonymous_id=current-device") {
		t.Fatal("admin API did not persist the extracted session and device ID")
	}
}

func TestAuthenticationCookieOnlyExplainsMissingSessionOrDevice(t *testing.T) {
	fixture := newFixture(t)
	for _, tc := range []struct{ cookie, want string }{
		{"__client=long-lived", "认证Cookie未包含__session登录会话；请同时填写Suno网站Cookie"},
		{"__client=long-lived; __session=" + strings.Split(cookieFixture("missing-device", "sid"), "ajs_anonymous_id=")[0], "认证Cookie未包含ajs_anonymous_id；请同时填写Suno网站Cookie"},
	} {
		_, err := fixture.gateway.AddAccount(context.Background(), AccountInput{Name: "incomplete", AuthCookie: tc.cookie, Enabled: boolPointer(false)})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("expected helpful missing Cookie message %q, got %v", tc.want, err)
		}
	}
}

func TestAuthenticationCookieOnlyCanRefreshCredentialsForExistingSameAccount(t *testing.T) {
	fixture := newFixture(t)
	account := fixture.add(t, "same account")
	updatedAuth := "__client=rotated-client; " + cookieFixture("same account", "rotated-session")
	fixture.gateway.mu.Lock()
	updated, err := fixture.gateway.updateAccountLocked(account.ID, AccountInput{Name: account.Name, AuthCookie: updatedAuth})
	fixture.gateway.mu.Unlock()
	if err != nil || updated.ID != account.ID {
		t.Fatalf("matching authentication Cookie update was rejected: %v", err)
	}
	stored, err := os.ReadFile(fixture.gateway.credentialPath(account.ID))
	if err != nil || !strings.Contains(string(stored), "ajs_anonymous_id=current-device") || credentialIdentity(string(stored)) != credentialIdentity(cookieFixture("same account", "rotated-session")) {
		t.Fatal("auth Cookie update did not preserve the existing device ID with the new session")
	}
	storedAuth, err := os.ReadFile(fixture.gateway.authPath(account.ID))
	if err != nil || !strings.Contains(string(storedAuth), "rotated-client") {
		t.Fatal("auth Cookie update was not persisted")
	}
}

func TestAuthenticationAndWebsiteCookiesMustMatchAccount(t *testing.T) {
	input := AccountInput{
		Name:       "mismatched cookies",
		Cookie:     cookieFixture("website-user", "website-session"),
		AuthCookie: "__client=client; " + cookieFixture("different-auth-user", "auth-session"),
	}
	_, err := normalizeAccountCookies(input, "")
	if err == nil || err.Error() != "网站Cookie与认证Cookie属于不同账号" {
		t.Fatalf("mismatched account cookies were not rejected clearly: %v", err)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestSingleSubmissionAffinityConcurrencyAndRestart(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	two := fixture.add(t, "two")
	gateway := fixture.gateway
	cookie, csrf := adminLogin(t, gateway)
	first := fixture.clients[one.ID]
	first.mu.Lock()
	first.block = make(chan struct{})
	first.mu.Unlock()
	body := fmt.Sprintf(`{"account_id":%q,"prompt":"piano","model":"v6-mini"}`, one.ID)
	job := jobFrom(t, adminRequest(gateway, cookie, csrf, "POST", "/admin/api/jobs", body))
	busy := adminRequest(gateway, cookie, csrf, "POST", "/admin/api/jobs", body)
	if busy.Code != 503 {
		t.Fatal("per-account concurrency cap ignored")
	}
	close(first.block)
	settled := waitJob(t, gateway, job.ID)
	if settled.AccountID != one.ID || settled.RequestID != "upstream-request" {
		t.Fatal("job lost original account affinity or upstream request")
	}
	queried := jobFrom(t, adminRequest(gateway, cookie, csrf, "GET", "/admin/api/jobs/"+job.ID, ""))
	if queried.State != "complete" {
		t.Fatal("job was not polled")
	}
	if first.gets != 1 || fixture.clients[two.ID].gets != 0 {
		t.Fatal("job query switched to a different account")
	}
	if strings.Contains(adminRequest(gateway, cookie, csrf, "GET", "/admin/api/jobs/"+job.ID, "").Body.String(), "signed.invalid") {
		t.Fatal("job response leaked signed media URL")
	}
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Config{DataDir: gateway.dataDir, AdminKey: testAdminKey, AccountClientFactory: gateway.config.AccountClientFactory})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.state.Jobs[job.ID].AccountID != one.ID || len(restarted.state.Jobs[job.ID].ClipIDs) == 0 {
		t.Fatal("job affinity did not survive restart")
	}
}

func TestAmbiguousSubmissionNeverRetriesAndAcceptedResponseIDsSurvive(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			fixture := newFixture(t)
			one := fixture.add(t, "one")
			two := fixture.add(t, "two")
			first := fixture.clients[one.ID]
			first.failure = errors.New("connection dropped with private-secret")
			if !accepted {
				first.response = generate.Response{}
			}
			cookie, csrf := adminLogin(t, fixture.gateway)
			job := jobFrom(t, adminRequest(fixture.gateway, cookie, csrf, "POST", "/admin/api/jobs", fmt.Sprintf(`{"prompt":"piano","account_id":%q}`, one.ID)))
			settled := waitJob(t, fixture.gateway, job.ID)
			if first.calls != 1 || fixture.clients[two.ID].calls != 0 {
				t.Fatal("generation retried or changed accounts")
			}
			if !accepted && settled.State != "submission_unknown" {
				t.Fatal("ambiguous network submission falsely failed")
			}
			if accepted && (settled.State != "submitted" || settled.RequestID == "" || len(settled.ClipIDs) == 0) {
				t.Fatal("accepted response IDs lost on error")
			}
			if strings.Contains(settled.Error, "private-secret") {
				t.Fatal("upstream error leaked credentials")
			}
		})
	}
}

func TestDisableArchiveRestoreCancelsAccountLifecycle(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	runtime := fixture.clients[one.ID]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.gateway.Run(ctx) }()
	select {
	case <-runtime.runStarted:
	case <-time.After(time.Second):
		t.Fatal("account keepalive did not start")
	}
	cookie, csrf := adminLogin(t, fixture.gateway)
	archived := adminRequest(fixture.gateway, cookie, csrf, "DELETE", "/admin/api/accounts/"+one.ID, "")
	if archived.Code != 200 {
		t.Fatal("archive failed")
	}
	select {
	case <-runtime.runStopped:
	case <-time.After(time.Second):
		t.Fatal("archived account keepalive was not cancelled")
	}
	restored := adminRequest(fixture.gateway, cookie, csrf, "POST", "/admin/api/accounts/"+one.ID+"/restore", `{}`)
	if restored.Code != 200 || fixture.gateway.state.Accounts[one.ID].DeletedAt != nil || fixture.gateway.state.Accounts[one.ID].Enabled {
		t.Fatal("archived account cannot be restored disabled")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway shutdown did not finish")
	}
}

func TestDownloadAssetRequiresAuthorizationAndDoesNotExposePaths(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	cookie, csrf := adminLogin(t, fixture.gateway)
	body := fmt.Sprintf(`{"url":"https://suno.com/song/%s","account_id":%q}`, testClip, one.ID)
	response := adminRequest(fixture.gateway, cookie, csrf, "POST", "/admin/api/downloads", body)
	if response.Code != 200 || strings.Contains(response.Body.String(), fixture.gateway.downloadDir) || strings.Contains(response.Body.String(), "signed.invalid") {
		t.Fatalf("unsafe file response: %s", response.Body.String())
	}
	var parsed struct {
		File FileView `json:"file"`
	}
	json.Unmarshal(response.Body.Bytes(), &parsed)
	anonymous := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(anonymous, request("GET", "/admin/api/downloads/"+parsed.File.ID+"/audio", ""))
	if anonymous.Code != 401 {
		t.Fatal("audio file accessible without authorization")
	}
	audio := adminRequest(fixture.gateway, cookie, csrf, "GET", "/admin/api/downloads/"+parsed.File.ID+"/audio", "")
	data, _ := io.ReadAll(audio.Result().Body)
	if audio.Code != 200 || string(data) != "ID3test-audio" {
		t.Fatal("managed file download failed")
	}
}

func apiKey(t *testing.T, gateway *Gateway) (string, string) {
	t.Helper()
	cookie, csrf := adminLogin(t, gateway)
	response := adminRequest(gateway, cookie, csrf, "POST", "/admin/api/keys", `{"label":"test app"}`)
	if response.Code != 201 {
		t.Fatalf("key creation failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Key == "" || body.ID == "" {
		t.Fatal("key response missing values")
	}
	return body.ID, body.Key
}

func publicRequest(method, path, body, key string) *http.Request {
	request := request(method, path, body)
	request.Header.Set("Authorization", "Bearer "+key)
	return request
}

func TestSunoStyleAPIUsesNativeFieldsAndReturnsClips(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	_, key := apiKey(t, fixture.gateway)
	response := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(response, publicRequest("POST", "/api/generate", `{"gpt_description_prompt":"calm piano at dawn","mv":"v6-mini","make_instrumental":true,"batch_size":1}`, key))
	if response.Code != 200 {
		t.Fatalf("suno generate failed: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		ID             string          `json:"id"`
		JobID          string          `json:"job_id"`
		Status         string          `json:"status"`
		RequestedModel string          `json:"requested_model"`
		Clips          []generate.Clip `json:"clips"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.JobID == "" || result.RequestedModel != "chirp-goose" || len(result.Clips) != 1 || result.Clips[0].ID != testClip {
		t.Fatalf("unexpected suno response: %s", response.Body.String())
	}
	client := fixture.clients[one.ID]
	if !client.lastRequest.Instrumental || client.lastRequest.Custom || client.lastRequest.Prompt != "calm piano at dawn" {
		t.Fatalf("suno request was translated incorrectly: %#v", client.lastRequest)
	}
	feed := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(feed, publicRequest("GET", "/api/feed?ids="+testClip, "", key))
	if feed.Code != 200 || !strings.Contains(feed.Body.String(), `"clips"`) || !strings.Contains(feed.Body.String(), testClip) {
		t.Fatalf("suno feed failed: %d %s", feed.Code, feed.Body.String())
	}
	billing := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(billing, publicRequest("GET", "/api/billing", "", key))
	if billing.Code != 200 || !strings.Contains(billing.Body.String(), `"total_credits_left":20`) {
		t.Fatalf("suno billing failed: %d %s", billing.Code, billing.Body.String())
	}
}

func TestSunoStyleCustomInstrumentalNeverSendsDescriptionAsLyrics(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	_, key := apiKey(t, fixture.gateway)
	response := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(response, publicRequest("POST", "/api/generate", `{"prompt":"a rainy morning","tags":"ambient piano","title":"Rain","make_instrumental":true,"metadata":{"create_mode":"custom"},"mv":"v6-mini"}`, key))
	if response.Code != 200 {
		t.Fatalf("custom instrumental failed: %d %s", response.Code, response.Body.String())
	}
	request := fixture.clients[one.ID].lastRequest
	if request.Prompt != "" || request.Lyrics != "" || request.Style != "ambient piano" || !request.Instrumental || !request.Custom {
		t.Fatalf("custom instrumental reused the description as lyrics: %#v", request)
	}
}

func TestOpenAIStyleAPIListsModelsAndReturnsGeneratedAudio(t *testing.T) {
	fixture := newFixture(t)
	_ = fixture.add(t, "one")
	_, key := apiKey(t, fixture.gateway)
	models := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(models, publicRequest("GET", "/v1/models", "", key))
	if models.Code != 200 || !strings.Contains(models.Body.String(), `"object":"list"`) || !strings.Contains(models.Body.String(), "v6-mini") {
		t.Fatalf("openai model list failed: %d %s", models.Code, models.Body.String())
	}
	response := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(response, publicRequest("POST", "/v1/audio/generations", `{"model":"v6-mini","prompt":"warm piano","n":1}`, key))
	if response.Code != 200 {
		t.Fatalf("openai generation failed: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Object != "audio.generation" || created.Status != "processing" || len(created.Data) != 1 || created.Data[0].ID != testClip {
		t.Fatalf("unexpected openai generation response: %s", response.Body.String())
	}
	query := httptest.NewRecorder()
	fixture.gateway.ServeHTTP(query, publicRequest("GET", "/v1/audio/generations/"+created.ID, "", key))
	if query.Code != 200 || !strings.Contains(query.Body.String(), `"status":"completed"`) || !strings.Contains(query.Body.String(), "signed.invalid") {
		t.Fatalf("openai generation query failed: %d %s", query.Code, query.Body.String())
	}
}

func TestAPIStyleGatesRouteFamilies(t *testing.T) {
	factory := func(config AccountClientConfig) (AccountClient, error) {
		return &fakeClient{config: config, runStarted: make(chan struct{}), runStopped: make(chan struct{})}, nil
	}
	for _, tc := range []struct {
		style   string
		method  string
		path    string
		body    string
		allowed bool
	}{
		{"suno", "GET", "/api/models", "", true},
		{"suno", "GET", "/v1/models", "", false},
		{"openai", "GET", "/v1/models", "", true},
		{"openai", "POST", "/api/generate", `{"prompt":"x"}`, false},
	} {
		gateway, err := New(Config{DataDir: t.TempDir(), AdminKey: testAdminKey, APIStyle: tc.style, AccountClientFactory: factory})
		if err != nil {
			t.Fatal(err)
		}
		secret := "style-gate-test-key-123456"
		gateway.mu.Lock()
		gateway.state.Keys["gate"] = &keyRecord{APIKeyView: APIKeyView{ID: "gate", Label: "gate", Prefix: secret[:8], CreatedAt: time.Now()}, Hash: digest(secret)}
		gateway.mu.Unlock()
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, publicRequest(tc.method, tc.path, tc.body, secret))
		gateway.Close()
		if tc.allowed && response.Code == 404 {
			t.Fatalf("style %s unexpectedly hid %s", tc.style, tc.path)
		}
		if !tc.allowed && response.Code != 404 {
			t.Fatalf("style %s did not hide %s: %d", tc.style, tc.path, response.Code)
		}
	}
}

func TestAccountProtocolDefaultsToAndroidWithWebFallback(t *testing.T) {
	fixture := newFixture(t)
	primary := fixture.add(t, "android-default")
	if primary.Protocol != "android" {
		t.Fatalf("new account did not default to android protocol: %s", primary.Protocol)
	}
	webInput := AccountInput{Name: "web-fallback", Cookie: cookieFixture("web-user", "session-web"), Protocol: "web"}
	fallback, err := fixture.gateway.AddAccount(context.Background(), webInput)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Protocol != "web" {
		t.Fatalf("explicit web fallback rejected: %s", fallback.Protocol)
	}
	if fixture.clients[fallback.ID].config.Protocol != "web" || fixture.clients[primary.ID].config.Protocol != "android" {
		t.Fatal("account client did not receive the selected protocol")
	}
	gateway, err := New(Config{DataDir: t.TempDir(), AdminKey: testAdminKey, DefaultProtocol: "web", AccountClientFactory: fixture.gateway.config.AccountClientFactory})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	if gateway.config.DefaultProtocol != "web" {
		t.Fatal("explicit web default protocol was not honored")
	}
	if _, err := New(Config{DataDir: t.TempDir(), AdminKey: testAdminKey, DefaultProtocol: "invalid"}); err == nil {
		t.Fatal("invalid default protocol accepted")
	}
}

func TestStorageUsesPerRecordFilesAndMigratesLegacyMetadata(t *testing.T) {
	fixture := newFixture(t)
	one := fixture.add(t, "one")
	keyID, _ := apiKey(t, fixture.gateway)
	if _, err := os.Stat(filepath.Join(fixture.gateway.dataDir, "metadata.json")); !os.IsNotExist(err) {
		t.Fatal("new gateway still writes a monolithic metadata.json")
	}
	for _, path := range []string{fixture.gateway.accountRecordPath(one.ID), fixture.gateway.credentialPath(one.ID), fixture.gateway.keyPath(keyID)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("per-record file missing %s: %v", path, err)
		}
	}
	var stored accountRecord
	if err := readJSON(fixture.gateway.accountRecordPath(one.ID), &stored); err != nil || stored.Name != "one" || stored.IdentityHash == "" {
		t.Fatalf("account record did not round-trip: %v", err)
	}

	root := t.TempDir()
	accountID := "legacy-account"
	if err := os.MkdirAll(filepath.Join(root, "accounts", accountID), 0700); err != nil {
		t.Fatal(err)
	}
	cookie := cookieFixture("legacy", "legacy-session")
	if err := os.WriteFile(filepath.Join(root, "accounts", accountID, "session.cookie"), []byte(cookie), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := persistent{Version: 1, Accounts: map[string]*accountRecord{accountID: {AccountView: AccountView{ID: accountID, Name: "Legacy", Enabled: false, Weight: 1, MaxConcurrency: 1, Protocol: "web", Model: "v6-mini", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, IdentityHash: credentialIdentity(cookie)}}, Jobs: map[string]*JobView{}, Keys: map[string]*keyRecord{}, Files: map[string]*fileRecord{}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	factory := func(config AccountClientConfig) (AccountClient, error) {
		return &fakeClient{config: config, runStarted: make(chan struct{}), runStopped: make(chan struct{})}, nil
	}
	migrated, err := New(Config{DataDir: root, AdminKey: testAdminKey, AccountClientFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if migrated.state.Accounts[accountID] == nil || migrated.state.Accounts[accountID].Name != "Legacy" {
		t.Fatal("legacy account was not migrated")
	}
	if _, err := os.Stat(filepath.Join(root, "metadata.json.migrated")); err != nil {
		t.Fatal("legacy metadata was not retired")
	}
	if _, err := os.Stat(filepath.Join(root, "accounts", accountID, "account.json")); err != nil {
		t.Fatal("migrated account record was not written")
	}
	if migrated.accounts[accountID] == nil {
		t.Fatal("disabled migrated account lost its maintenance runtime")
	}
}
