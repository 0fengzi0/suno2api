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

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(path, data)
}

func (gateway *Gateway) accountDir(id string) string {
	return filepath.Join(gateway.dataDir, "accounts", id)
}
func (gateway *Gateway) credentialPath(id string) string {
	return filepath.Join(gateway.accountDir(id), "session.cookie")
}
func (gateway *Gateway) authPath(id string) string {
	return filepath.Join(gateway.accountDir(id), "auth.cookie")
}
func (gateway *Gateway) accountRecordPath(id string) string {
	return filepath.Join(gateway.accountDir(id), "account.json")
}
func (gateway *Gateway) keyPath(id string) string {
	return filepath.Join(gateway.dataDir, "keys", id+".json")
}
func (gateway *Gateway) jobPath(id string) string {
	return filepath.Join(gateway.dataDir, "jobs", id+".json")
}
func (gateway *Gateway) fileRecordPath(id string) string {
	return filepath.Join(gateway.dataDir, "files", id+".json")
}

func (gateway *Gateway) persistAccountLocked(id string) error {
	record := gateway.state.Accounts[id]
	if record == nil {
		return nil
	}
	if err := os.MkdirAll(gateway.accountDir(id), 0700); err != nil {
		return err
	}
	return writeJSONFile(gateway.accountRecordPath(id), record)
}
func (gateway *Gateway) persistKeyLocked(id string) error {
	record := gateway.state.Keys[id]
	if record == nil {
		return nil
	}
	return writeJSONFile(gateway.keyPath(id), record)
}
func (gateway *Gateway) persistJobLocked(id string) error {
	job := gateway.state.Jobs[id]
	if job == nil {
		return nil
	}
	return writeJSONFile(gateway.jobPath(id), job)
}
func (gateway *Gateway) persistFileLocked(id string) error {
	record := gateway.state.Files[id]
	if record == nil {
		return nil
	}
	return writeJSONFile(gateway.fileRecordPath(id), record)
}

// persistLocked writes every record to its own file. Account sessions stay in
// per-account session.cookie/auth.cookie files, so the gateway needs no database.
func (gateway *Gateway) persistLocked() error {
	for id := range gateway.state.Accounts {
		if err := gateway.persistAccountLocked(id); err != nil {
			return err
		}
	}
	for id := range gateway.state.Keys {
		if err := gateway.persistKeyLocked(id); err != nil {
			return err
		}
	}
	for id := range gateway.state.Jobs {
		if err := gateway.persistJobLocked(id); err != nil {
			return err
		}
	}
	for id := range gateway.state.Files {
		if err := gateway.persistFileLocked(id); err != nil {
			return err
		}
	}
	return nil
}

func readRecordDir(directory, suffix string, visit func(name string, data []byte) error) error {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := visit(strings.TrimSuffix(entry.Name(), suffix), data); err != nil {
			return err
		}
	}
	return nil
}

// loadStateLocked reads per-record files. A legacy metadata.json is migrated
// once, then renamed so it cannot overwrite newer records on the next start.
func (gateway *Gateway) loadStateLocked() error {
	legacyPath := filepath.Join(gateway.dataDir, "metadata.json")
	if _, err := os.Stat(legacyPath); err == nil {
		var legacy persistent
		if err := readJSON(legacyPath, &legacy); err != nil {
			return errors.New("gateway metadata could not be loaded")
		}
		if legacy.Version != 1 {
			return errors.New("unsupported gateway metadata format")
		}
		gateway.state = normalizePersistent(legacy)
		if err := gateway.persistLocked(); err != nil {
			return err
		}
		if err := os.Rename(legacyPath, legacyPath+".migrated"); err != nil {
			return errors.New("legacy gateway metadata could not be retired after migration")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return errors.New("gateway metadata could not be loaded")
	}

	if err := readRecordDir(filepath.Join(gateway.dataDir, "keys"), ".json", func(name string, data []byte) error {
		var record keyRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.ID == "" {
			record.ID = name
		}
		gateway.state.Keys[record.ID] = &record
		return nil
	}); err != nil {
		return errors.New("gateway API key records could not be loaded")
	}
	if err := readRecordDir(filepath.Join(gateway.dataDir, "jobs"), ".json", func(name string, data []byte) error {
		var job JobView
		if err := json.Unmarshal(data, &job); err != nil {
			return err
		}
		if job.ID == "" {
			job.ID = name
		}
		gateway.state.Jobs[job.ID] = &job
		return nil
	}); err != nil {
		return errors.New("gateway job records could not be loaded")
	}
	if err := readRecordDir(filepath.Join(gateway.dataDir, "files"), ".json", func(name string, data []byte) error {
		var record fileRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.ID == "" {
			record.ID = name
		}
		gateway.state.Files[record.ID] = &record
		return nil
	}); err != nil {
		return errors.New("gateway file records could not be loaded")
	}
	accounts, err := os.ReadDir(filepath.Join(gateway.dataDir, "accounts"))
	if err != nil && !os.IsNotExist(err) {
		return errors.New("gateway account records could not be loaded")
	}
	for _, entry := range accounts {
		if !entry.IsDir() {
			continue
		}
		var record accountRecord
		if err := readJSON(filepath.Join(gateway.dataDir, "accounts", entry.Name(), "account.json"), &record); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errors.New("gateway account records could not be loaded")
		}
		if record.ID == "" {
			record.ID = entry.Name()
		}
		gateway.state.Accounts[record.ID] = &record
	}
	return nil
}

func normalizePersistent(state persistent) persistent {
	if state.Accounts == nil {
		state.Accounts = map[string]*accountRecord{}
	}
	if state.Jobs == nil {
		state.Jobs = map[string]*JobView{}
	}
	if state.Keys == nil {
		state.Keys = map[string]*keyRecord{}
	}
	if state.Files == nil {
		state.Files = map[string]*fileRecord{}
	}
	state.Version = 1
	return state
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
