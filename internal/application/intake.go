package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type ValidationError struct {
	Code   string
	Fields map[string]string
}

func (e *ValidationError) Error() string { return e.Code }

type Intake struct {
	repository     Repository
	clock          Clock
	normalizer     Normalizer
	consentVersion string
	fingerprinter  Fingerprinter
}

type plainFingerprinter struct{}

func (plainFingerprinter) Sum(binding string, content []byte) string {
	digest := sha256.Sum256(append([]byte(binding+":"), content...))
	return hex.EncodeToString(digest[:])
}

// WithFingerprinter replaces the default unkeyed SHA-256 fingerprint (tests only).
func (s *Intake) WithFingerprinter(f Fingerprinter) *Intake {
	s.fingerprinter = f
	return s
}

func NewIntake(repository Repository, clock Clock, normalizer Normalizer, consentVersion string) *Intake {
	return &Intake{repository: repository, clock: clock, normalizer: normalizer, consentVersion: consentVersion, fingerprinter: plainFingerprinter{}}
}

func (s *Intake) Submit(ctx context.Context, in domain.Input) (domain.Receipt, bool, error) {
	return s.SubmitScoped(ctx, domain.Anonymous(), in)
}

func (s *Intake) SubmitScoped(ctx context.Context, namespace domain.Namespace, in domain.Input) (domain.Receipt, bool, error) {
	if !namespace.Valid() {
		return domain.Receipt{}, false, &ValidationError{Code: "InvalidRequest"}
	}
	if !domain.ValidID(in.ID) {
		return domain.Receipt{}, false, &ValidationError{Code: "InvalidRequest"}
	}
	in.ID = domain.SubmissionID(strings.ToLower(string(in.ID)))
	now := s.clock.Now()
	if in.Website != "" {
		return domain.Receipt{SubmissionID: in.ID, ReceivedAt: now}, true, nil
	}
	in.Name = s.normalizer.NFC(in.Name)
	normalized, fields := domain.Normalize(in)
	if len(fields) > 0 {
		return domain.Receipt{}, false, &ValidationError{Code: "InvalidFeedback", Fields: fields}
	}
	content, err := json.Marshal(struct {
		Name, Email, Phone, Message, ConsentVersion string
		Accepted                                    bool
	}{normalized.Name, normalized.Email, normalized.Phone, normalized.Message, normalized.ConsentVersion, normalized.Accepted})
	if err != nil {
		return domain.Receipt{}, false, err
	}
	digest := sha256.Sum256(content)
	legacyFingerprint := hex.EncodeToString(digest[:])
	binding := "anonymous"
	if namespace.Kind == "app" {
		binding = string(namespace.Key(""))
	}
	fingerprint := s.fingerprinter.Sum(binding, content)
	var previous domain.Submission
	var exists bool
	if scoped, ok := s.repository.(ScopedRepository); ok {
		previous, exists, err = scoped.LookupScoped(ctx, namespace, in.ID)
	} else if namespace.Kind == "anonymous" {
		previous, exists, err = s.repository.Lookup(ctx, in.ID)
	} else {
		return domain.Receipt{}, false, ErrStorageUnavailable
	}
	if err != nil {
		return domain.Receipt{}, false, err
	}
	if exists {
		matches := previous.Fingerprint == fingerprint || (previous.Legacy && namespace.Kind == "anonymous" && previous.Fingerprint == legacyFingerprint)
		if !matches {
			return domain.Receipt{}, false, ErrIdentityConflict
		}
		return domain.Receipt{SubmissionID: previous.ID, ReceivedAt: previous.ReceivedAt}, false, nil
	}
	if !normalized.Accepted {
		return domain.Receipt{}, false, &ValidationError{Code: "ConsentRequired", Fields: map[string]string{"consent.accepted": "ConsentRequired"}}
	}
	if normalized.ConsentVersion != s.consentVersion {
		return domain.Receipt{}, false, &ValidationError{Code: "ConsentRequired", Fields: map[string]string{"consent.documentVersion": "ConsentOutdated"}}
	}
	submission := domain.Submission{Namespace: namespace, CommandID: string(normalized.ID), ID: normalized.ID, Name: normalized.Name, Email: normalized.Email, Phone: normalized.Phone, Message: normalized.Message, ConsentVersion: normalized.ConsentVersion, ConsentPurpose: "feedback-reply", AcceptedAt: now, ReceivedAt: now, Fingerprint: fingerprint}
	return s.repository.Accept(ctx, submission)
}
