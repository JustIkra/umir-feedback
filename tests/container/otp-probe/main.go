package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mechanisms "github.com/JustIkra/umir-feedback/internal/adapters/runtime"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

const (
	baseURL        = "http://127.0.0.1:8084"
	challengePath  = "/internal/v1/email-challenges"
	registrationID = "123e4567-e89b-42d3-a456-426614174000"
	verifyID       = "123e4567-e89b-42d3-a456-426614174001"
	completeID     = "123e4567-e89b-42d3-a456-426614174002"
	statePath      = "/var/lib/otp-probe/state.json"
	journalPath    = "/var/lib/umir-feedback/journal.jsonl"
)

type state struct {
	ChallengeID string `json:"challengeID"`
	Code        string `json:"code"`
	ProofID     string `json:"proofID"`
	Verified    []byte `json:"verified"`
}

type receipt struct {
	ChallengeID string `json:"challengeID"`
}

type proof struct {
	ProofID string `json:"proofID"`
}

type challengeRecord struct {
	ID            string `json:"id"`
	Registration  string `json:"registrationID"`
	Purpose       string `json:"purpose"`
	EncryptedCode struct {
		Nonce      []byte `json:"nonce"`
		Ciphertext []byte `json:"ciphertext"`
	} `json:"encryptedCode"`
	Issues []json.RawMessage `json:"issues"`
}

type journalRecord struct {
	Type      string           `json:"type"`
	Challenge *challengeRecord `json:"challenge"`
}

func main() {
	if len(os.Args) == 1 {
		if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
			if phaseErr := firstPhase(); phaseErr != nil {
				fail(phaseErr.Error())
			}
			return
		} else if err == nil {
			if phaseErr := secondPhase(); phaseErr != nil {
				fail(phaseErr.Error())
			}
			return
		}
		fail("probe recovery state could not be inspected")
	}
	if len(os.Args) != 2 {
		fail("invalid probe mode")
	}
	var err error
	switch os.Args[1] {
	case "first":
		err = firstPhase()
	case "second":
		err = secondPhase()
	case "check-logs":
		err = checkLogs()
	default:
		err = errors.New("invalid probe mode")
	}
	if err != nil {
		fail(err.Error())
	}
}

func firstPhase() error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	if err := waitReady(ctx, client); err != nil {
		return err
	}
	unauthorized, err := request(ctx, client, http.MethodPost, challengePath, nil, "")
	if err != nil || unauthorized.status != http.StatusUnauthorized {
		return errors.New("unauthenticated begin was not rejected")
	}
	token := fixtureToken()
	beginBody := map[string]string{
		"registrationID": registrationID,
		"purpose":        "operator-registration",
		"email":          "otp-recipient@invalid.test",
	}
	first, err := postJSON(ctx, client, challengePath, beginBody, token)
	if err != nil || first.status != http.StatusAccepted {
		return errors.New("authenticated begin did not return 202")
	}
	replay, err := postJSON(ctx, client, challengePath, beginBody, token)
	if err != nil || replay.status != http.StatusAccepted || !bytes.Equal(first.body, replay.body) {
		return errors.New("begin replay changed its receipt")
	}
	var created receipt
	if err := json.Unmarshal(first.body, &created); err != nil || created.ChallengeID == "" {
		return errors.New("begin response did not contain a challenge")
	}
	challenge, journalBytes, err := waitForChallenge(ctx, created.ChallengeID)
	if err != nil {
		return err
	}
	if challenge.Registration != registrationID || challenge.Purpose != "operator-registration" || len(challenge.Issues) != 1 || len(challenge.EncryptedCode.Ciphertext) == 0 {
		return errors.New("journal did not contain one encrypted initial issuance")
	}
	if err := verifyJournalPermissions(); err != nil {
		return err
	}
	ciphertextText := base64.StdEncoding.EncodeToString(challenge.EncryptedCode.Ciphertext)
	if !bytes.Contains(journalBytes, []byte(ciphertextText)) {
		return errors.New("journal did not persist the encrypted code")
	}
	crypto, err := mechanisms.NewChallengeCrypto(fixturePepper(), fixtureEncryptionKey())
	if err != nil {
		return errors.New("fixture cryptography could not be initialized")
	}
	code, err := crypto.Open(created.ChallengeID+":operator-registration:code", domain.SealedValue{Nonce: challenge.EncryptedCode.Nonce, Ciphertext: challenge.EncryptedCode.Ciphertext})
	if err != nil || len(code) != 6 {
		return errors.New("journal code ciphertext could not be authenticated")
	}
	if _, err := os.Stat(journalPath); err != nil {
		return errors.New("application journal unavailable")
	}
	issuanceBody := map[string]string{
		"registrationID": registrationID,
		"purpose":        "operator-registration",
		"issuanceID":     "123e4567-e89b-42d3-a456-426614174003",
	}
	resend, err := postJSON(ctx, client, challengePath+"/"+created.ChallengeID+"/resend", issuanceBody, token)
	if err != nil || resend.status != http.StatusTooManyRequests {
		return errors.New("immediate resend was not rate limited")
	}
	wrongCode := "000000"
	if code == wrongCode {
		wrongCode = "000001"
	}
	wrongBody := map[string]string{
		"registrationID": registrationID,
		"purpose":        "operator-registration",
		"verificationID": verifyID,
		"code":           wrongCode,
	}
	wrong, err := postJSON(ctx, client, challengePath+"/"+created.ChallengeID+"/verify", wrongBody, token)
	if err != nil || wrong.status != http.StatusBadRequest {
		return errors.New("wrong code was not rejected")
	}
	wrongReplay, err := postJSON(ctx, client, challengePath+"/"+created.ChallengeID+"/verify", wrongBody, token)
	if err != nil || wrongReplay.status != http.StatusBadRequest || errorCode(wrong.body) != "InvalidCode" || errorCode(wrongReplay.body) != "InvalidCode" {
		return errors.New("wrong verification replay did not return the same fixed error")
	}
	wrongBody["code"] = differentCode(wrongCode)
	conflict, err := postJSON(ctx, client, challengePath+"/"+created.ChallengeID+"/verify", wrongBody, token)
	if err != nil || conflict.status != http.StatusConflict || errorCode(conflict.body) != "VerificationIdentityConflict" {
		return errors.New("verification ID conflict was not rejected")
	}
	correctBody := map[string]string{
		"registrationID": registrationID,
		"purpose":        "operator-registration",
		"verificationID": "123e4567-e89b-42d3-a456-426614174004",
		"code":           code,
	}
	verified, err := postJSON(ctx, client, challengePath+"/"+created.ChallengeID+"/verify", correctBody, token)
	if err != nil || verified.status != http.StatusOK {
		return errors.New("correct code was not accepted")
	}
	var issued proof
	if err := json.Unmarshal(verified.body, &issued); err != nil || issued.ProofID == "" {
		return errors.New("verification response did not contain a proof")
	}
	finalBytes, err := os.ReadFile(journalPath)
	if err != nil || bytes.Contains(finalBytes, []byte(ciphertextText)) || bytes.Contains(finalBytes, []byte(code)) || bytes.Contains(finalBytes, []byte(issued.ProofID)) {
		return errors.New("journal retained code ciphertext or plaintext secret")
	}
	if bytes.Contains(journalBytes, []byte(issued.ProofID)) {
		return errors.New("proof appeared in journal before completion")
	}
	if err := saveState(state{ChallengeID: created.ChallengeID, Code: code, ProofID: issued.ProofID, Verified: append([]byte(nil), verified.body...)}); err != nil {
		return err
	}
	if _, err := fmt.Println("OTP acceptance phase 1 passed: auth, begin replay, encrypted delivery state, resend cooldown, verification replay and conflict"); err != nil {
		return errors.New("probe status output failed")
	}
	return nil
}

func secondPhase() error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	if err := waitReady(ctx, client); err != nil {
		return err
	}
	previous, err := loadState()
	if err != nil {
		return err
	}
	token := fixtureToken()
	verifyBody := map[string]string{
		"registrationID": registrationID,
		"purpose":        "operator-registration",
		"verificationID": "123e4567-e89b-42d3-a456-426614174004",
		"code":           previous.Code,
	}
	verified, err := postJSON(ctx, client, challengePath+"/"+previous.ChallengeID+"/verify", verifyBody, token)
	if err != nil || verified.status != http.StatusOK || !bytes.Equal(verified.body, previous.Verified) {
		return errors.New("successful verification replay changed after restart")
	}
	completeBody := map[string]string{
		"registrationID": registrationID,
		"purpose":        "operator-registration",
		"completionID":   completeID,
		"proofID":        previous.ProofID,
	}
	completed, err := postJSON(ctx, client, challengePath+"/"+previous.ChallengeID+"/complete", completeBody, token)
	if err != nil || completed.status != http.StatusOK {
		return errors.New("completion did not succeed after restart")
	}
	completionReplay, err := postJSON(ctx, client, challengePath+"/"+previous.ChallengeID+"/complete", completeBody, token)
	if err != nil || completionReplay.status != http.StatusOK || !bytes.Equal(completed.body, completionReplay.body) {
		return errors.New("completion replay changed its response")
	}
	challenge, journalBytes, err := waitForChallenge(ctx, previous.ChallengeID)
	if err != nil {
		return err
	}
	if len(challenge.Issues) != 1 || len(challenge.EncryptedCode.Ciphertext) != 0 || bytes.Contains(journalBytes, []byte(previous.Code)) || bytes.Contains(journalBytes, []byte(previous.ProofID)) {
		return errors.New("journal contains duplicate initial issuance or plaintext secret")
	}
	if _, err := fmt.Println("OTP acceptance phase 2 passed: verification proof recovery and exact completion replay after restart"); err != nil {
		return errors.New("probe status output failed")
	}
	return nil
}

func checkLogs() error {
	previous, err := loadState()
	if err != nil {
		return err
	}
	const maxLogBytes = 1 << 20
	logBytes, err := io.ReadAll(io.LimitReader(os.Stdin, maxLogBytes+1))
	if err != nil {
		return errors.New("application logs could not be read")
	}
	if len(logBytes) > maxLogBytes {
		return errors.New("application logs exceeded the inspection limit")
	}
	secrets := [][]byte{
		[]byte(previous.Code), []byte(previous.ProofID),
		[]byte(fixtureToken()), bytes.Repeat([]byte{3}, 32),
		[]byte(base64.StdEncoding.EncodeToString(fixturePepper())), fixturePepper(),
		[]byte(base64.StdEncoding.EncodeToString(fixtureEncryptionKey())), fixtureEncryptionKey(),
		[]byte("fixture-smtp-user"), []byte("fixture-smtp-password"),
		[]byte("feedback@invalid.test"), []byte("operator@invalid.test"),
		[]byte("123456:fixture-telegram-token"), []byte("123456"),
		[]byte("fixture-proxy-user"), []byte("fixture-proxy-password"),
		[]byte("otp-recipient@invalid.test"),
	}
	for _, secret := range secrets {
		if len(secret) > 0 && bytes.Contains(logBytes, secret) {
			return errors.New("application logs contain an OTP or provider secret")
		}
	}
	if _, err := fmt.Println("OTP acceptance log hygiene passed"); err != nil {
		return errors.New("probe status output failed")
	}
	return nil
}

func waitReady(ctx context.Context, client *http.Client) error {
	for {
		health, healthErr := request(ctx, client, http.MethodGet, "/healthz", nil, "")
		ready, readyErr := request(ctx, client, http.MethodGet, "/readyz", nil, "")
		if healthErr == nil && readyErr == nil && health.status == http.StatusOK && ready.status == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("application readiness deadline exceeded")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type response struct {
	status int
	body   []byte
}

func postJSON(ctx context.Context, client *http.Client, path string, body map[string]string, token string) (response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return response{}, errors.New("request encoding failed")
	}
	return request(ctx, client, http.MethodPost, path, encoded, token)
}

func request(ctx context.Context, client *http.Client, method, path string, body []byte, token string) (response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return response{}, errors.New("request construction failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{}, errors.New("application request failed")
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return response{}, errors.New("application response could not be read")
	}
	return response{status: resp.StatusCode, body: data}, nil
}

func waitForChallenge(ctx context.Context, id string) (challengeRecord, []byte, error) {
	for {
		record, raw, err := readChallenge(id)
		if err == nil {
			return record, raw, nil
		}
		select {
		case <-ctx.Done():
			return challengeRecord{}, nil, errors.New("journal challenge deadline exceeded")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func readChallenge(id string) (challengeRecord, []byte, error) {
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return challengeRecord{}, nil, err
	}
	lines := bytes.Split(raw, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	latest := make(map[string]challengeRecord)
	for _, line := range lines {
		var record journalRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return challengeRecord{}, raw, err
		}
		if record.Type == "challenge" && record.Challenge != nil {
			latest[record.Challenge.ID] = *record.Challenge
		}
	}
	if len(latest) != 1 {
		return challengeRecord{}, raw, errors.New("journal challenge count changed")
	}
	challenge, exists := latest[id]
	if !exists {
		return challengeRecord{}, raw, errors.New("journal challenge missing")
	}
	return challenge, raw, nil
}

func saveState(value state) error {
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		return errors.New("probe state directory unavailable")
	}
	directory, err := os.Stat(filepath.Dir(statePath))
	if err != nil || directory.Mode().Perm() != 0700 {
		return errors.New("probe state directory permissions are invalid")
	}
	owner, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 65532 || owner.Gid != 65532 {
		return errors.New("probe state directory owner is invalid")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.New("probe state encoding failed")
	}
	file, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("probe state could not be created")
	}
	_, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("probe state could not be persisted")
	}
	return nil
}

func loadState() (state, error) {
	info, err := os.Stat(statePath)
	if err != nil || info.Mode().Perm() != 0600 {
		return state{}, errors.New("probe recovery state unavailable")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return state{}, errors.New("probe recovery state could not be read")
	}
	var result state
	if err := json.Unmarshal(data, &result); err != nil || result.ChallengeID == "" || result.Code == "" || result.ProofID == "" || len(result.Verified) == 0 {
		return state{}, errors.New("probe recovery state is invalid")
	}
	return result, nil
}

func verifyJournalPermissions() error {
	directory, err := os.Stat(filepath.Dir(journalPath))
	if err != nil || directory.Mode().Perm() != 0700 {
		return errors.New("application state directory permissions are invalid")
	}
	owner, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 65532 || owner.Gid != 65532 {
		return errors.New("application state directory owner is invalid")
	}
	file, err := os.Stat(journalPath)
	if err != nil || file.Mode().Perm() != 0600 {
		return errors.New("application journal permissions are invalid")
	}
	fileOwner, ok := file.Sys().(*syscall.Stat_t)
	if !ok || fileOwner.Uid != 65532 || fileOwner.Gid != 65532 {
		return errors.New("application journal owner is invalid")
	}
	return nil
}

func fixtureToken() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
}

func fixturePepper() []byte { return bytes.Repeat([]byte{1}, 32) }

func fixtureEncryptionKey() []byte { return bytes.Repeat([]byte{2}, 32) }

func differentCode(value string) string {
	if value[0] != '9' {
		return "9" + value[1:]
	}
	return "8" + value[1:]
}

func errorCode(data []byte) string {
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	return body.Code
}

func fail(message string) {
	message = strings.ReplaceAll(message, "\n", " ")
	if _, err := fmt.Fprintln(os.Stderr, "OTP acceptance failed:", message); err != nil {
		os.Exit(1)
	}
	os.Exit(1)
}
