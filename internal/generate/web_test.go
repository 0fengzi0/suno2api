package generate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebGenerationUsesObservedRoutesHeadersAndPayload(t *testing.T) {
	var posts atomic.Int32
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test.session.jwt" || request.Header.Get("Cookie") != "" || request.Header.Get("Device-Id") != "12345678-abcd-4abc-8abc-123456789abc" {
			t.Error("web request did not isolate session and device identity")
		}
		var outer struct {
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(request.Header.Get("Browser-Token")), &outer) != nil {
			t.Error("Browser-Token outer JSON missing")
		}
		inner, err := base64.StdEncoding.DecodeString(outer.Token)
		var browser struct {
			Timestamp int64 `json:"timestamp"`
		}
		if err != nil || json.Unmarshal(inner, &browser) != nil || time.Now().UnixMilli()-browser.Timestamp > 10000 {
			t.Error("Browser-Token timestamp metadata malformed")
		}
		switch request.URL.Path {
		case "/a/api/c/check":
			fmt.Fprint(writer, `{"required":false,"captcha_version":2}`)
		case "/a/api/billing/info/":
			fmt.Fprint(writer, `{"plan":{"id":"free-plan"},"models":[]}`)
		case "/a/api/generate/v2-web/":
			posts.Add(1)
			json.NewDecoder(request.Body).Decode(&captured)
			fmt.Fprintf(writer, `{"id":"request-web","clips":[{"id":%q,"status":"submitted","model_name":"chirp-goose"}]}`, testClipID)
		case "/a/api/feed/v3":
			var query map[string]any
			json.NewDecoder(request.Body).Decode(&query)
			filters := query["filters"].(map[string]any)
			ids := filters["ids"].(map[string]any)
			if ids["presence"] != "True" {
				t.Error("web feed query used wrong presence value")
			}
			fmt.Fprintf(writer, `{"clips":[{"id":%q,"status":"complete","model_name":"chirp-goose","media_urls":[{"url":"https://cdn1.suno.ai/song.m4a","content_type":"m4a-opus"}]}]}`, testClipID)
		default:
			t.Errorf("unexpected web route: %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{Protocol: "web", BaseURL: server.URL + "/a/api/", Token: `__session=test.session.jwt; ajs_anonymous_id=%2212345678-abcd-4abc-8abc-123456789abc%22; unrelated=must-not-send`, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Generate(context.Background(), Request{Prompt: "calm piano", Model: "v6-mini", Instrumental: true, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || response.RequestedModel != "chirp-goose" {
		t.Fatal("wrong web generation model or request count")
	}
	if captured["mv"] != "chirp-goose" || captured["gpt_description_prompt"] != "calm piano" || captured["make_instrumental"] != true || captured["token"] != nil || captured["token_provider"] != nil {
		t.Fatalf("wrong web body: %#v", captured)
	}
	for _, key := range []string{"stream", "batch_size", "task"} {
		if _, ok := captured[key]; ok {
			t.Fatalf("unobserved native field %s leaked into web request", key)
		}
	}
	transaction, _ := captured["transaction_uuid"].(string)
	if !clipIDPattern.MatchString(transaction) || transaction[14] != '4' {
		t.Fatal("transaction UUID is not v4")
	}
	metadata := captured["metadata"].(map[string]any)
	if metadata["create_mode"] != "simple" || metadata["web_client_pathname"] != "/create" || metadata["create_session_token"] != client.createSessionToken || metadata["user_tier"] != "free-plan" || metadata["disable_volume_normalization"] != false || metadata["lyrics_model"] != "default" {
		t.Fatalf("web metadata differs from observed builder: %#v", metadata)
	}
	clips, err := client.Get(context.Background(), response.IDs())
	if err != nil || len(clips) != 1 || clips[0].Status != "complete" {
		t.Fatalf("web feed polling failed: %v", err)
	}
}

func TestWebCaptchaProviderAndTransactionSessions(t *testing.T) {
	client := &Client{createSessionToken: "one-local-session"}
	payload := generationPayload{Model: "chirp-goose", Prompt: "lyrics", Token: "real-user-verification", GenerationType: "TEXT", Metadata: generationMetadata{CreateMode: "custom"}}
	first, err := client.webPayload(payload, CaptchaCheck{Required: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.webPayload(payload, CaptchaCheck{Required: true, CaptchaVersion: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if first["token_provider"] != 1 || second["token_provider"] != 2 {
		t.Fatal("captcha provider did not match official fallback and v2 branch")
	}
	if first["transaction_uuid"] == second["transaction_uuid"] {
		t.Fatal("distinct submissions share transaction UUID")
	}
	metadata := first["metadata"].(map[string]any)
	if _, ok := metadata["user_tier"]; ok {
		t.Fatal("missing tier was fabricated")
	}
	if _, ok := metadata["lyrics_model"]; ok {
		t.Fatal("simple lyrics model incorrectly added to custom request")
	}
}

func TestWebDestinationAndDeviceIdentityRequirements(t *testing.T) {
	for _, config := range []Config{{Protocol: "web", BaseURL: "https://suno.com/api/"}, {Protocol: "web", BaseURL: DefaultBaseURL}, {Protocol: "android", BaseURL: WebBaseURL}, {Protocol: "web", BaseURL: "https://suno.com/a/unrelated/"}} {
		if _, err := NewClient(config); err == nil {
			t.Fatal("protocol sent credentials to an unapproved host or prefix")
		}
	}
	client, err := NewClient(Config{Protocol: "web", Token: "test.session.jwt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.webDeviceID(); err == nil || !strings.Contains(err.Error(), "device-id") {
		t.Fatal("missing actual Cookie device identity was silently replaced")
	}
}
