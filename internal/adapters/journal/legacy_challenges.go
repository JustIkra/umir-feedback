package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

var errLegacyChallengeCapacity = errors.New("legacy challenge capacity exhausted")
var errLegacyChallengeBinding = errors.New("legacy challenge binding invalid")

const (
	maxChallengeRecordBytes = 8 << 10
	maxChallengeVerifies    = 5
	maxChallengeIssues      = 4
)

type wireChallenge struct {
	ID                string                 `json:"id"`
	RegistrationID    string                 `json:"registrationID"`
	Email             string                 `json:"email"`
	Purpose           string                 `json:"purpose"`
	CreatedAt         time.Time              `json:"createdAt"`
	ExpiresAt         time.Time              `json:"expiresAt"`
	ResendAvailableAt time.Time              `json:"resendAvailableAt"`
	RetainUntil       time.Time              `json:"retainUntil"`
	State             string                 `json:"state"`
	CodeDigest        []byte                 `json:"codeDigest"`
	EncryptedCode     wireSealedValue        `json:"encryptedCode"`
	BadAttempts       int                    `json:"badAttempts"`
	Verifications     []wireCodeVerification `json:"verifications"`
	Issues            []wireCodeIssuance     `json:"issues"`
	ProofDigest       []byte                 `json:"proofDigest"`
	EncryptedProof    wireSealedValue        `json:"encryptedProof"`
	VerifiedAt        time.Time              `json:"verifiedAt"`
	CompletionID      string                 `json:"completionID"`
	CompletedAt       time.Time              `json:"completedAt"`
}

type wireSealedValue struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type wireCodeVerification struct {
	ID       string `json:"id"`
	Digest   []byte `json:"digest"`
	Verified bool   `json:"verified"`
}

type wireCodeIssuance struct {
	ID                string    `json:"id"`
	AcceptedAt        time.Time `json:"acceptedAt"`
	ResendAvailableAt time.Time `json:"resendAvailableAt"`
	Job               wireJob   `json:"job"`
}

func sameChallengeBinding(left, right legacyEmailChallenge) bool {
	return left.ID == right.ID && left.RegistrationID == right.RegistrationID && left.Email == right.Email && left.Purpose == right.Purpose && left.CreatedAt.Equal(right.CreatedAt) && left.ExpiresAt.Equal(right.ExpiresAt)
}

func validateChallengeSnapshot(challenge legacyEmailChallenge) error {
	if err := legacyValidateEmailChallenge(challenge); err != nil {
		return err
	}
	retainUntil := challenge.CreatedAt.Add(24 * time.Hour)
	if challenge.State == legacyChallengeVerified || challenge.State == legacyChallengeCompleted {
		retainUntil = challenge.VerifiedAt.Add(24 * time.Hour)
	}
	if !challenge.RetainUntil.Equal(retainUntil) {
		return errors.New("challenge retention deadline is invalid")
	}
	for _, issue := range challenge.Issues {
		job := issue.Job
		if len(job.ErrorCode) > maxErrorCodeSize || len(job.ProviderID) > maxProviderSize || !domain.ValidErrorCode(job.ErrorCode) || !domain.ValidProviderID(job.ProviderID) {
			return errors.New("challenge issuance job is invalid")
		}
	}
	return nil
}

func encodeChallengeRecord(challenge legacyEmailChallenge) ([]byte, error) {
	line, err := encodeRecord(record{Version: formatVersion, Type: "challenge", Challenge: pointer(toWireChallenge(challenge))})
	if err != nil {
		return nil, err
	}
	if len(line) > maxChallengeRecordBytes {
		return nil, errLegacyChallengeCapacity
	}
	return line, nil
}

func toWireChallenge(challenge legacyEmailChallenge) wireChallenge {
	wire := wireChallenge{
		ID: string(challenge.ID), RegistrationID: string(challenge.RegistrationID), Email: challenge.Email, Purpose: string(challenge.Purpose),
		CreatedAt: challenge.CreatedAt, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: challenge.ResendAvailableAt,
		RetainUntil: challenge.RetainUntil, State: string(challenge.State), CodeDigest: challenge.CodeDigest,
		EncryptedCode: wireSealedValue{Nonce: challenge.EncryptedCode.Nonce, Ciphertext: challenge.EncryptedCode.Ciphertext},
		BadAttempts:   challenge.BadAttempts, ProofDigest: challenge.ProofDigest,
		EncryptedProof: wireSealedValue{Nonce: challenge.EncryptedProof.Nonce, Ciphertext: challenge.EncryptedProof.Ciphertext},
		VerifiedAt:     challenge.VerifiedAt, CompletionID: string(challenge.CompletionID), CompletedAt: challenge.CompletedAt,
	}
	wire.Verifications = make([]wireCodeVerification, len(challenge.Verifications))
	for index, verification := range challenge.Verifications {
		wire.Verifications[index] = wireCodeVerification{ID: string(verification.ID), Digest: verification.Digest, Verified: verification.Verified}
	}
	wire.Issues = make([]wireCodeIssuance, len(challenge.Issues))
	for index, issue := range challenge.Issues {
		wire.Issues[index] = wireCodeIssuance{ID: string(issue.ID), AcceptedAt: issue.AcceptedAt, ResendAvailableAt: issue.ResendAvailableAt, Job: toWireJob(issue.Job)}
	}
	return wire
}

func fromWireChallenge(wire wireChallenge) legacyEmailChallenge {
	challenge := legacyEmailChallenge{
		ID: legacyChallengeID(wire.ID), RegistrationID: legacyRegistrationID(wire.RegistrationID), Email: wire.Email, Purpose: legacyChallengePurpose(wire.Purpose),
		CreatedAt: wire.CreatedAt, ExpiresAt: wire.ExpiresAt, ResendAvailableAt: wire.ResendAvailableAt,
		RetainUntil: wire.RetainUntil, State: legacyChallengeState(wire.State), CodeDigest: wire.CodeDigest,
		EncryptedCode: legacySealedValue{Nonce: wire.EncryptedCode.Nonce, Ciphertext: wire.EncryptedCode.Ciphertext}, BadAttempts: wire.BadAttempts,
		ProofDigest: wire.ProofDigest, EncryptedProof: legacySealedValue{Nonce: wire.EncryptedProof.Nonce, Ciphertext: wire.EncryptedProof.Ciphertext},
		VerifiedAt: wire.VerifiedAt, CompletionID: legacyCompletionID(wire.CompletionID), CompletedAt: wire.CompletedAt,
	}
	challenge.Verifications = make([]legacyCodeVerification, len(wire.Verifications))
	for index, verification := range wire.Verifications {
		challenge.Verifications[index] = legacyCodeVerification{ID: legacyVerificationID(verification.ID), Digest: verification.Digest, Verified: verification.Verified}
	}
	challenge.Issues = make([]legacyCodeIssuance, len(wire.Issues))
	for index, issue := range wire.Issues {
		challenge.Issues[index] = legacyCodeIssuance{ID: legacyIssuanceID(issue.ID), AcceptedAt: issue.AcceptedAt, ResendAvailableAt: issue.ResendAvailableAt, Job: fromWireJob(issue.Job)}
	}
	return challenge
}

func validateChallengeFields(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	keys := []string{"id", "registrationID", "email", "purpose", "createdAt", "expiresAt", "resendAvailableAt", "retainUntil", "state", "codeDigest", "encryptedCode", "badAttempts", "verifications", "issues", "proofDigest", "encryptedProof", "verifiedAt", "completionID", "completedAt"}
	if err := validateObjectKeys(object, keys, keys); err != nil {
		return err
	}
	for _, name := range []string{"encryptedCode", "encryptedProof"} {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(object[name], &value); err != nil {
			return err
		}
		sealedKeys := []string{"nonce", "ciphertext"}
		if err := validateObjectKeys(value, sealedKeys, sealedKeys); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	var verifications []json.RawMessage
	if err := json.Unmarshal(object["verifications"], &verifications); err != nil {
		return err
	}
	if len(verifications) > maxChallengeVerifies {
		return errors.New("challenge has too many verification records")
	}
	for _, raw := range verifications {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		verificationKeys := []string{"id", "digest", "verified"}
		if err := validateObjectKeys(value, verificationKeys, verificationKeys); err != nil {
			return err
		}
	}
	var issues []json.RawMessage
	if err := json.Unmarshal(object["issues"], &issues); err != nil {
		return err
	}
	if len(issues) > maxChallengeIssues {
		return errors.New("challenge has too many issuance records")
	}
	for _, raw := range issues {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		issueKeys := []string{"id", "acceptedAt", "resendAvailableAt", "job"}
		if err := validateObjectKeys(value, issueKeys, issueKeys); err != nil {
			return err
		}
		if err := validateJobFields(value["job"]); err != nil {
			return fmt.Errorf("issuance job: %w", err)
		}
	}
	return nil
}

func sortedChallenges(challenges map[legacyChallengeID]legacyEmailChallenge) []legacyEmailChallenge {
	result := make([]legacyEmailChallenge, 0, len(challenges))
	for _, challenge := range challenges {
		result = append(result, challenge)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func hasCiphertext(value legacySealedValue) bool { return len(value.Ciphertext) != 0 }
