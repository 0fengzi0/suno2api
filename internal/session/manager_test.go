package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tokenFixture(sessionID, subject string, issued, expires int64) string {
	payload, _ := json.Marshal(map[string]any{"iss": "https://auth.suno.com", "sid": sessionID, "sub": subject, "iat": issued, "exp": expires})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func fileFixture(t *testing.T, token string, cookies bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.cookie")
	contents := "Bearer " + token
	if cookies {
		contents = "Cookie: analytics=local-only; __session=" + token + "; __session_suffix=" + token + "; ajs_anonymous_id=%22existing-device%22; preferences=preserved\n"
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func managerFixture(t *testing.T, path string, handler http.HandlerFunc) *Manager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	manager, err := NewManager(Config{TokenFile: path, HTTPClient: server.Client(), AuthBaseURL: server.URL + "/", RetryMin: 10 * time.Millisecond, RetryMax: 30 * time.Millisecond, LockTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func sessionResponse(writer http.ResponseWriter, token string) {
	http.SetCookie(writer, &http.Cookie{Name: "__session", Value: token, Secure: true, Path: "/", HttpOnly: true})
	_ = json.NewEncoder(writer).Encode(map[string]string{"jwt": token})
}

func TestEnsureRefreshesEarlyWithOnlyAuthenticationCookiesAndPreservesDevice(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("test-session", "test-user", now-300, now+20)
	renewed := tokenFixture("test-session", "test-user", now, now+600)
	path := fileFixture(t, old, true)
	var count atomic.Int32
	manager := managerFixture(t, path, func(writer http.ResponseWriter, request *http.Request) {
		count.Add(1)
		body, _ := io.ReadAll(request.Body)
		if request.Method != "POST" || request.URL.Path != "/v1/client/sessions/test-session/tokens" || len(body) != 0 || request.Header.Get("Authorization") != "" || request.Header.Get("Origin") != "https://suno.com" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("refresh request differs from observed normal authentication flow")
		}
		if strings.Contains(request.Header.Get("Cookie"), "ajs_") || strings.Contains(request.Header.Get("Cookie"), "analytics") || !strings.Contains(request.Header.Get("Cookie"), "__session=") {
			t.Error("refresh sent non-authentication cookies")
		}
		sessionResponse(writer, renewed)
	})
	got, err := manager.Ensure(context.Background())
	if err != nil || got != renewed {
		t.Fatalf("refresh failed: %v", err)
	}
	if _, err := manager.Ensure(context.Background()); err != nil || count.Load() != 1 {
		t.Fatal("fresh token triggered duplicate refresh")
	}
	contents, _ := os.ReadFile(path)
	if !strings.Contains(string(contents), "ajs_anonymous_id=%22existing-device%22") || !strings.Contains(string(contents), "analytics=local-only") || strings.Contains(string(contents), old) {
		t.Fatal("refresh did not preserve local cookies or replace the old session")
	}
	status, err := manager.Status(context.Background())
	if err != nil || status.RefreshCount != 1 || status.ExpiresAt.Unix() != now+600 || status.LastRefresh.IsZero() {
		t.Fatalf("incorrect refresh status: %#v %v", status, err)
	}
	projected, _ := json.Marshal(status)
	for _, private := range []string{old, renewed, "test-session", "test-user", "existing-device"} {
		if strings.Contains(string(projected), private) {
			t.Fatal("status exposed private session material")
		}
	}
}

func TestExpiredCookieCanUseNormalRefreshButRawTokenCannot(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now-1)
	renewed := tokenFixture("session", "user", now, now+600)
	manager := managerFixture(t, fileFixture(t, old, true), func(writer http.ResponseWriter, request *http.Request) { sessionResponse(writer, renewed) })
	got, err := manager.Ensure(context.Background())
	if err != nil || got != renewed {
		t.Fatalf("expired Cookie could not refresh: %v", err)
	}
	var calls atomic.Int32
	raw := managerFixture(t, fileFixture(t, old, false), func(writer http.ResponseWriter, request *http.Request) { calls.Add(1) })
	if _, err := raw.Refresh(context.Background()); !errors.Is(err, ErrRefreshCredentialsRequired) {
		t.Fatalf("raw access token used as refresh Cookie: %v", err)
	}
	if _, err := raw.Ensure(context.Background()); !errors.Is(err, ErrAuthentication) || calls.Load() != 0 {
		t.Fatal("expired raw token was sent for Cookie refresh")
	}
	active := tokenFixture("session", "user", now-300, now+600)
	rawPath := fileFixture(t, active, false)
	rawManager := managerFixture(t, rawPath, func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("active raw token unexpectedly refreshed")
	})
	if got, err := rawManager.Ensure(context.Background()); err != nil || got != active {
		t.Fatal("existing valid raw token was blocked")
	}
}

func TestInvalidRefreshResultsNeverChangeTheTokenFile(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	valid := tokenFixture("session", "user", now, now+600)
	for _, scenario := range []string{"wrong-session", "wrong-user", "wrong-issuer", "no-extension", "expired-new", "missing-jwt", "missing-cookie", "wrong-cookie", "cookie-domain", "cookie-path", "insecure-cookie", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			path := fileFixture(t, old, true)
			before, _ := os.ReadFile(path)
			var manager *Manager
			manager = managerFixture(t, path, func(writer http.ResponseWriter, request *http.Request) {
				token := valid
				switch scenario {
				case "wrong-session":
					token = tokenFixture("another-session", "user", now, now+600)
				case "wrong-user":
					token = tokenFixture("session", "another-user", now, now+600)
				case "wrong-issuer":
					payload, _ := json.Marshal(map[string]any{"iss": "https://unrelated.invalid", "sid": "session", "sub": "user", "iat": now, "exp": now + 600})
					token = "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
				case "no-extension":
					token = old
				case "expired-new":
					token = tokenFixture("session", "user", now-300, now-1)
				case "missing-jwt":
					http.SetCookie(writer, &http.Cookie{Name: "__session", Value: valid, Secure: true, Path: "/"})
					fmt.Fprint(writer, `{}`)
					return
				case "missing-cookie":
					json.NewEncoder(writer).Encode(map[string]string{"jwt": valid})
					return
				case "redirect":
					http.Redirect(writer, request, "https://unrelated.invalid/", http.StatusTemporaryRedirect)
					return
				}
				cookie := &http.Cookie{Name: "__session", Value: token, Secure: true, Path: "/"}
				switch scenario {
				case "wrong-cookie":
					cookie.Value = old
				case "cookie-domain":
					cookie.Domain = "unrelated.invalid"
				case "cookie-path":
					cookie.Path = "/unrelated"
				case "insecure-cookie":
					manager.baseURL.Scheme = "https"
					cookie.Secure = false
				}
				http.SetCookie(writer, cookie)
				json.NewEncoder(writer).Encode(map[string]string{"jwt": token})
			})
			if scenario == "insecure-cookie" {
				manager.baseURL.Scheme = "http"
			}
			_, err := manager.Refresh(context.Background())
			if err == nil || !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("invalid result accepted: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("invalid refresh overwrote token file")
			}
			if strings.Contains(err.Error(), old) || strings.Contains(err.Error(), valid) || strings.Contains(err.Error(), "/sessions/") {
				t.Fatal("refresh error exposed token or session URL")
			}
		})
	}
}

func TestConcurrentEnsureAcrossManagersMakesOneRefresh(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	path := fileFixture(t, old, true)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		sessionResponse(writer, renewed)
	}))
	defer server.Close()
	first, err := NewManager(Config{TokenFile: path, HTTPClient: server.Client(), AuthBaseURL: server.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewManager(Config{TokenFile: path, HTTPClient: server.Client(), AuthBaseURL: server.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for index := 0; index < 20; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			manager := first
			if index%2 == 0 {
				manager = second
			}
			if token, err := manager.Ensure(context.Background()); err != nil || token != renewed {
				t.Errorf("concurrent Ensure failed: %v", err)
			}
		}(index)
	}
	workers.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh stampede: %d requests", calls.Load())
	}
}

func TestRejectedRefreshKeepsValidTokenUntilExpiryAndRemembersFileFingerprint(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	path := fileFixture(t, old, true)
	var calls atomic.Int32
	manager := managerFixture(t, path, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusUnauthorized)
	})
	for index := 0; index < 3; index++ {
		token, err := manager.Ensure(context.Background())
		if err != nil || token != old {
			t.Fatal("failed pre-refresh blocked still-valid token")
		}
	}
	status, _ := manager.Status(context.Background())
	if calls.Load() != 1 || !status.ReauthenticationRequired || status.LastRefreshError != "authentication_required" || status.RefreshCount != 0 {
		t.Fatalf("failed refresh was retried or reported as successful: %#v", status)
	}
	if err := manager.Run(context.Background(), nil); !errors.Is(err, ErrAuthentication) || calls.Load() != 1 {
		t.Fatal("keepalive did not stop after terminal authentication refusal")
	}
	replacement := tokenFixture("session", "user", now, now+600)
	if err := os.WriteFile(path, []byte("__session="+replacement+"; ajs_anonymous_id=device"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status(context.Background())
	if status.ReauthenticationRequired || status.LastRefreshError != "" {
		t.Fatal("new user file inherited terminal refusal from old cookies")
	}
	if token, err := manager.Ensure(context.Background()); err != nil || token != replacement {
		t.Fatal("new user session was not adopted")
	}
}

func TestRateLimitBackoffAndRunCancellation(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	var calls atomic.Int32
	manager := managerFixture(t, fileFixture(t, old, true), func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		sessionResponse(writer, renewed)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var states []string
	err := manager.Run(ctx, func(event Event) {
		states = append(states, event.State)
		if event.State == "refreshed" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || calls.Load() != 2 || len(states) < 2 || states[0] != "retrying" {
		t.Fatalf("rate limit retry/cancellation failed: calls %d states %v err %v", calls.Load(), states, err)
	}
	status, _ := manager.Status(context.Background())
	if status.RefreshCount != 1 {
		t.Fatal("successful retry was not recorded")
	}
	activeManager := managerFixture(t, fileFixture(t, renewed, true), func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("cancelled waiting keepalive submitted request")
	})
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := activeManager.Run(cancelled, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("keepalive ignored cancellation")
	}
}

func TestConcurrentUserFileUpdateIsNotOverwritten(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	replacement := tokenFixture("new-session", "user", now, now+900)
	path := fileFixture(t, old, true)
	userFile := "__session=" + replacement + "; ajs_anonymous_id=user-updated-device"
	manager := managerFixture(t, path, func(writer http.ResponseWriter, request *http.Request) {
		if err := os.WriteFile(path, []byte(userFile), 0600); err != nil {
			t.Error(err)
		}
		sessionResponse(writer, renewed)
	})
	if _, err := manager.Refresh(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent update not detected: %v", err)
	}
	contents, _ := os.ReadFile(path)
	if string(contents) != userFile {
		t.Fatal("refresh overwrote user's newer authentication file")
	}
}

func TestRejectsRealtimeCredentialAndUnapprovedAuthenticationDestinations(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"iss": "https://auth.suno.com", "sid": "fake-session", "sub": "fake-user", "iat": time.Now().Unix() - 1, "exp": time.Now().Unix() + 600, "x-ably-capability": "subscribe-only"})
	abbly := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	manager := managerFixture(t, fileFixture(t, abbly, true), func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("realtime token was sent to authentication")
	})
	status, err := manager.Status(context.Background())
	if !errors.Is(err, ErrUnsupportedCredential) || status.State != "unsupported_credential" {
		t.Fatal("realtime subscription token was accepted as login")
	}
	if _, err := manager.Refresh(context.Background()); !errors.Is(err, ErrUnsupportedCredential) {
		t.Fatal("realtime credential was treated as renewable")
	}
	for _, base := range []string{"https://unrelated.invalid/", "https://auth.suno.com.unrelated.invalid/", "http://auth.suno.com/", "https://auth.suno.com/unrelated/", "https://suno.com/a/api/", "https://user:secret@auth.suno.com/"} {
		if _, err := NewManager(Config{TokenFile: "unused.cookie", AuthBaseURL: base}); err == nil {
			t.Fatal("accepted unapproved authentication destination")
		}
	}
}

func TestRunRechecksScheduleAfterExternalRefreshWhileWaiting(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+61)
	renewed := tokenFixture("session", "user", now, now+600)
	path := fileFixture(t, old, true)
	var calls atomic.Int32
	manager := managerFixture(t, path, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		sessionResponse(writer, renewed)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	replaced := false
	refreshedEvents := 0
	err := manager.Run(ctx, func(event Event) {
		if event.State == "waiting" && !replaced {
			replaced = true
			if err := os.WriteFile(path, []byte("__session="+renewed+"; ajs_anonymous_id=external-device"), 0600); err != nil {
				t.Error(err)
			}
		} else if event.State == "waiting" && replaced {
			cancel()
		}
		if event.State == "refreshed" {
			refreshedEvents++
		}
	})
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 || refreshedEvents != 0 {
		t.Fatalf("old timer forced duplicate refresh: calls=%d events=%d err=%v", calls.Load(), refreshedEvents, err)
	}
}

func TestRunSuccessfulEventCancellationDoesNotEmitSpuriousStoppedEvent(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	var calls atomic.Int32
	manager := managerFixture(t, fileFixture(t, old, true), func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		sessionResponse(writer, renewed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var states []string
	err := manager.Run(ctx, func(event Event) {
		states = append(states, event.State)
		if event.State == "refreshed" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 || len(states) != 1 || states[0] != "refreshed" {
		t.Fatalf("successful cancellation emitted spurious events or additional requests: calls=%d states=%v err=%v", calls.Load(), states, err)
	}
}

func TestChallengePageWithoutMitigatedHeaderStopsAsVerificationRequired(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now-1)
	path := fileFixture(t, old, true)
	manager := managerFixture(t, path, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusForbidden)
		fmt.Fprint(writer, "<!DOCTYPE html><html><head><title>Just a moment...</title></head><body><script src=\"/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1\"></script>Enable JavaScript and cookies to continue</body></html>")
	})
	_, err := manager.Refresh(context.Background())
	var refreshErr *Error
	if !errors.Is(err, ErrChallenge) || !errors.As(err, &refreshErr) || refreshErr.Code != "verification_required" {
		t.Fatalf("HTML challenge page was not treated as a verification requirement: %v", err)
	}
	contents, _ := os.ReadFile(path)
	if !strings.Contains(string(contents), old) {
		t.Fatal("challenge response changed the token file")
	}
}

func TestPlainHTMLForbiddenWithoutChallengeMarkersStaysUnclassified(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now-1)
	manager := managerFixture(t, fileFixture(t, old, true), func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusForbidden)
		fmt.Fprint(writer, "<html><body>Access denied</body></html>")
	})
	_, err := manager.Refresh(context.Background())
	var refreshErr *Error
	if !errors.Is(err, ErrRefreshRejected) || !errors.As(err, &refreshErr) || refreshErr.Code != "http_403_unclassified" {
		t.Fatalf("403 without challenge markers must stay unclassified: %v", err)
	}
}
