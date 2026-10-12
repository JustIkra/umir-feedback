package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/vpn"
)

const (
	proxyUser = "fixture-user"
	proxyPass = "fixture-password"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "pinned Xray CONNECT policy check failed"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func run(xrayPath string) (result error) {
	proxyPort, err := findPort()
	if err != nil {
		return err
	}
	nodeListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, nodeListener.Close()) }()
	var nodeConnections atomic.Int32
	go acceptNodes(nodeListener, &nodeConnections)

	_, nodePortText, err := net.SplitHostPort(nodeListener.Addr().String())
	if err != nil {
		return err
	}
	nodePort, err := strconv.Atoi(nodePortText)
	if err != nil {
		return err
	}
	subscription := fmt.Sprintf("vless://11111111-1111-4111-8111-111111111111@127.0.0.1:%d?encryption=none&security=reality&sni=node.example&fp=chrome&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&type=tcp", nodePort)
	config, err := vpn.BuildConfig([]byte(subscription), vpn.ProxyCredentials{Username: proxyUser, Password: proxyPass}, uint16(proxyPort))
	if err != nil {
		return errors.New("dummy VLESS config rejected")
	}
	configFile, err := os.CreateTemp("/", "vpn-connect-*.json")
	if err != nil {
		return err
	}
	configPath := configFile.Name()
	defer func() { result = errors.Join(result, removeFile(configPath)) }()
	if err := configFile.Chmod(0o600); err != nil {
		return errors.Join(err, configFile.Close())
	}
	if _, err := configFile.Write(config); err != nil {
		return errors.Join(err, configFile.Close())
	}
	if err := configFile.Close(); err != nil {
		return err
	}

	command := exec.Command(xrayPath, "run", "-config", configPath)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		return err
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	defer func() { result = errors.Join(result, stopXray(command, processDone)) }()
	if err := waitForProxy(processDone, proxyPort); err != nil {
		return err
	}

	before := nodeConnections.Load()
	status, connected, err := connectStatus(proxyPort, "api.telegram.org:443", "wrong-user", "wrong-pass")
	if err != nil || !connected || status != 407 || nodeConnections.Load() != before {
		return errors.New("proxy authentication was not enforced")
	}
	for _, target := range []string{
		"example.invalid:443",
		"api.telegram.org.evil:443",
		"149.154.167.220:443",
		"api.telegram.org:80",
		"api.telegram.org:444",
	} {
		before = nodeConnections.Load()
		_, connected, err := connectStatus(proxyPort, target, proxyUser, proxyPass)
		if !connected || err != nil && !errors.Is(err, io.EOF) && !isTimeout(err) {
			return errors.New("proxy rejected an invalid request instead of blackholing it")
		}
		if err := waitForNoNodeIncrease(&nodeConnections, before); err != nil {
			return err
		}
	}
	before = nodeConnections.Load()
	_, connected, err = connectStatus(proxyPort, "api.telegram.org:443", proxyUser, proxyPass)
	if !connected || err != nil && !errors.Is(err, io.EOF) && !isTimeout(err) {
		return errors.New("valid Telegram CONNECT did not reach the proxy")
	}
	if err := waitForNodeIncrease(&nodeConnections, before); err != nil {
		return err
	}
	return nil
}

func findPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	closeErr := listener.Close()
	if err != nil || closeErr != nil {
		return 0, errors.Join(err, closeErr)
	}
	return strconv.Atoi(portText)
}

func acceptNodes(listener net.Listener, count *atomic.Int32) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		count.Add(1)
		if err := connection.Close(); err != nil {
			return
		}
	}
}

func connectStatus(port int, target, username, password string) (status int, connected bool, result error) {
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return 0, false, err
	}
	connected = true
	defer func() { result = errors.Join(result, connection.Close()) }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return 0, connected, err
	}
	auth := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password)) + "\r\n"
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + auth + "\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		return 0, connected, err
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return 0, connected, err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, connected, errors.New("proxy returned a malformed response")
	}
	status, err = strconv.Atoi(fields[1])
	return status, connected, err
}

func waitForProxy(processDone <-chan error, port int) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			return connection.Close()
		}
		select {
		case <-processDone:
			return errors.New("xray exited before opening the CONNECT listener")
		case <-deadline.C:
			return errors.New("xray CONNECT listener did not open")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func waitForNodeIncrease(count *atomic.Int32, previous int32) error {
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if count.Load() > previous {
			return nil
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return errors.New("allowed Telegram CONNECT did not use the configured VLESS node")
		}
	}
}

func waitForNoNodeIncrease(count *atomic.Int32, previous int32) error {
	deadline := time.NewTimer(150 * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if count.Load() != previous {
			return errors.New("blackhole route dialed a VLESS node")
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return nil
		}
	}
}

func isTimeout(err error) bool {
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func stopXray(command *exec.Cmd, done <-chan error) error {
	if command.Process == nil {
		return nil
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		select {
		case <-done:
			return nil
		default:
			return err
		}
	}
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		if err := command.Process.Kill(); err != nil {
			return err
		}
		<-done
		return nil
	}
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
