package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	stateDir       = "/var/lib/telegram-vpn"
	configPath     = stateDir + "/config.json"
	xrayPath       = "/usr/local/bin/xray"
	defaultRefresh = 24 * time.Hour
	defaultMaxAge  = 48 * time.Hour
)

var moscowLocation = time.FixedZone("Europe/Moscow", 3*60*60)

type vpnProcess struct {
	command *exec.Cmd
	done    chan error
}

func Command(args []string) int {
	if len(args) == 1 && args[0] == "healthcheck" {
		if healthcheck() != nil {
			return 1
		}
		return 0
	}
	if len(args) == 1 && args[0] == "status" {
		if status() != nil {
			return 1
		}
		return 0
	}
	if len(args) != 0 {
		return 2
	}
	if run() != nil {
		return 1
	}
	return 0
}

func run() (returnErr error) {
	if err := secureStateDir(stateDir); err != nil {
		return err
	}
	refresh, err := durationEnv("TELEGRAM_VPN_REFRESH_INTERVAL", defaultRefresh)
	if err != nil {
		return err
	}
	maxAge, err := durationEnv("TELEGRAM_VPN_MAX_LKG_AGE", defaultMaxAge)
	if err != nil || maxAge > 48*time.Hour {
		return errors.New("invalid VPN maximum age")
	}
	listenPort, err := uint16Env("TELEGRAM_VPN_LISTEN_PORT", 8080)
	if err != nil {
		return err
	}
	urlBytes, err := readSecret("/run/secrets/telegram-vpn-subscription-url", 8192)
	if err != nil {
		return err
	}
	usernameBytes, err := readSecret("/run/secrets/telegram-proxy-username", 258)
	if err != nil {
		return err
	}
	passwordBytes, err := readSecret("/run/secrets/telegram-proxy-password", 258)
	if err != nil {
		return err
	}
	credentials, err := parseProxyCredentials(usernameBytes, passwordBytes)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := update(ctx, string(urlBytes), credentials, listenPort, maxAge); err != nil {
		if err := reportUpdateFailure(); err != nil {
			return err
		}
	}

	var child *vpnProcess
	defer func() { returnErr = errors.Join(returnErr, stopChild(child)) }()
	start := func() {
		usable, err := LastKnownGoodFresh(configPath, time.Now().In(moscowLocation), maxAge)
		if err != nil || !usable {
			return
		}
		if validateConfig(ctx, configPath) != nil {
			return
		}
		usable, err = LastKnownGoodFresh(configPath, time.Now().In(moscowLocation), maxAge)
		if err != nil || !usable {
			return
		}
		cmd := exec.Command(xrayPath, "run", "-config", configPath)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if cmd.Start() == nil {
			process := &vpnProcess{command: cmd, done: make(chan error, 1)}
			child = process
			go func() { process.done <- cmd.Wait(); close(process.done) }()
		}
	}
	expiryTimer := time.NewTimer(time.Hour)
	defer expiryTimer.Stop()
	if !expiryTimer.Stop() {
		<-expiryTimer.C
	}
	var expiryChannel <-chan time.Time
	refreshExpiry := func() {
		if !expiryTimer.Stop() {
			select {
			case <-expiryTimer.C:
			default:
			}
		}
		updatedAt, err := configModTime()
		if err != nil {
			expiryChannel = nil
			return
		}
		delay := updatedAt.Add(maxAge).Sub(time.Now().In(moscowLocation))
		if delay < 0 {
			delay = 0
		}
		expiryTimer.Reset(delay)
		expiryChannel = expiryTimer.C
	}
	start()
	refreshExpiry()
	refreshTicker := time.NewTicker(refresh)
	defer refreshTicker.Stop()
	reconcileTicker := time.NewTicker(5 * time.Second)
	defer reconcileTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := stopChild(child); err != nil {
				return err
			}
			child = nil
			return nil
		case <-refreshTicker.C:
			if err := refreshTunnel(configPath, maxAge, func() time.Time { return time.Now().In(moscowLocation) }, func() error {
				if err := stopChild(child); err != nil {
					return err
				}
				child = nil
				return nil
			}, func() error {
				return update(ctx, string(urlBytes), credentials, listenPort, maxAge)
			}, start); err != nil {
				return err
			}
			refreshExpiry()
		case <-expiryChannel:
			expiryChannel = nil
			if err := stopChild(child); err != nil {
				return err
			}
			child = nil
		case <-reconcileTicker.C:
			usable, freshnessErr := LastKnownGoodFresh(configPath, time.Now().In(moscowLocation), maxAge)
			if freshnessErr != nil || !usable {
				if err := stopChild(child); err != nil {
					return err
				}
				child = nil
				refreshExpiry()
				continue
			}
			if child != nil {
				select {
				case <-child.done:
					child = nil
				default:
				}
			}
			if child == nil {
				start()
				refreshExpiry()
			}
		}
	}
}

func refreshTunnel(configPath string, maxAge time.Duration, now func() time.Time, stop func() error, update func() error, start func()) error {
	if err := stop(); err != nil {
		return err
	}
	if err := update(); err != nil {
		if reportErr := reportUpdateFailure(); reportErr != nil {
			return reportErr
		}
	}
	fresh, err := LastKnownGoodFresh(configPath, now(), maxAge)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	start()
	return nil
}

func reportUpdateFailure() error {
	_, err := fmt.Fprintln(os.Stderr, "VPN subscription update failed; checking the bounded cache")
	return err
}

func update(parent context.Context, rawURL string, credentials ProxyCredentials, port uint16, maxAge time.Duration) (result error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	subscription, err := DownloadSubscription(ctx, strings.TrimSpace(rawURL))
	if err != nil {
		return err
	}
	config, err := BuildConfig(subscription, credentials, port)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(stateDir, ".candidate-*.json")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { result = errors.Join(result, removeCandidate(name)) }()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(config); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := validateConfig(ctx, name); err != nil {
		return errors.New("VPN config validation failed")
	}
	if err := os.Rename(name, configPath); err != nil {
		return err
	}
	directory, err := os.Open(stateDir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	if err := directory.Sync(); err != nil {
		return err
	}
	updatedAt, err := configModTime()
	if err != nil || !LastKnownGoodValid(updatedAt, time.Now().In(moscowLocation), maxAge) {
		return errors.New("VPN config timestamp invalid")
	}
	return nil
}

func validateConfig(parent context.Context, path string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, xrayPath, "run", "-test", "-config", path)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func configModTime() (time.Time, error) {
	info, err := os.Lstat(configPath)
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !currentOwner(info) || !noSymlinkAncestors(configPath) {
		return time.Time{}, errors.New("VPN config permissions invalid")
	}
	return info.ModTime(), nil
}

func removeCandidate(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func secureStateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !currentOwner(info) || !noSymlinkAncestors(path) {
		return errors.New("VPN state directory unavailable")
	}
	return nil
}

func readSecret(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBytes || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("VPN secret unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("VPN secret unavailable")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > maxBytes {
		return nil, errors.New("VPN secret unavailable")
	}
	return data, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < time.Minute {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return parsed, nil
}

func uint16Env(key string, fallback uint16) (uint16, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return uint16(parsed), nil
}

func stopChild(child *vpnProcess) error {
	if child == nil || child.command.Process == nil {
		return nil
	}
	select {
	case <-child.done:
		return nil
	default:
	}
	if err := child.command.Process.Signal(syscall.SIGTERM); err != nil {
		select {
		case <-child.done:
			return nil
		default:
			return err
		}
	}
	select {
	case <-child.done:
		return nil
	case <-time.After(5 * time.Second):
		if err := child.command.Process.Kill(); err != nil {
			select {
			case <-child.done:
				return nil
			default:
				return err
			}
		}
		<-child.done
		return nil
	}
}

func healthcheck() error {
	port, err := uint16Env("TELEGRAM_VPN_LISTEN_PORT", 8080)
	if err != nil {
		return err
	}
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}

func status() error {
	maxAge, err := durationEnv("TELEGRAM_VPN_MAX_LKG_AGE", defaultMaxAge)
	if err != nil || maxAge > 48*time.Hour {
		return errors.New("invalid VPN maximum age")
	}
	updatedAt, err := configModTime()
	if err != nil {
		if _, printErr := fmt.Fprintln(os.Stdout, "state=unavailable"); printErr != nil {
			return printErr
		}
		return err
	}
	updatedAt = updatedAt.In(moscowLocation)
	expiresAt := updatedAt.Add(maxAge)
	if LastKnownGoodValid(updatedAt, time.Now().In(moscowLocation), maxAge) {
		if _, err := fmt.Fprintf(os.Stdout, "state=fresh updated_at=%s expires_at=%s\n", updatedAt.Format(time.RFC3339), expiresAt.Format(time.RFC3339)); err != nil {
			return err
		}
		return nil
	}
	if _, err := fmt.Fprintf(os.Stdout, "state=expired updated_at=%s expires_at=%s\n", updatedAt.Format(time.RFC3339), expiresAt.Format(time.RFC3339)); err != nil {
		return err
	}
	return errors.New("VPN config is expired")
}
