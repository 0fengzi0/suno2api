package session

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const DefaultAuthBaseURL = "https://auth.suno.com/"

type coordinator struct {
	gate             chan struct{}
	mu               sync.Mutex
	refreshCount     int64
	lastRefresh      time.Time
	lastAttempt      time.Time
	lastError        error
	errorFingerprint [32]byte
	nextAttempt      time.Time
	failures         int
}

var coordinators sync.Map

type Manager struct {
	path          string
	authPath      string
	baseURL       *url.URL
	httpClient    *http.Client
	refreshBefore time.Duration
	retryMin      time.Duration
	retryMax      time.Duration
	lockTimeout   time.Duration
	maxFailures   int
	shared        *coordinator
}

func NewManager(config Config) (*Manager, error) {
	if strings.TrimSpace(config.TokenFile) == "" {
		return nil, &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
	}
	path, err := filepath.Abs(config.TokenFile)
	if err != nil {
		return nil, &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	authPath := ""
	if config.AuthCookieFile != "" {
		authPath, err = filepath.Abs(config.AuthCookieFile)
		if err != nil {
			return nil, &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
		}
		if resolved, err := filepath.EvalSymlinks(authPath); err == nil {
			authPath = resolved
		}
	}
	base := config.AuthBaseURL
	if base == "" {
		base = DefaultAuthBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid session authentication base URL")
	}
	host := strings.ToLower(parsed.Hostname())
	pathPrefix := strings.TrimRight(parsed.Path, "/") + "/"
	official := parsed.Scheme == "https" && (parsed.Port() == "" || parsed.Port() == "443") && ((host == "auth.suno.com" && pathPrefix == "/") || (host == "suno.com" && pathPrefix == "/api/clerk/"))
	address := net.ParseIP(host)
	test := config.HTTPClient != nil && address != nil && address.IsLoopback() && (parsed.Scheme == "http" || parsed.Scheme == "https")
	if !official && !test {
		return nil, errors.New("session credentials may only be sent to the official Suno authentication endpoint")
	}
	if authPath != "" && !test && host != "auth.suno.com" {
		return nil, errors.New("the separate authentication Cookie file is restricted to auth.suno.com")
	}
	parsed.Path = pathPrefix
	client := http.Client{Timeout: 45 * time.Second}
	if config.HTTPClient != nil {
		client = *config.HTTPClient
	}
	if client.Timeout <= 0 {
		client.Timeout = 45 * time.Second
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if config.RefreshBefore <= 0 {
		config.RefreshBefore = time.Minute
	}
	if config.RetryMin <= 0 {
		config.RetryMin = 5 * time.Second
	}
	if config.RetryMax <= 0 {
		config.RetryMax = time.Minute
	}
	if config.RetryMax < config.RetryMin {
		config.RetryMax = config.RetryMin
	}
	if config.LockTimeout <= 0 {
		config.LockTimeout = 10 * time.Second
	}
	if config.MaxFailures <= 0 {
		config.MaxFailures = 3
	}
	created := &coordinator{gate: make(chan struct{}, 1)}
	created.gate <- struct{}{}
	actual, _ := coordinators.LoadOrStore(strings.ToLower(path), created)
	return &Manager{path: path, authPath: authPath, baseURL: parsed, httpClient: &client, refreshBefore: config.RefreshBefore, retryMin: config.RetryMin, retryMax: config.RetryMax, lockTimeout: config.LockTimeout, maxFailures: config.MaxFailures, shared: actual.(*coordinator)}, nil
}

func (manager *Manager) Status(ctx context.Context) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	source, err := manager.loadCredential()
	if err != nil {
		return Status{State: "unsupported_credential"}, err
	}
	now := time.Now()
	status := Status{State: "active", IssuedAt: time.Unix(source.claims.Issued, 0).UTC(), ExpiresAt: time.Unix(source.claims.Expiry, 0).UTC(), RefreshAt: manager.refreshAt(source), CanRefresh: source.canRefresh}
	status.AuthCookieFileConfigured = manager.authPath != ""
	if source.authError != nil {
		status.LastRefreshError = errorCode(source.authError)
		status.ReauthenticationRequired = true
	}
	if !source.canRefresh {
		status.State = "manual_token"
	}
	if !now.Before(status.RefreshAt) {
		status.State = "refresh_due"
	}
	if !now.Before(status.ExpiresAt) {
		status.State = "expired"
	}
	manager.shared.mu.Lock()
	status.RefreshCount = manager.shared.refreshCount
	status.LastRefresh = manager.shared.lastRefresh
	status.LastRefreshAttempt = manager.shared.lastAttempt
	if manager.shared.errorFingerprint == source.fingerprint && manager.shared.lastError != nil {
		status.LastRefreshError = errorCode(manager.shared.lastError)
		status.NextRetryAt = manager.shared.nextAttempt
		status.ReauthenticationRequired = terminalRefreshError(manager.shared.lastError)
		if status.ReauthenticationRequired && now.Before(status.ExpiresAt) {
			status.State = "reauthentication_required"
		}
		if errors.Is(manager.shared.lastError, ErrChallenge) {
			status.State = "verification_required"
			status.ReauthenticationRequired = false
		}
		if errors.Is(manager.shared.lastError, ErrRefreshRejected) {
			status.State = "refresh_blocked"
			status.ReauthenticationRequired = false
		}
	}
	manager.shared.mu.Unlock()
	return status, nil
}

func (manager *Manager) Ensure(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source, err := manager.loadCredential()
	if err != nil {
		return "", err
	}
	if time.Now().Before(manager.refreshAt(source)) {
		return source.token, nil
	}
	token, err := manager.refresh(ctx, source, false)
	if err == nil {
		return token, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	latest, readError := manager.loadCredential()
	if readError == nil && time.Now().Unix() < latest.claims.Expiry {
		return latest.token, nil
	}
	return "", &Error{Code: "expired", cause: ErrAuthentication}
}

func (manager *Manager) Refresh(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source, err := manager.loadCredential()
	if err != nil {
		return "", err
	}
	return manager.refresh(ctx, source, true)
}

func (manager *Manager) refresh(ctx context.Context, initial credential, force bool) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-manager.shared.gate:
	}
	defer func() { manager.shared.gate <- struct{}{} }()
	source, err := manager.loadCredential()
	if err != nil {
		return "", err
	}
	now := time.Now()
	if source.fingerprint != initial.fingerprint && now.Unix() < source.claims.Expiry {
		return source.token, nil
	}
	if !force && now.Before(manager.refreshAt(source)) {
		return source.token, nil
	}
	if !source.canRefresh {
		return "", &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
	}
	manager.shared.mu.Lock()
	if manager.shared.errorFingerprint != source.fingerprint {
		manager.shared.lastError = nil
		manager.shared.failures = 0
		manager.shared.nextAttempt = time.Time{}
	}
	if manager.shared.lastError != nil && (terminalRefreshError(manager.shared.lastError) || now.Before(manager.shared.nextAttempt)) {
		err := manager.shared.lastError
		manager.shared.mu.Unlock()
		return "", err
	}
	manager.shared.mu.Unlock()
	release, err := manager.acquireFileLock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	current, err := manager.loadCredential()
	if err != nil {
		return "", err
	}
	if current.fingerprint != source.fingerprint {
		if now.Unix() < current.claims.Expiry {
			return current.token, nil
		}
		return "", &Error{Code: "file_conflict", cause: ErrConflict}
	}
	manager.shared.mu.Lock()
	manager.shared.lastAttempt = now.UTC()
	manager.shared.mu.Unlock()
	renewed, err := manager.requestRefresh(ctx, source)
	if err == nil {
		err = manager.replaceCredential(ctx, source, renewed)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		manager.rememberFailure(source, err)
		return "", err
	}
	manager.shared.mu.Lock()
	manager.shared.refreshCount++
	manager.shared.lastRefresh = time.Now().UTC()
	manager.shared.lastError = nil
	manager.shared.failures = 0
	manager.shared.nextAttempt = time.Time{}
	manager.shared.mu.Unlock()
	return renewed.token, nil
}

func (manager *Manager) refreshAt(source credential) time.Time {
	lifetime := time.Duration(source.claims.Expiry-source.claims.Issued) * time.Second
	lead := manager.refreshBefore
	if lead > lifetime/2 {
		lead = lifetime / 2
	}
	return time.Unix(source.claims.Expiry, 0).Add(-lead).UTC()
}

func terminalRefreshError(err error) bool {
	return errors.Is(err, ErrAuthentication) || errors.Is(err, ErrRefreshCredentialsRequired) || errors.Is(err, ErrUnsupportedCredential) || errors.Is(err, ErrInvalidResponse) || errors.Is(err, ErrChallenge) || errors.Is(err, ErrRefreshRejected)
}

func (manager *Manager) rememberFailure(source credential, err error) {
	manager.shared.mu.Lock()
	defer manager.shared.mu.Unlock()
	manager.shared.lastError = err
	manager.shared.errorFingerprint = source.fingerprint
	manager.shared.failures++
	delay := manager.retryMin
	for attempt := 1; attempt < manager.shared.failures && delay < manager.retryMax; attempt++ {
		delay *= 2
		if delay > manager.retryMax {
			delay = manager.retryMax
		}
	}
	var failure *Error
	if errors.As(err, &failure) && failure.RetryAfter > delay {
		delay = failure.RetryAfter
	}
	manager.shared.nextAttempt = time.Now().Add(delay).UTC()
}

func (manager *Manager) Run(ctx context.Context, onEvent func(Event)) error {
	failures := 0
	emit := func(state string, status Status, err error, delay time.Duration) {
		if onEvent != nil {
			code := ""
			if err != nil {
				code = errorCode(err)
			}
			onEvent(Event{State: state, Status: status, ErrorCode: code, RetryIn: delay})
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := manager.Status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emit("stopped", status, err, 0)
			return err
		}
		if !status.CanRefresh {
			err := &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
			emit("stopped", status, err, 0)
			return err
		}
		if status.ReauthenticationRequired {
			err := &Error{Code: "authentication_required", cause: ErrAuthentication}
			emit("stopped", status, err, 0)
			return err
		}
		if status.State == "verification_required" {
			err := &Error{Code: "verification_required", cause: ErrChallenge}
			emit("stopped", status, err, 0)
			return err
		}
		if status.State == "refresh_blocked" {
			err := &Error{Code: "http_403_unclassified", cause: ErrRefreshRejected}
			emit("stopped", status, err, 0)
			return err
		}
		delay := time.Until(status.RefreshAt)
		if status.NextRetryAt.After(status.RefreshAt) && status.LastRefreshError != "" {
			delay = time.Until(status.NextRetryAt)
		}
		if delay > 0 {
			emit("waiting", status, nil, delay)
			if err := waitContext(ctx, delay); err != nil {
				return err
			}
		}
		source, readError := manager.loadCredential()
		if readError != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emit("stopped", status, readError, 0)
			return readError
		}
		beforeRefresh, _ := manager.Status(ctx)
		_, err = manager.refresh(ctx, source, false)
		updated, _ := manager.Status(ctx)
		if err == nil {
			failures = 0
			if updated.RefreshCount > beforeRefresh.RefreshCount {
				emit("refreshed", updated, nil, 0)
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminalRefreshError(err) {
			emit("stopped", updated, err, 0)
			return err
		}
		failures++
		if failures >= manager.maxFailures {
			exhausted := &Error{Code: "unavailable", cause: ErrUnavailable}
			emit("stopped", updated, exhausted, 0)
			return exhausted
		}
		retry := time.Until(updated.NextRetryAt)
		if retry <= 0 {
			retry = manager.retryMin
		}
		emit("retrying", updated, err, retry)
		if err := waitContext(ctx, retry); err != nil {
			return err
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
