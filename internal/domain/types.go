package domain

import "time"

type SubmissionID string
type Channel string
type JobState string

const (
	SMTP     Channel  = "smtp"
	Telegram Channel  = "telegram"
	Pending  JobState = "pending"
	InFlight JobState = "in-flight"
	Sent     JobState = "sent"
	Failed   JobState = "failed"
)

type Submission struct {
	ID             SubmissionID
	Name           string
	Email          string
	Phone          string
	Message        string
	ConsentVersion string
	ConsentPurpose string
	AcceptedAt     time.Time
	ReceivedAt     time.Time
	Fingerprint    string
}

type Receipt struct {
	SubmissionID SubmissionID `json:"submissionID"`
	ReceivedAt   time.Time    `json:"receivedAt"`
}

type Job struct {
	Channel        Channel
	State          JobState
	Attempts       int
	NextAttemptAt  time.Time
	RetryStartedAt time.Time
	StartedAt      time.Time
	ConfirmedAt    time.Time
	ErrorCode      string
	ProviderID     string
}

type Delivery struct {
	Submission Submission
	Job        Job
}

type RetryPolicy struct {
	MaxAttempts int
	MaxAge      time.Duration
	MinDelay    time.Duration
	MaxDelay    time.Duration
}

type DeliveryResult struct {
	Success    bool
	Permanent  bool
	Code       string
	ProviderID string
	RetryAfter time.Duration
}
