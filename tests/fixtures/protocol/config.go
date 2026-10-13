package protocol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type FileRef struct {
	Target string `json:"target"`
	Path   string `json:"path"`
}
type Config struct {
	Version               int                `json:"version"`
	Protocol              string             `json:"protocol"`
	Project               string             `json:"project"`
	Owner                 string             `json:"owner"`
	FixtureID             string             `json:"fixtureID"`
	InitialInstant        string             `json:"initialInstant"`
	ClockGeneration       string             `json:"clockGeneration"`
	ControlCredentialFile FileRef            `json:"controlCredentialFile"`
	CredentialFiles       map[string]FileRef `json:"credentialFiles"`
	ControlSocket         string             `json:"controlSocket"`
	PrivatePort           int                `json:"privatePort"`
	PublicPort            int                `json:"publicPort"`
	Origin                string             `json:"origin"`
	ServiceURLs           map[string]string  `json:"serviceURLs"`
	Delivery              json.RawMessage    `json:"delivery"`
	StateFile             string             `json:"stateFile"`
	Bootstrap             json.RawMessage    `json:"bootstrap"`
}

func UUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func SHA(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func LoadConfig(path string) (Config, string, error) {
	var cfg Config
	data, err := readFile(path, 1<<20, 0600)
	if err != nil {
		return cfg, "", err
	}
	if err := validate("OwnerConfig", data); err != nil {
		return cfg, "", errors.New("invalid owner config")
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, "", err
	}
	if cfg.Owner != "feedback" || cfg.PublicPort != 0 || cfg.PrivatePort != 8080 || cfg.StateFile != "/var/lib/umir-feedback/feedback.jsonl" || string(cfg.Bootstrap) != "null" || cfg.ControlSocket != "/run/umir-fixtures/control.sock" || cfg.ControlCredentialFile != (FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/fixture-feedback"}) {
		return cfg, "", errors.New("invalid feedback binding")
	}
	var delivery struct {
		SMTPHost          string  `json:"smtpHost"`
		SMTPPort          int     `json:"smtpPort"`
		SMTPTLS           string  `json:"smtpTLS"`
		SMTPCA            FileRef `json:"smtpCAFile"`
		NotificationProxy string  `json:"notificationProxy"`
		NotificationCA    FileRef `json:"notificationCAFile"`
	}
	if json.Unmarshal(cfg.Delivery, &delivery) != nil || delivery.SMTPCA != (FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/synthetic-ca.pem"}) || delivery.NotificationCA != (FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/synthetic-ca.pem"}) {
		return cfg, "", errors.New("invalid delivery references")
	}
	keys := []string{"gateway-feedback", "feedback-auth"}
	if len(cfg.CredentialFiles) != len(keys) {
		return cfg, "", errors.New("unexpected credential binding")
	}
	for _, key := range keys {
		if cfg.CredentialFiles[key] != (FileRef{Target: "feedback", Path: "/run/umir-fixtures/secrets/" + key}) {
			return cfg, "", errors.New("invalid credential binding")
		}
	}
	for _, name := range []string{"auth", "gateway", "warehouse", "serving", "feedback"} {
		u, err := url.Parse(cfg.ServiceURLs[name])
		port := "8080"
		if name == "gateway" {
			port = "8082"
		}
		if err != nil || u.Scheme != "http" || u.Host != name+":"+port || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return cfg, "", errors.New("invalid service endpoint")
		}
		cfg.ServiceURLs[name] = strings.TrimSuffix(cfg.ServiceURLs[name], "/")
	}
	origin, err := url.Parse(cfg.Origin)
	if err != nil || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" || origin.Port() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return cfg, "", errors.New("invalid synthetic origin")
	}
	if _, err := NewClock(cfg.FixtureID, cfg.FixtureID, cfg.InitialInstant, cfg.ClockGeneration); err != nil {
		return cfg, "", err
	}
	return cfg, SHA(data), nil
}
func ReadCredential(ref FileRef) (string, error) {
	if ref.Target != "feedback" || filepath.Dir(ref.Path) != "/run/umir-fixtures/secrets" || strings.Contains(filepath.Base(ref.Path), "..") {
		return "", errors.New("credential outside allowlist")
	}
	data, err := readFile(ref.Path, 64, 0600)
	if err != nil {
		return "", err
	}
	if len(data) != 64 {
		return "", errors.New("invalid credential")
	}
	for _, b := range data {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return "", errors.New("invalid credential")
		}
	}
	return string(data), nil
}
func readFile(path string, limit int64, maximumMode os.FileMode) ([]byte, error) {
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		s, err := os.Lstat(dir)
		if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe parent")
		}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("file unavailable")
	}
	s, statErr := f.Stat()
	if statErr != nil {
		closeErr := f.Close()
		return nil, errors.Join(statErr, closeErr)
	}
	stat, ok := s.Sys().(*syscall.Stat_t)
	if !ok || !s.Mode().IsRegular() || s.Mode().Perm()&^maximumMode != 0 || maximumMode == 0600 && s.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
		closeErr := f.Close()
		return nil, errors.Join(errors.New("unsafe file"), closeErr)
	}
	if s.Size() > limit {
		closeErr := f.Close()
		return nil, errors.Join(errors.New("file bound"), closeErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > limit {
		return nil, errors.Join(errors.New("file bound"), readErr, closeErr)
	}
	return data, nil
}
func BinarySHA() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func CheckControlSocket(path string) error {
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		s, err := os.Lstat(dir)
		if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe control parent")
		}
	}
	for _, name := range []string{filepath.Dir(path), path} {
		s, err := os.Lstat(name)
		if err != nil {
			return errors.New("control unavailable")
		}
		stat, ok := s.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
			return errors.New("unsafe control owner")
		}
		if name == path {
			if s.Mode()&os.ModeSocket == 0 || s.Mode().Perm() != 0600 {
				return errors.New("unsafe socket")
			}
		} else if s.Mode().Perm() != 0700 {
			return errors.New("unsafe control directory")
		}
	}
	return nil
}

func ReadSecret(name string, limit int64) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return nil, errors.New("invalid secret name")
	}
	return readFile("/run/umir-fixtures/secrets/"+name, limit, 0600)
}
