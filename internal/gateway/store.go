package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
func randomSecret() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func atomicFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".gateway-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if file.Chmod(0600) != nil {
		return errors.New("cannot protect gateway file")
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if file.Sync() != nil || file.Close() != nil {
		return errors.New("cannot flush gateway file")
	}
	return os.Rename(file.Name(), path)
}

func readJSON(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16<<20+1))
	if err != nil || len(data) > 16<<20 {
		return errors.New("gateway metadata exceeds its size limit")
	}
	return json.Unmarshal(data, destination)
}

func (gateway *Gateway) persistLocked() error {
	data, err := json.MarshalIndent(gateway.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(filepath.Join(gateway.dataDir, "metadata.json"), data)
}
func (gateway *Gateway) credentialPath(id string) string {
	return filepath.Join(gateway.dataDir, "accounts", id, "session.cookie")
}
func (gateway *Gateway) authPath(id string) string {
	return filepath.Join(gateway.dataDir, "accounts", id, "auth.cookie")
}

func credentialIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 7 && strings.EqualFold(value[:7], "Cookie:") {
		value = strings.TrimSpace(value[7:])
	}
	for _, part := range strings.Split(value, ";") {
		name, token, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && (name == "__session" || strings.HasPrefix(name, "__session_")) {
			value = token
			if name == "__session" {
				break
			}
		}
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Issuer  string `json:"iss"`
		Subject string `json:"sub"`
		Session string `json:"sid"`
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(payload, &all) != nil {
		return ""
	}
	if _, exists := all["x-ably-capability"]; exists {
		return ""
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer != "https://auth.suno.com" || claims.Subject == "" || claims.Session == "" {
		return ""
	}
	return digest(claims.Issuer + "\x00" + claims.Subject)
}
