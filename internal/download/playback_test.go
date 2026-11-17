package download

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestPlaybackCTRMatchesNISTVectorAtOffsets(t *testing.T) {
	key := mustHex(t, "2b7e151628aed2a6abf7158809cf4f3c")
	iv := mustHex(t, "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff")
	plaintext := mustHex(t, "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	encrypted := mustHex(t, "874d6191b620e3261bef6864990db6ce9806f66b7970fdff8617187bb9fffdff5ae4df3edbd5d35e5b4f09020db03eab1e031dda2fbe03d1792170a0f3009cee")
	for _, offset := range []int64{0, 1, 15, 16, 17, 31, 32, 47, 63} {
		stream, err := playbackCTR(key, iv, offset)
		if err != nil {
			t.Fatal(err)
		}
		decoded := make([]byte, len(encrypted)-int(offset))
		stream.XORKeyStream(decoded, encrypted[offset:])
		if !bytes.Equal(decoded, plaintext[offset:]) {
			t.Errorf("CTR differs at offset %d", offset)
		}
	}
}

func TestPlaybackCTRCounterCarryAndInvalidInputs(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	for _, iv := range [][]byte{mustHex(t, "0000000000000000000000000000ffff"), bytes.Repeat([]byte{255}, 16)} {
		plain := bytes.Repeat([]byte("continuous playback boundary data"), 200)
		block, _ := aes.NewCipher(key)
		encrypted := make([]byte, len(plain))
		cipher.NewCTR(block, iv).XORKeyStream(encrypted, plain)
		for _, offset := range []int64{15, 16, 17, 255, 256, 257, 4097} {
			stream, err := playbackCTR(key, iv, offset)
			if err != nil {
				t.Fatal(err)
			}
			decoded := make([]byte, len(encrypted)-int(offset))
			stream.XORKeyStream(decoded, encrypted[offset:])
			if !bytes.Equal(decoded, plain[offset:]) {
				t.Errorf("counter carry differs at %d", offset)
			}
		}
	}
	for _, input := range []struct {
		key, iv []byte
		offset  int64
	}{{key, make([]byte, 15), 0}, {key, make([]byte, 16), -1}, {make([]byte, 15), make([]byte, 16), 0}} {
		if _, err := playbackCTR(input.key, input.iv, input.offset); err == nil {
			t.Fatal("invalid CTR input accepted")
		}
	}
}

func wrapField(t *testing.T, material []byte, clipID, token string, nonceByte byte) string {
	t.Helper()
	kek := sha256.Sum256([]byte(token))
	block, err := aes.NewCipher(kek[:])
	if err != nil {
		t.Fatal(err)
	}
	mode, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{nonceByte}, 12)
	wrapped := append(nonce, mode.Seal(nil, nonce, material, []byte(clipID))...)
	return base64.StdEncoding.EncodeToString(wrapped)
}

func TestPlaybackLicenseAuthenticatesClipAndActualSession(t *testing.T) {
	material := bytes.Repeat([]byte{7}, 32)
	wrapped := wrapField(t, material, testID, "the-actual-session", 3)
	kek := sha256.Sum256([]byte("the-actual-session"))
	decoded, err := unwrapPlaybackField(wrapped, testID, kek[:])
	if err != nil || !bytes.Equal(decoded, material) {
		t.Fatalf("failed license unwrap: %v", err)
	}
	if _, err := unwrapPlaybackField(wrapped, "different-clip", kek[:]); err == nil {
		t.Fatal("accepted license for another clip")
	}
	wrongKEK := sha256.Sum256([]byte("another-session"))
	if _, err := unwrapPlaybackField(wrapped, testID, wrongKEK[:]); err == nil {
		t.Fatal("accepted license for another session")
	}
	for _, value := range []string{"not base64", base64.StdEncoding.EncodeToString(make([]byte, 28))} {
		if _, err := unwrapPlaybackField(value, testID, kek[:]); err == nil {
			t.Fatal("accepted malformed wrapped material")
		}
	}
}

func TestNormalPlaybackDownloadUsesLicenseAndKeepsSecretsPrivate(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	iv := bytes.Repeat([]byte{2}, 16)
	plaintext := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{7}, 2000)...)
	block, _ := aes.NewCipher(key)
	encrypted := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(encrypted, plaintext)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "session.token")
	if err := os.WriteFile(tokenFile, []byte("actual-session"), 0600); err != nil {
		t.Fatal(err)
	}
	licenseCalls := 0
	client := testClient(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/clip/" + testID + "/":
			return reply(request, 200, `{"id":"`+testID+`","status":"complete","title":"playback","media_urls":[{"url":"https://d2lwuy8qc234o3.cloudfront.net/1/clip/`+testID+`.m4a?test_signature=private","encoding":"1.0.0","delivery":"progressive"}]}`)
		case "/api/mango/rights":
			licenseCalls++
			var body struct {
				ContentParams struct {
					ContentID   string `json:"content_id"`
					ContentType string `json:"content_type"`
				} `json:"content_params"`
				Version string `json:"version"`
			}
			if request.Method != http.MethodPost || json.NewDecoder(request.Body).Decode(&body) != nil ||
				body.ContentParams.ContentID != testID || body.ContentParams.ContentType != "clip" || body.Version != "1.0.0" {
				t.Fatal("license request differs from APK protocol")
			}
			if request.Header.Get("Authorization") != "Bearer actual-session" {
				t.Fatal("license request lacked the actual session")
			}
			if err := os.WriteFile(tokenFile, []byte("changed-after-request"), 0600); err != nil {
				t.Fatal(err)
			}
			response, _ := json.Marshal(map[string]string{
				"key": wrapField(t, key, testID, "actual-session", 1),
				"iv":  wrapField(t, iv, testID, "actual-session", 2),
			})
			return reply(request, 200, string(response))
		case "/1/clip/" + testID + ".m4a":
			if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
				t.Fatal("CDN received credentials")
			}
			return reply(request, 200, string(encrypted))
		}
		t.Fatalf("unexpected request %s", request.URL.Path)
		return nil, nil
	}, Options{TokenFile: tokenFile})
	result, err := client.Download(context.Background(), "https://suno.com/song/"+testID, directory)
	if err != nil {
		t.Fatal(err)
	}
	if licenseCalls != 1 || result.Track.Source != "playback_media" || result.Track.Format != "m4a" {
		t.Fatalf("unexpected playback result %#v calls=%d", result, licenseCalls)
	}
	contents, err := os.ReadFile(result.Path)
	if err != nil || !bytes.Equal(contents, plaintext) {
		t.Fatalf("download did not match decoded playback: %v", err)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"private", "actual-session", "audio_url", `"key"`, `"iv"`} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("result JSON exposes %s", secret)
		}
	}
}

func TestPlaybackLicenseDenialNeverFetchesMedia(t *testing.T) {
	client := testClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "studio-api-prod.suno.com" || request.URL.Path != "/api/mango/rights" {
			t.Fatal("requested media after denied license")
		}
		return reply(request, http.StatusForbidden, `{"detail":"not playable"}`)
	}, Options{Token: "test-session"})
	_, err := client.DownloadTrack(context.Background(), Track{ID: testID, AudioURL: "https://cdn1.suno.ai/" + testID + ".m4a", Encoding: "1.0.0"}, t.TempDir())
	if err == nil {
		t.Fatal("license denial was ignored")
	}
}

func TestPlaybackReaderStreamsWithoutBlockAlignedReads(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 16)
	iv := bytes.Repeat([]byte{1}, 16)
	plain := bytes.Repeat([]byte("unaligned-reader-data"), 500)
	block, _ := aes.NewCipher(key)
	encrypted := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(encrypted, plain)
	license := &playbackLicense{key: key, iv: iv}
	reader, err := license.reader(bytes.NewReader(encrypted), 0)
	if err != nil {
		t.Fatal(err)
	}
	var decoded bytes.Buffer
	if _, err := io.CopyBuffer(&decoded, reader, make([]byte, 19)); err != nil || !bytes.Equal(decoded.Bytes(), plain) {
		t.Fatalf("streaming decode differs: %v", err)
	}
}
