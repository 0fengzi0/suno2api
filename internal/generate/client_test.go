package generate

import (
	"context"
	"encoding/json"
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

const testClipID = "12345678-1234-4567-8901-123456789012"

func testClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(Config{Protocol: "android", BaseURL: server.URL + "/api/", Token: "test.token.value", HTTPClient: server.Client(), PollInterval: time.Millisecond, WaitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGenerateAndWaitUsesMiniDefaultAndPolls(t *testing.T) {
	var submitted atomic.Int32
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test.token.value" {
			t.Error("missing bearer authorization")
		}
		switch request.URL.Path {
		case "/api/c/check":
			fmt.Fprint(writer, `{"required":false}`)
		case "/api/billing/info/":
			fmt.Fprint(writer, `{"models":[{"external_key":"locked","can_use":false,"is_default_model":true},{"external_key":"chirp-goose","can_use":true,"is_default_model":true}],"credits":0,"total_credits_left":40}`)
		case "/api/generate/v2/":
			if request.Method != http.MethodPost {
				t.Error("generation must use POST")
			}
			submitted.Add(1)
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["prompt"] != "" || body["gpt_description_prompt"] != "gentle piano" || body["mv"] != "chirp-goose" || body["generation_type"] != "TEXT" || body["make_instrumental"] != true || body["batch_size"] != float64(1) {
				t.Errorf("unexpected simple-generation request: %#v", body)
			}
			fmt.Fprintf(writer, `{"id":"request-id","clips":[{"id":%q,"status":"submitted"}]}`, testClipID)
		case "/api/clip/" + testClipID + "/":
			if polls.Add(1) == 1 {
				fmt.Fprintf(writer, `{"id":%q,"status":"streaming","audio_url":"https://cdn1.suno.ai/stream.mp3"}`, testClipID)
				return
			}
			fmt.Fprintf(writer, `{"id":%q,"status":"complete","title":"Piano","audio_url":"https://cdn1.suno.ai/final.mp3"}`, testClipID)
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server)
	response, err := client.Generate(context.Background(), Request{Prompt: "gentle piano", Instrumental: true, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	clips, err := client.Wait(context.Background(), response.IDs())
	if err != nil || len(clips) != 1 || clips[0].Status != "complete" {
		t.Fatalf("clips %v, error %v", clips, err)
	}
	if submitted.Load() != 1 || polls.Load() != 2 {
		t.Fatal("generation was duplicated or streaming URL was treated as completed")
	}
}

func TestCustomPayloadUsesLyricsAndStyle(t *testing.T) {
	client := &Client{}
	payload, err := client.payload(Request{Lyrics: "[Verse]\nHello", Style: "acoustic folk", Title: "Hello", Model: "chirp-goose", Custom: true, CaptchaToken: "user-verified-token"})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Prompt != "[Verse]\nHello" || payload.Tags != "acoustic folk" || payload.Title != "Hello" || payload.Description != "" || payload.Token != "user-verified-token" || payload.BatchSize != 2 {
		t.Fatalf("unexpected custom payload: %#v", payload)
	}
	if _, err := client.payload(Request{Model: "chirp-goose"}); err == nil {
		t.Fatal("empty song description accepted")
	}
	if _, err := client.payload(Request{Model: "chirp-goose", Custom: true}); err == nil {
		t.Fatal("empty custom lyrics accepted")
	}
}

func TestInstrumentalAndDescriptionNeverBecomeLyrics(t *testing.T) {
	client := &Client{}
	payload, err := client.payload(Request{Prompt: "rainy morning guitar", Style: "acoustic folk", Title: "Morning", Instrumental: true, Model: "v6-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Metadata.CreateMode != "simple" || payload.Prompt != "" || payload.Description != "rainy morning guitar" || !payload.Instrumental || payload.Tags != "" || payload.Title != "" {
		t.Fatalf("a simple instrumental request was turned into custom lyrics: %#v", payload)
	}
	payload, err = client.payload(Request{Prompt: "rainy morning guitar", Style: "ambient", Custom: true, Instrumental: true, Model: "v6-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Prompt != "" || payload.Tags != "ambient" || payload.Description != "" || !payload.Instrumental {
		t.Fatalf("instrumental custom mode reused the description as lyrics: %#v", payload)
	}
	if _, err := client.payload(Request{Prompt: "song description", Custom: true, Model: "v6-mini"}); err == nil {
		t.Fatal("custom vocal mode accepted a description without lyrics")
	}
	if _, err := client.payload(Request{Custom: true, Instrumental: true, Model: "v6-mini"}); err == nil {
		t.Fatal("instrumental custom mode accepted an empty style")
	}
}

func TestEmptyProtocolDefaultsToAndroid(t *testing.T) {
	client, err := NewClient(Config{Token: "test.token"})
	if err != nil {
		t.Fatal(err)
	}
	if client.protocol != "android" {
		t.Fatalf("default protocol is not android: %s", client.protocol)
	}
	if !strings.Contains(client.baseURL.String(), "studio-api-prod.suno.com") {
		t.Fatalf("android default base URL wrong: %s", client.baseURL)
	}
	client, err = NewClient(Config{Token: "test.token", Protocol: "web"})
	if err != nil || client.protocol != "web" || !strings.Contains(client.baseURL.String(), "suno.com/a/api/") {
		t.Fatalf("web fallback protocol unavailable: %v %s", err, client.baseURL)
	}
}

func TestMiniAliasAndRequestedIDsExcludeOtherModels(t *testing.T) {
	client := &Client{}
	payload, err := client.payload(Request{Prompt: "piano", Model: "v6-mini"})
	if err != nil || payload.Model != "chirp-goose" {
		t.Fatalf("v6-mini alias not mapped: %v", err)
	}
	response := Response{RequestedModel: "chirp-goose", Clips: []Clip{{ID: "other", ModelName: "chirp-hawk"}, {ID: "wanted", ModelName: "chirp-goose"}}}
	ids := response.RequestedIDs()
	if len(ids) != 1 || ids[0] != "wanted" {
		t.Fatal("requested model wait included an unrelated model")
	}
}

func TestWaitRecognizesCompletedBareAudioContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, `{"id":%q,"status":"complete","audio_url":"","media_urls":[{"url":"https://cdn1.suno.ai/song.m4a","content_type":"m4a-opus","encoding":"1.0.0"}]}`, testClipID)
	}))
	defer server.Close()
	clips, err := testClient(t, server).Wait(context.Background(), []string{testClipID})
	if err != nil || len(clips) != 1 || clips[0].Status != "complete" {
		t.Fatalf("completed playback media was not recognized: %v", err)
	}
	if (Clip{MediaURLs: []MediaURL{{URL: "https://cdn1.suno.ai/video.mp4", ContentType: "video/mp4"}}}).hasAudio() {
		t.Fatal("video alone was treated as completed song audio")
	}
}

func TestGenerateNeverRetriesPOSTAndRedactsToken(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/c/check" {
			fmt.Fprint(writer, `{"required":false}`)
			return
		}
		posts.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(writer, `{"detail":"failed test.token.value verification-token"}`)
	}))
	defer server.Close()
	_, err := testClient(t, server).Generate(context.Background(), Request{Prompt: "piano", Model: "chirp-goose", CaptchaToken: "verification-token"})
	if err == nil || posts.Load() != 1 {
		t.Fatal("failed generation POST was retried")
	}
	if strings.Contains(err.Error(), "test.token.value") || strings.Contains(err.Error(), "verification-token") {
		t.Fatal("API error leaked bearer or verification token")
	}
}

func TestCaptchaPreflightStopsGenerationWithoutUserVerification(t *testing.T) {
	for _, fixture := range []string{`{"required":true}`, `{}`} {
		var submissions atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/api/c/check" {
				var body map[string]string
				json.NewDecoder(request.Body).Decode(&body)
				if body["ctype"] != "generation" {
					t.Error("wrong preflight operation")
				}
				fmt.Fprint(writer, fixture)
				return
			}
			submissions.Add(1)
		}))
		_, err := testClient(t, server).Generate(context.Background(), Request{Prompt: "piano"})
		server.Close()
		if err == nil || submissions.Load() != 0 {
			t.Fatal("generation bypassed required or malformed verification preflight")
		}
		if fixture == `{"required":true}` && !errors.Is(err, ErrCaptchaRequired) {
			t.Fatalf("missing verification error type: %v", err)
		}
	}
}

func TestAuthenticatedRequestsDoNotFollowRedirects(t *testing.T) {
	var escaped atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { escaped.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, err := testClient(t, source).Get(context.Background(), []string{testClipID})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusTemporaryRedirect || escaped.Load() != 0 {
		t.Fatalf("redirect followed: escaped %d error %v", escaped.Load(), err)
	}
}

func TestWaitReturnsFailedClipsAndHonorsCancellation(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			fmt.Fprintf(writer, `{"id":%q,"status":"error","metadata":{"error_type":"moderation"}}`, testClipID)
		}))
		defer server.Close()
		clips, err := testClient(t, server).Wait(context.Background(), []string{testClipID})
		if !errors.Is(err, ErrGenerationFailed) || len(clips) != 1 {
			t.Fatalf("failed clips not reported: %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			fmt.Fprintf(writer, `{"id":%q,"status":"submitted"}`, testClipID)
		}))
		defer server.Close()
		client := testClient(t, server)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err := client.Wait(ctx, []string{testClipID})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("poll ignored context: %v", err)
		}
	})
}

func TestTokenFileIsReadAgainOnLaterRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.token")
	if err := os.WriteFile(path, []byte("first-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = append(seen, request.Header.Get("Authorization"))
		fmt.Fprintf(writer, `{"id":%q,"status":"complete"}`, testClipID)
	}))
	defer server.Close()
	client := testClient(t, server)
	client.token = ""
	client.tokenFile = path
	if _, err := client.Get(context.Background(), []string{testClipID}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), []string{testClipID}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer first-token" || seen[1] != "Bearer second-token" {
		t.Fatal("later requests used stale token file contents")
	}
}

func TestErrorCategoriesAndDestinationValidation(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
		cause  error
	}{
		{401, `{"detail":"expired"}`, ErrAuthentication},
		{403, `{"detail":"captcha required"}`, ErrCaptchaRequired},
		{402, `{}`, ErrInsufficientCredits},
		{429, `{}`, ErrRateLimited},
	} {
		if !errors.Is(parseAPIError(test.status, []byte(test.body)), test.cause) {
			t.Errorf("unrecognized error category for %d", test.status)
		}
	}
	for _, base := range []string{"https://attacker.invalid/api/", "https://studio-api-prod.suno.com.attacker.invalid/api/", "http://studio-api-prod.suno.com/api/", "https://user:password@studio-api-prod.suno.com/api/"} {
		if _, err := NewClient(Config{BaseURL: base, Token: "must-not-leave"}); err == nil {
			t.Fatal("accepted unsafe credential destination")
		}
	}
}
