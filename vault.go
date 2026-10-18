package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Vault hält Zugangsdaten AES-GCM-verschlüsselt in DATA_DIR/credentials.json.
// Der Schlüssel liegt getrennt in DATA_DIR/secret.key (0600). Wer beides hat,
// kann entschlüsseln – es schützt vor versehentlich weitergegebenen Backups
// der JSON-Datei, nicht vor root auf dem Host.
type Vault struct {
	mu   sync.Mutex
	path string
	aead cipher.AEAD
	data map[string]string // Schlüssel -> base64(nonce|ciphertext)
}

// Cred sind Zugangsdaten für ein Gerät (oder die Standard-Zugangsdaten).
type Cred struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

const defaultCredKey = "default"

func openVault(dir string) (*Vault, error) {
	keyPath := filepath.Join(dir, "secret.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("secret.key hat nicht 32 Byte")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	v := &Vault{path: filepath.Join(dir, "credentials.json"), aead: aead, data: map[string]string{}}
	if b, err := os.ReadFile(v.path); err == nil {
		if err := json.Unmarshal(b, &v.data); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (v *Vault) Get(key string) (Cred, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	enc, ok := v.data[key]
	if !ok {
		return Cred{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < v.aead.NonceSize() {
		return Cred{}, false
	}
	ns := v.aead.NonceSize()
	// Der Schlüsselname ist Additional Data: ein Eintrag lässt sich nicht
	// unbemerkt einem anderen Gerät unterschieben.
	plain, err := v.aead.Open(nil, raw[:ns], raw[ns:], []byte(key))
	if err != nil {
		return Cred{}, false
	}
	var c Cred
	if json.Unmarshal(plain, &c) != nil {
		return Cred{}, false
	}
	return c, true
}

func (v *Vault) Set(key string, c Cred) error {
	plain, _ := json.Marshal(c)
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(v.aead.Seal(nonce, nonce, plain, []byte(key)))
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data[key] = enc
	return v.saveLocked()
}

func (v *Vault) Delete(key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.data, key)
	return v.saveLocked()
}

func (v *Vault) Has(key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.data[key]
	return ok
}

func (v *Vault) saveLocked() error {
	b, _ := json.MarshalIndent(v.data, "", "  ")
	return writeFileAtomic(v.path, b, 0o600)
}

// For liefert die Zugangsdaten für ein Gerät: eigene, sonst die Standard-
// Zugangsdaten, sonst admin ohne Passwort (Werkszustand älterer Geräte).
func (v *Vault) For(id string) (Cred, string) {
	if c, ok := v.Get("dev:" + id); ok {
		return c, "device"
	}
	if c, ok := v.Get(defaultCredKey); ok {
		return c, "default"
	}
	return Cred{User: "admin"}, "factory"
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
