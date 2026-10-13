package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func Call(ctx context.Context, cfg Config, body []byte) ([]byte, int, error) {
	if err := validateEnvelope(body); err != nil {
		return nil, 0, errors.New("invalid control request")
	}
	var inputRequest Request
	if json.Unmarshal(body, &inputRequest) != nil || validate(operationSchema(inputRequest.Operation, "request"), body) != nil {
		return nil, 0, errors.New("invalid control operation")
	}
	if err := CheckControlSocket(cfg.ControlSocket); err != nil {
		return nil, 0, err
	}
	credential, err := ReadCredential(cfg.ControlCredentialFile)
	if err != nil {
		return nil, 0, errors.New("control credential unavailable")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", cfg.ControlSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://fixture"+ControlPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, errors.New("control unavailable")
	}
	result, readErr := io.ReadAll(io.LimitReader(response.Body, MaxResponse+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(result) > MaxResponse {
		return nil, 0, errors.New("control response unavailable")
	}
	var envelope Response
	var input Request
	if json.Unmarshal(body, &input) != nil || json.Unmarshal(result, &envelope) != nil {
		return nil, 0, errors.New("invalid control response")
	}
	schemaName := "Error"
	if envelope.Error == nil {
		schemaName = operationSchema(envelope.Operation, "response")
	}
	if validate(schemaName, result) != nil || envelope.RequestID != input.RequestID || envelope.Project != cfg.Project || envelope.Operation != input.Operation || envelope.Error == nil && response.StatusCode != 200 || envelope.Error != nil && response.StatusCode != statusFor(envelope.Error.Code) {
		return nil, 0, errors.New("invalid control response")
	}
	return result, response.StatusCode, nil
}
func operationSchema(operation, suffix string) string {
	var output []byte
	for _, ch := range operation {
		if ch == '.' {
			output = append(output, '_')
		} else {
			output = append(output, byte(ch))
		}
	}
	return string(output) + "_" + suffix
}
func RequestFromStdin() error {
	cfg, _, err := LoadConfig(ConfigPath)
	if err != nil {
		return errors.New("invalid control configuration")
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, MaxRequest+1))
	if err != nil || len(body) > MaxRequest {
		return errors.New("invalid control input")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, _, err := Call(ctx, cfg, body)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(result, '\n'))
	return err
}
func Healthcheck() error {
	cfg, _, err := LoadConfig(ConfigPath)
	if err != nil {
		return err
	}
	id, err := UUID()
	if err != nil {
		return err
	}
	body, err := json.Marshal(Request{Version: 1, Project: cfg.Project, RequestID: id, Operation: "fixture.ready", Data: json.RawMessage(`{}`)})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	data, status, err := Call(ctx, cfg, body)
	if err != nil || status != 200 {
		return errors.New("fixture is not ready")
	}
	var response struct {
		Data ReadyState `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil || !response.Data.Ready {
		return errors.New("fixture is not ready")
	}
	return nil
}
