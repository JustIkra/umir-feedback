//go:build integration

package pgintegration

import (
	"context"
	"testing"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

type domainNamespace = domain.Namespace

const fingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// submissionOf builds a valid submission with synthetic data.
func submissionOf(ns domain.Namespace, id string, at time.Time) domain.Submission {
	return domain.Submission{
		Namespace: ns, CommandID: id, ID: domain.SubmissionID(id),
		Name: "Synthetic Name", Email: "synthetic@example.test", Phone: "+79990000000", Message: "Synthetic message text",
		ConsentVersion: "2026-10-12", ConsentPurpose: "feedback-reply", AcceptedAt: at, ReceivedAt: at, Fingerprint: fingerprint,
	}
}

type acceptor interface {
	Accept(context.Context, domain.Submission) (domain.Receipt, bool, error)
}

func seed(t testing.TB, store acceptor, ns domain.Namespace, id string, at time.Time) {
	t.Helper()
	if _, created, err := store.Accept(context.Background(), submissionOf(ns, id, at)); err != nil || !created {
		t.Fatalf("seed %s: created=%v err=%v", id, created, err)
	}
}
