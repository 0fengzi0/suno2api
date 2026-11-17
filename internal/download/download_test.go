package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testID = "c1a16429-f3fe-4d99-bdcf-61ee13ed6f00"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func reply(request *http.Request, status int, contents string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(contents)),
		Header: make(http.Header), ContentLength: int64(len(contents)), Request: request}, nil
}

func testClient(handler roundTripFunc, options Options) *Client {
	options.HTTPClient = &http.Client{Transport: handler}
	return NewClient(options)
}

func TestShareURLRejectsUnexpectedDestinations(t *testing.T) {
	for _, address := range []string{
		"http://suno.com/song/" + testID,
		"https://suno.com.evil.example/song/" + testID,
		"https://suno.com@127.0.0.1/song/" + testID,
		"https://127.0.0.1/song/" + testID,
		"https://suno.com:8443/song/" + testID,
		"https://suno.com/song/../../secret",
		"https://suno.com/playlist/" + testID,
	} {
		if _, _, err := ParseShareURL(address); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
	for _, host := range []string{"suno.com", "www.suno.com", "suno.ai", "www.suno.ai"} {
		id, _, err := ParseShareURL("https://" + host + "/song/" + testID + "?source=share")
		if err != nil || id != testID {
			t.Errorf("valid share rejected: %v", err)
		}
	}
}

func TestShortcodeDownloadAndNoCredentialAtCDN(t *testing.T) {
	directory := t.TempDir()
	audio := "ID3" + strings.Repeat("a", 1000)
	client := testClient(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/share/code/Abc_12":
			return reply(request, 200, `{"success":true,"content_id":"`+testID+`","content_type":"song"}`)
		case "/api/clip/" + testID + "/":
			return reply(request, 200, `{"id":"`+testID+`","status":"complete","title":" A:/song? ","audio_url":"https://cdn1.suno.ai/`+testID+`.mp3"}`)
		case "/" + testID + ".mp3":
			if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
				t.Fatal("credential reached CDN")
			}
			return reply(request, 200, audio)
		}
		t.Fatalf("unexpected request %s", request.URL)
		return nil, nil
	}, Options{Token: "test-token"})
	result, err := client.Download(context.Background(), "https://suno.com/s/Abc_12", directory)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != int64(len(audio)) || result.Track.Format != "mp3" || len(result.SHA256) != 64 {
		t.Fatalf("unexpected result %#v", result)
	}
	if filepath.Base(result.Path) != "A__song_-"+testID+".mp3" {
		t.Errorf("unsafe or unexpected filename %s", result.Path)
	}
	contents, err := os.ReadFile(result.Path)
	if err != nil || string(contents) != audio {
		t.Fatalf("download differs: %v", err)
	}
	if _, err := client.Download(context.Background(), "https://suno.com/s/Abc_12", result.Path); err == nil {
		t.Fatal("overwrote existing file")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatal("temporary download files leaked")
	}
}

func TestAuthorizedDownloadUsesObservedPayloadOnce(t *testing.T) {
	gets, posts := 0, 0
	client := testClient(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer secret-test" {
			t.Fatal("missing API credential")
		}
		switch request.URL.Path {
		case "/api/clip/" + testID + "/":
			return reply(request, 200, `{"id":"`+testID+`","status":"complete","audio_url":"https://studio-api.prod.suno.com/api/forbidden"}`)
		case "/api/download/clip/" + testID:
			gets++
			if request.URL.Query().Get("format") != "mp3" {
				t.Fatal("missing format")
			}
			if gets == 1 {
				return reply(request, 200, `{"ok":false,"reason":"not_authorized"}`)
			}
			return reply(request, 200, `{"ok":true,"download_url":"https://cdn1.suno.ai/`+testID+`.mp3"}`)
		case "/api/download/authorize":
			posts++
			var body map[string]string
			if request.Method != http.MethodPost || json.NewDecoder(request.Body).Decode(&body) != nil ||
				body["item_id"] != testID || body["item_type"] != "clip" || body["surface"] != "share" {
				t.Fatal("authorization payload differs from APK")
			}
			return reply(request, 200, `{"ok":true,"already_unlocked":false,"credit_deducted":true}`)
		}
		t.Fatalf("unexpected request %s", request.URL)
		return nil, nil
	}, Options{Token: "secret-test", AllowAuthorize: true})
	track, err := client.Resolve(context.Background(), "https://suno.com/song/"+testID)
	if err != nil || posts != 1 || gets != 2 || !track.CreditDeducted || track.Source != "authorized_download" {
		t.Fatalf("authorization failed: %#v %v posts=%d gets=%d", track, err, posts, gets)
	}
}

func TestDownloadAuthorizationIsOptIn(t *testing.T) {
	client := testClient(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatal("unexpected mutating request")
		}
		if strings.Contains(request.URL.Path, "/download/") {
			return reply(request, 200, `{"ok":false,"reason":"not_authorized"}`)
		}
		return reply(request, 200, `{"id":"`+testID+`","status":"complete","audio_url":"https://studio-api.prod.suno.com/api/forbidden"}`)
	}, Options{Token: "test-token"})
	_, err := client.Resolve(context.Background(), "https://suno.com/song/"+testID)
	if !errors.Is(err, ErrAuthorizationRequired) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestEncodedPlaybackRequiresNormalLicense(t *testing.T) {
	client := testClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "studio-api-prod.suno.com" {
			t.Fatal("tried to download encoded media")
		}
		return reply(request, 200, `{"id":"`+testID+`","status":"complete","media_urls":[{"url":"https://d2lwuy8qc234o3.cloudfront.net/1/clip/`+testID+`.m4a","encoding":"1.0.0"}]}`)
	}, Options{})
	_, err := client.Download(context.Background(), "https://suno.com/song/"+testID, t.TempDir())
	if !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestHTMLOrOversizeNeverPublishesFile(t *testing.T) {
	for _, contents := range []string{"<html>access denied</html>", "ID3" + strings.Repeat("a", 2000)} {
		t.Run(contents[:3], func(t *testing.T) {
			directory := t.TempDir()
			client := testClient(func(request *http.Request) (*http.Response, error) {
				return reply(request, 200, contents)
			}, Options{MaxBytes: 1024})
			_, err := client.DownloadTrack(context.Background(), Track{ID: testID, Title: "song", AudioURL: "https://cdn1.suno.ai/" + testID + ".mp3"}, directory)
			if err == nil {
				t.Fatal("accepted invalid audio")
			}
			entries, _ := os.ReadDir(directory)
			if len(entries) != 0 {
				t.Fatal("invalid audio was published")
			}
		})
	}
}

func TestM4AKeepsCorrectFileExtension(t *testing.T) {
	audio := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}, bytes.Repeat([]byte{1}, 1000)...)
	client := testClient(func(request *http.Request) (*http.Response, error) {
		return reply(request, 200, string(audio))
	}, Options{})
	track := Track{ID: testID, Title: "song", AudioURL: "https://cdn1.suno.ai/" + testID + ".m4a"}
	directory := t.TempDir()
	if _, err := client.DownloadTrack(context.Background(), track, filepath.Join(directory, "song.mp3")); err == nil {
		t.Fatal("mislabeled m4a as mp3")
	}
	result, err := client.DownloadTrack(context.Background(), track, directory)
	if err != nil || result.Track.Format != "m4a" || filepath.Ext(result.Path) != ".m4a" {
		t.Fatalf("unexpected M4A output %#v %v", result, err)
	}
}

func TestRedirectDoesNotEscapeSuno(t *testing.T) {
	count := 0
	client := testClient(func(request *http.Request) (*http.Response, error) {
		count++
		response, _ := reply(request, 302, "")
		response.Header.Set("Location", "https://evil.example/private")
		return response, nil
	}, Options{})
	_, _, err := client.readPage(context.Background(), "https://suno.com/s/abc")
	if err == nil || count != 1 {
		t.Fatal("followed unsafe redirect")
	}
}

func TestPageFallbackFindsMatchingClipOnly(t *testing.T) {
	clip := `{"id":"` + testID + `","status":"complete","title":"matching song","audio_url":"https://cdn1.suno.ai/` + testID + `.mp3"}`
	flight, _ := json.Marshal(`11:{"props":{"clip":` + clip + `}}`)
	page := []byte(`<script>self.__next_f.push([1,` + string(flight) + `])</script>`)
	got := parsePageClip(page, testID)
	if got.ID != testID || got.Title != "matching song" {
		t.Fatalf("did not resolve Next payload %#v", got)
	}
}

func TestSessionTokenProviderCoversMetadataButNeverCDN(t *testing.T) {
	providerCalls := 0
	client := testClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == "studio-api-prod.suno.com" {
			if request.Header.Get("Authorization") != "Bearer fresh-session" {
				t.Fatal("metadata did not use refreshed token")
			}
			return reply(request, 200, `{"id":"`+testID+`","status":"complete","audio_url":"https://cdn1.suno.ai/`+testID+`.mp3"}`)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("CDN received refreshed session token")
		}
		return reply(request, 200, "ID3"+strings.Repeat("a", 600))
	}, Options{Token: "stale-token", TokenProvider: func(ctx context.Context) (string, error) {
		providerCalls++
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "fresh-session", nil
	}})
	if _, err := client.Download(context.Background(), "https://suno.com/song/"+testID, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if providerCalls != 1 {
		t.Fatalf("provider ran %d times; public CDN must not invoke session refresh", providerCalls)
	}
}
