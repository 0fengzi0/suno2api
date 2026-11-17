package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func authManagerFixture(t *testing.T, access, auth string, handler http.HandlerFunc) *Manager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	manager, err := NewManager(Config{TokenFile: access, AuthCookieFile: auth, HTTPClient: server.Client(), AuthBaseURL: server.URL + "/", RetryMin: time.Millisecond, RetryMax: 3 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestSeparateAuthenticationCookiesRotateWithoutLeakingToAccessFile(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	access := fileFixture(t, old, true)
	auth := filepath.Join(filepath.Dir(access), "auth.cookie")
	if err := os.WriteFile(auth, []byte("\ufeffcookie: __client=original-client; ajs_anonymous_id=auth-device; analytics=local-only"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := authManagerFixture(t, access, auth, func(writer http.ResponseWriter, request *http.Request) {
		cookies := request.Header.Get("Cookie")
		if !strings.Contains(cookies, "__client=original-client") || !strings.Contains(cookies, "__session="+old) || strings.Contains(cookies, "ajs_anonymous_id") || strings.Contains(cookies, "analytics") {
			t.Error("separate auth Cookie request has an incorrect scope")
		}
		http.SetCookie(writer, &http.Cookie{Name: "__client", Value: "rotated-client", Path: "/", Secure: true, HttpOnly: true})
		sessionResponse(writer, renewed)
	})
	if token, err := manager.Refresh(context.Background()); err != nil || token != renewed {
		t.Fatalf("normal refresh failed: %v", err)
	}
	accessContent, _ := os.ReadFile(access)
	authContent, _ := os.ReadFile(auth)
	if !strings.Contains(string(accessContent), renewed) || !strings.Contains(string(accessContent), "existing-device") || strings.Contains(string(accessContent), "rotated-client") {
		t.Fatal("access file lost its device or received long-lived authentication Cookie")
	}
	if !strings.Contains(string(authContent), "__client=rotated-client") || !strings.Contains(string(authContent), "__session="+renewed) || !strings.Contains(string(authContent), "auth-device") {
		t.Fatal("normal authentication Cookie rotation was not preserved")
	}
	status, _ := manager.Status(context.Background())
	if !status.AuthCookieFileConfigured || status.RefreshCount != 1 {
		t.Fatal("separate authentication status not projected")
	}
}

func TestCombinedAuthenticationStoreRotatesClientAndSuffixOnlySession(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	for _, separateSamePath := range []bool{false, true} {
		path := fileFixture(t, old, true)
		contents, _ := os.ReadFile(path)
		os.WriteFile(path, append(contents, []byte("; __client=old-client")...), 0600)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.SetCookie(writer, &http.Cookie{Name: "__client", Value: "new-client", Secure: true, Path: "/"})
			http.SetCookie(writer, &http.Cookie{Name: "__session_rotated", Value: renewed, Secure: true, Path: "/"})
			fmt.Fprintf(writer, `{"jwt":%q}`, renewed)
		}))
		config := Config{TokenFile: path, HTTPClient: server.Client(), AuthBaseURL: server.URL + "/"}
		if separateSamePath {
			config.AuthCookieFile = path
		}
		manager, err := NewManager(config)
		if err != nil {
			t.Fatal(err)
		}
		_, err = manager.Refresh(context.Background())
		server.Close()
		if err != nil {
			t.Fatalf("combined refresh failed: %v", err)
		}
		updated, _ := os.ReadFile(path)
		if !strings.Contains(string(updated), "__client=new-client") || strings.Contains(string(updated), "__client=old-client") {
			t.Fatal("combined store lost normal long-lived cookie rotation")
		}
		saved, err := readCredential(path)
		if err != nil || saved.token != renewed {
			t.Fatal("suffix-only Set-Cookie left the preferred base session stale")
		}
	}
}

func TestAuthenticationFileChangesResetRefusalAndConflictsPreserveNewCookies(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	renewed := tokenFixture("session", "user", now, now+600)
	access := fileFixture(t, old, true)
	auth := filepath.Join(filepath.Dir(access), "auth.cookie")
	os.WriteFile(auth, []byte("__client=first-client"), 0600)
	var calls atomic.Int32
	manager := authManagerFixture(t, access, auth, func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		if !strings.Contains(request.Header.Get("Cookie"), "__client=second-client") {
			t.Error("new authentication file was not re-read")
		}
		sessionResponse(writer, renewed)
	})
	if _, err := manager.Refresh(context.Background()); !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("expected unclassified blocked response: %v", err)
	}
	if _, err := manager.Refresh(context.Background()); !errors.Is(err, ErrRefreshRejected) || calls.Load() != 1 {
		t.Fatal("terminal refusal was retried without new credentials")
	}
	if err := os.WriteFile(auth, []byte("__client=second-client"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Refresh(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("new auth file did not reset prior refusal: %v", err)
	}
	access2 := fileFixture(t, old, true)
	auth2 := filepath.Join(filepath.Dir(access2), "auth.cookie")
	os.WriteFile(auth2, []byte("__client=initial"), 0600)
	before, _ := os.ReadFile(access2)
	conflict := authManagerFixture(t, access2, auth2, func(writer http.ResponseWriter, request *http.Request) {
		os.WriteFile(auth2, []byte("__client=user-updated"), 0600)
		sessionResponse(writer, renewed)
	})
	if _, err := conflict.Refresh(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("authentication file conflict not detected: %v", err)
	}
	after, _ := os.ReadFile(access2)
	authAfter, _ := os.ReadFile(auth2)
	if string(before) != string(after) || string(authAfter) != "__client=user-updated" {
		t.Fatal("concurrent user authentication edit was overwritten")
	}
}

func TestMissingSeparateAuthenticationFileDoesNotBlockAnUnexpiredAccessToken(t *testing.T) {
	now := time.Now().Unix()
	token := tokenFixture("session", "user", now-300, now+20)
	access := fileFixture(t, token, true)
	manager := authManagerFixture(t, access, filepath.Join(filepath.Dir(access), "not-created.cookie"), func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("missing authentication file unexpectedly submitted request")
	})
	if actual, err := manager.Ensure(context.Background()); err != nil || actual != token {
		t.Fatalf("valid access token was blocked by missing auth Cookie: %v", err)
	}
	status, err := manager.Status(context.Background())
	if err != nil || status.CanRefresh || !status.ReauthenticationRequired {
		t.Fatal("missing auth Cookie status was not explicit")
	}
}

func TestRefreshDistinguishesChallengeAuthenticationAndUnclassifiedRefusals(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	for _, test := range []struct {
		name, header, body, code, state string
		cause                           error
	}{
		{"challenge", "challenge", "<html>challenge</html>", "verification_required", "verification_required", ErrChallenge},
		{"authentication", "", `{"errors":[{"code":"session_expired"}]}`, "authentication_required", "reauthentication_required", ErrAuthentication},
		{"unclassified", "", "<html>denied</html>", "http_403_unclassified", "refresh_blocked", ErrRefreshRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := managerFixture(t, fileFixture(t, old, true), func(writer http.ResponseWriter, request *http.Request) {
				if test.header != "" {
					writer.Header().Set("CF-Mitigated", test.header)
				}
				writer.WriteHeader(http.StatusForbidden)
				fmt.Fprint(writer, test.body)
			})
			_, err := manager.Refresh(context.Background())
			if !errors.Is(err, test.cause) || errorCode(err) != test.code {
				t.Fatalf("incorrect refusal classification: %v", err)
			}
			status, _ := manager.Status(context.Background())
			if status.State != test.state {
				t.Fatalf("incorrect state: %s", status.State)
			}
			if runError := manager.Run(context.Background(), nil); !errors.Is(runError, test.cause) {
				t.Fatalf("keepalive did not stop on refusal: %v", runError)
			}
		})
	}
}

func TestKeepaliveStopsAfterBoundedTemporaryFailures(t *testing.T) {
	now := time.Now().Unix()
	old := tokenFixture("session", "user", now-300, now+20)
	var calls atomic.Int32
	manager := managerFixture(t, fileFixture(t, old, true), func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Run(ctx, nil); !errors.Is(err, ErrUnavailable) || calls.Load() != 3 {
		t.Fatalf("temporary failures were not bounded: calls=%d err=%v", calls.Load(), err)
	}
}
