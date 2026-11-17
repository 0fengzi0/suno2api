package download

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type playbackLicense struct {
	key []byte
	iv  []byte
}

func (license *playbackLicense) clear() {
	clear(license.key)
	clear(license.iv)
}

func (client *Client) playbackLicense(ctx context.Context, clipID, version string) (*playbackLicense, error) {
	if !uuidPattern.MatchString(clipID) || version != "1.0.0" {
		return nil, errors.New("invalid Suno playback license request")
	}
	token, err := client.tokenValue(ctx)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrAuthenticationRequired
	}
	body := struct {
		ContentParams struct {
			ContentID   string `json:"content_id"`
			ContentType string `json:"content_type"`
		} `json:"content_params"`
		Version string `json:"version"`
	}{Version: version}
	body.ContentParams.ContentID = clipID
	body.ContentParams.ContentType = "clip"
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	response, err := client.request(ctx, http.MethodPost, apiBase+"mango/rights", strings.NewReader(string(encoded)), apiRequest, true)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	contents, err := readLimited(response.Body, 64<<10)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &APIError{Operation: "playback license", Status: response.StatusCode, Reason: http.StatusText(response.StatusCode)}
	}
	var wrapped struct {
		Key string `json:"key"`
		IV  string `json:"iv"`
	}
	if json.Unmarshal(contents, &wrapped) != nil || wrapped.Key == "" || wrapped.IV == "" {
		return nil, errors.New("Suno did not return a valid playback license")
	}
	if response.Request == nil {
		return nil, errors.New("cannot bind playback license to the actual authorized request")
	}
	authorization := response.Request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) <= 7 {
		return nil, errors.New("playback license request did not carry the Suno session token")
	}
	actualToken := strings.TrimPrefix(authorization, "Bearer ")
	keyEncryptionKey := sha256.Sum256([]byte(actualToken))
	defer clear(keyEncryptionKey[:])
	key, err := unwrapPlaybackField(wrapped.Key, clipID, keyEncryptionKey[:])
	if err != nil {
		return nil, err
	}
	iv, err := unwrapPlaybackField(wrapped.IV, clipID, keyEncryptionKey[:])
	if err != nil {
		clear(key)
		return nil, err
	}
	if (len(key) != 16 && len(key) != 24 && len(key) != 32) || len(iv) != aes.BlockSize {
		clear(key)
		clear(iv)
		return nil, errors.New("Suno playback license contains invalid AES material")
	}
	return &playbackLicense{key: key, iv: iv}, nil
}

func unwrapPlaybackField(encoded, clipID string, keyEncryptionKey []byte) ([]byte, error) {
	wrapped, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(wrapped) <= 28 {
		return nil, errors.New("Suno playback license has invalid wrapped material")
	}
	block, err := aes.NewCipher(keyEncryptionKey)
	if err != nil {
		return nil, errors.New("cannot initialize playback license cipher")
	}
	mode, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("cannot initialize playback license authentication")
	}
	material, err := mode.Open(nil, wrapped[:12], wrapped[12:], []byte(clipID))
	clear(wrapped)
	if err != nil {
		return nil, errors.New("Suno playback license authentication failed")
	}
	return material, nil
}

func (license *playbackLicense) reader(reader io.Reader, offset int64) (io.Reader, error) {
	stream, err := playbackCTR(license.key, license.iv, offset)
	if err != nil {
		return nil, err
	}
	return &cipher.StreamReader{S: stream, R: reader}, nil
}

func playbackCTR(key, iv []byte, offset int64) (cipher.Stream, error) {
	if len(iv) != aes.BlockSize || offset < 0 {
		return nil, errors.New("invalid Suno playback IV or offset")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid Suno playback AES key")
	}
	counter := append([]byte(nil), iv...)
	carry := uint64(offset / aes.BlockSize)
	for index := len(counter) - 1; index >= 0 && carry != 0; index-- {
		sum := uint64(counter[index]) + (carry & 255)
		counter[index] = byte(sum)
		carry = (carry >> 8) + (sum >> 8)
	}
	stream := cipher.NewCTR(block, counter)
	if skip := int(offset % aes.BlockSize); skip > 0 {
		padding := make([]byte, skip)
		stream.XORKeyStream(padding, padding)
	}
	return stream, nil
}
