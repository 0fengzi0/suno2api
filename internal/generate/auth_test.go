package generate

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenProviderUsesRequestContextAndDoesNotFallBackAfterFailure(t *testing.T) {
	type marker struct{}
	ctx := context.WithValue(context.Background(), marker{}, "request-context")
	calls := 0
	client := &Client{token: "stale.token.value", tokenProvider: func(actual context.Context) (string, error) {
		calls++
		if actual.Value(marker{}) != "request-context" {
			t.Fatal("refresh provider lost caller context")
		}
		return "fresh.token.value", nil
	}}
	value, err := client.currentToken(ctx)
	if err != nil || value != "fresh.token.value" || calls != 1 {
		t.Fatal("request did not use shared session provider")
	}
	client.tokenProvider = func(context.Context) (string, error) { return "", context.Canceled }
	if value, err := client.currentToken(ctx); value != "" || !errors.Is(err, context.Canceled) {
		t.Fatal("failed provider fell back to a stale token")
	}
}

func TestNormalizeToken(t *testing.T) {
	for _, value := range []string{"a.b.c", "  a.b.c\r\n", "Bearer a.b.c", "bearer a.b.c", "Cookie: other=ignored; __session=a.b.c; analytics=ignored", "__session_suffix=a.b.c"} {
		token, err := normalizeToken(value)
		if err != nil || token != "a.b.c" {
			t.Fatalf("normalize token: got %q, %v", token, err)
		}
	}
	for _, value := range []string{"", "Bearer ", "secret\nsecond", "a b", strings.Repeat("x", 16385)} {
		_, err := normalizeToken(value)
		if err == nil {
			t.Fatal("accepted empty, multiline or oversized token")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("token leaked in validation error")
		}
	}
	if _, err := normalizeToken(""); !errors.Is(err, ErrAuthentication) {
		t.Fatal("missing token must be recognizable as authentication error")
	}
}

func TestNormalizeTokenRejectsExpiredJWT(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	_, err := normalizeToken("header." + payload + ".signature")
	if !errors.Is(err, ErrAuthentication) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected actionable expired-session error, got %v", err)
	}
}

func TestReadTokenFileRejectsDirectoriesAndOversizedValues(t *testing.T) {
	directory := t.TempDir()
	if _, err := readTokenFile(directory); err == nil {
		t.Fatal("accepted directory as token")
	}
	path := filepath.Join(directory, "token.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("s", 20000)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(path); err == nil {
		t.Fatal("accepted oversized token file")
	}
	if err := os.WriteFile(path, []byte("Bearer refreshed.token.value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := readTokenFile(path)
	if err != nil || value != "refreshed.token.value" {
		t.Fatalf("token file: got %q, %v", value, err)
	}
}
