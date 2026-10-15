package domain

import (
	"errors"
	"strings"
	"time"
)

const (
	MaxErrorCodeSize = 128
	MaxProviderSize  = 128
)

func ValidateSubmission(submission Submission) error {
	input := Input{ID: submission.ID, Name: submission.Name, Email: submission.Email, Phone: submission.Phone, Message: submission.Message, ConsentVersion: submission.ConsentVersion, Accepted: true}
	normalized, fields := Normalize(input)
	if (submission.Legacy && submission.Namespace.Kind != "anonymous") || !submission.Namespace.Valid() || submission.CommandID != string(submission.ID) || len(fields) != 0 || normalized.ID != submission.ID || normalized.Name != submission.Name || normalized.Email != submission.Email || normalized.Phone != submission.Phone || normalized.Message != submission.Message || !ValidEmail(submission.Email) || len(submission.Name) > 400 || len(submission.Message) > 8000 || len(submission.ConsentVersion) == 0 || len(submission.ConsentVersion) > 128 || submission.ConsentPurpose != "feedback-reply" || len(submission.Fingerprint) != 64 || !ValidHex(submission.Fingerprint) || submission.AcceptedAt.IsZero() || submission.ReceivedAt.IsZero() || submission.AcceptedAt.After(submission.ReceivedAt) || !IsMoscowTime(submission.AcceptedAt) || !IsMoscowTime(submission.ReceivedAt) {
		return errors.New("submission record is incomplete or invalid")
	}
	return nil
}
func ValidateJob(job Job) error {
	if !ValidChannel(job.Channel) || job.Attempts < 0 || job.Attempts > 10 || !ValidJobState(job.State) || len(job.ErrorCode) > MaxErrorCodeSize || len(job.ProviderID) > MaxProviderSize || !ValidErrorCode(job.ErrorCode) || !ValidProviderID(job.ProviderID) {
		return errors.New("delivery job is invalid")
	}
	switch job.State {
	case Pending:
		if job.NextAttemptAt.IsZero() || !job.ConfirmedAt.IsZero() || job.ProviderID != "" || (job.Attempts == 0 && !job.StartedAt.IsZero()) || (job.Attempts > 0 && job.StartedAt.IsZero()) {
			return errors.New("pending job fields are invalid")
		}
	case InFlight:
		if job.Attempts <= 0 || job.StartedAt.IsZero() || !job.NextAttemptAt.IsZero() || !job.ConfirmedAt.IsZero() || job.ErrorCode != "" || job.ProviderID != "" {
			return errors.New("in-flight job fields are invalid")
		}
	case Sent:
		if job.Attempts <= 0 || job.StartedAt.IsZero() || !job.NextAttemptAt.IsZero() || job.ConfirmedAt.IsZero() || job.ErrorCode != "" {
			return errors.New("sent job fields are invalid")
		}
	case Failed:
		if !job.NextAttemptAt.IsZero() || job.ErrorCode == "" || job.ConfirmedAt.IsZero() || (job.Attempts == 0 && !job.StartedAt.IsZero()) || (job.Attempts > 0 && job.StartedAt.IsZero()) {
			return errors.New("failed job fields are invalid")
		}
	}
	return nil
}
func ValidJobTimeline(job Job, submission Submission) bool {
	cycleStart := RetryCycleStart(job, submission)
	for _, instant := range []time.Time{job.NextAttemptAt, job.RetryStartedAt, job.StartedAt, job.ConfirmedAt} {
		if !instant.IsZero() && !IsMoscowTime(instant) {
			return false
		}
	}
	if cycleStart.Before(submission.ReceivedAt) {
		return false
	}
	if !job.StartedAt.IsZero() && job.StartedAt.Before(cycleStart) {
		return false
	}
	if !job.NextAttemptAt.IsZero() && job.NextAttemptAt.Before(cycleStart) {
		return false
	}
	if !job.NextAttemptAt.IsZero() && !job.StartedAt.IsZero() && job.NextAttemptAt.Before(job.StartedAt) {
		return false
	}
	return job.ConfirmedAt.IsZero() || job.StartedAt.IsZero() || !job.ConfirmedAt.Before(job.StartedAt)
}
func ValidTransition(previous, next Job) bool {
	if next.ID != previous.ID || next.Channel != previous.Channel || ValidateJob(next) != nil {
		return false
	}
	switch {
	case previous.State == Pending && next.State == InFlight:
		return next.Attempts == previous.Attempts+1 && next.RetryStartedAt.Equal(previous.RetryStartedAt)
	case previous.State == Pending && next.State == Failed:
		return next.Attempts == previous.Attempts && next.RetryStartedAt.Equal(previous.RetryStartedAt)
	case previous.State == InFlight && (next.State == Pending || next.State == Sent || next.State == Failed):
		return next.Attempts == previous.Attempts && next.StartedAt.Equal(previous.StartedAt) && next.RetryStartedAt.Equal(previous.RetryStartedAt)
	case previous.State == Failed && next.State == Pending:
		return next.Attempts == 0 && !next.RetryStartedAt.IsZero() && next.StartedAt.IsZero()
	default:
		return false
	}
}
func ValidFinish(previous, next Job) bool {
	return previous.State == InFlight && ValidTransition(previous, next)
}
func ValidChannel(channel Channel) bool {
	return channel == SMTP || channel == Telegram
}
func ValidJobState(state JobState) bool {
	return state == Pending || state == InFlight || state == Sent || state == Failed
}
func ValidErrorCode(value string) bool {
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', strings.ContainsRune("._-", char):
		default:
			return false
		}
	}
	return true
}
func ValidProviderID(value string) bool {
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', strings.ContainsRune("._:-", char):
		default:
			return false
		}
	}
	return true
}
func ValidHex(value string) bool {
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9', char >= 'a' && char <= 'f':
		default:
			return false
		}
	}
	return true
}
func IsMoscowTime(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 3*60*60
}
func MoscowLocation() *time.Location {
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return time.FixedZone("MSK", 3*60*60)
	}
	return location
}
func RetryCycleStart(job Job, submission Submission) time.Time {
	if !job.RetryStartedAt.IsZero() {
		return job.RetryStartedAt
	}
	return submission.ReceivedAt
}
func SameContent(a, b Submission) bool {
	return a.Namespace == b.Namespace && a.CommandID == b.CommandID && a.ID == b.ID && a.Name == b.Name && a.Email == b.Email && a.Phone == b.Phone && a.Message == b.Message && a.ConsentVersion == b.ConsentVersion && a.ConsentPurpose == b.ConsentPurpose && a.Fingerprint == b.Fingerprint
}
