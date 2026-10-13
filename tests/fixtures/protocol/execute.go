package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/JustIkra/umir-feedback/internal/adapters/journal"
	"github.com/JustIkra/umir-feedback/internal/application"
	"github.com/JustIkra/umir-feedback/internal/domain"
)

func (h *Handler) execute(ctx context.Context, r Request) Response {
	res := Response{Version: 1, Project: h.opts.Project, RequestID: r.RequestID, Operation: r.Operation}
	var in struct {
		ExpectedInstanceID string `json:"expectedInstanceID"`
		ExpectedGeneration string `json:"expectedGeneration"`
		Instant            string `json:"instant"`
	}
	if json.Unmarshal(r.Data, &in) != nil {
		return h.failure(r, "InvalidRequest")
	}
	switch r.Operation {
	case "fixture.ready":
		state := h.opts.Clock.Read()
		res.Data = ReadyState{Target: "feedback", InstanceID: state.InstanceID, Protocol: Protocol, BinarySHA256: h.opts.BinarySHA256, ConfigSHA256: h.opts.ConfigSHA256, Ready: state.Healthy && h.opts.Ready(ctx), Capabilities: []string{"fixture.ready", "clock.read", "clock.set", "clock.checkpoint", "feedback.observe", "owner.fault.arm", "owner.fault.read", "owner.fault.clear"}}
	case "clock.read":
		res.Data = h.opts.Clock.Read()
	case "clock.set":
		if code := h.opts.Clock.Set(in.ExpectedInstanceID, in.ExpectedGeneration, in.Instant); code != "" {
			return h.failure(r, code)
		}
		res.Data = h.opts.Clock.Read()
	case "clock.checkpoint":
		state, ok := h.opts.Clock.ReadExpected(in.ExpectedInstanceID, in.ExpectedGeneration)
		if !ok {
			return h.failure(r, "ClockConflict")
		}
		b, e := json.Marshal(state)
		if e != nil {
			return h.failure(r, "ClockUnavailable")
		}
		var canonical any
		if json.Unmarshal(b, &canonical) != nil {
			return h.failure(r, "ClockUnavailable")
		}
		b, e = json.Marshal(canonical)
		if e != nil {
			return h.failure(r, "ClockUnavailable")
		}
		res.Data = struct {
			Clock ClockState `json:"clock"`
			SHA   string     `json:"checkpointSHA256"`
		}{state, SHA(b)}
	case "feedback.observe":
		data, code := h.observe(ctx, r.Data)
		if code != "" {
			return h.failure(r, code)
		}
		res.Data = data
	case "owner.fault.arm", "owner.fault.read", "owner.fault.clear":
		data, code := h.opts.Faults.Control(r.Operation, r.Data)
		if code != "" {
			return h.failure(r, code)
		}
		res.Data = data
	default:
		return h.failure(r, "UnsupportedOperation")
	}
	return res
}

type Observation struct {
	InstanceID     string               `json:"instanceID"`
	SnapshotID     string               `json:"snapshotID"`
	SnapshotSHA256 string               `json:"snapshotSHA256"`
	Recovered      bool                 `json:"recovered"`
	Complete       bool                 `json:"complete"`
	Records        []journal.RecordView `json:"records"`
}

func (h *Handler) observe(ctx context.Context, body []byte) (Observation, string) {
	var in struct {
		Submissions []struct {
			Namespace domain.Namespace    `json:"namespace"`
			ID        domain.SubmissionID `json:"submissionID"`
		} `json:"submissions"`
		WaitMs int `json:"waitMs"`
	}
	if json.Unmarshal(body, &in) != nil {
		return Observation{}, "InvalidRequest"
	}
	selectors := make([]journal.Selector, 0, len(in.Submissions))
	for _, s := range in.Submissions {
		selectors = append(selectors, journal.Selector{Namespace: s.Namespace, ID: s.ID})
	}
	owner := h.opts.Clock.Read()
	until := time.Now().Add(time.Duration(in.WaitMs) * time.Millisecond)
	for {
		if ctx.Err() != nil {
			return Observation{}, "WaitExpired"
		}
		if !h.observationClockMatches(owner) {
			return Observation{}, "ClockConflict"
		}
		rows, complete, e := h.opts.Store.Observe(ctx, selectors)
		if !h.observationClockMatches(owner) {
			return Observation{}, "ClockConflict"
		}
		if e != nil {
			code := "StoreUnavailable"
			if !errors.Is(e, application.ErrStorageUnavailable) {
				code = "OutcomeUnknown"
			}
			return Observation{}, code
		}
		remaining := time.Until(until)
		if !complete && remaining > 0 {
			poll := 100 * time.Millisecond
			if remaining < poll {
				poll = remaining
			}
			timer := time.NewTimer(poll)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Observation{}, "WaitExpired"
			case <-timer.C:
			}
			if !h.observationClockMatches(owner) {
				return Observation{}, "ClockConflict"
			}
			if time.Now().Before(until) {
				continue
			}
		}
		if ctx.Err() != nil {
			return Observation{}, "WaitExpired"
		}
		sort.Slice(rows, func(i, j int) bool { return pairKey(rows[i]) < pairKey(rows[j]) })
		seen := map[string]bool{}
		for _, r := range rows {
			if len(r.Jobs) != 2 {
				return Observation{}, "NotReady"
			}
			for _, j := range r.Jobs {
				if !uuidPattern.MatchString(j.JobID) || seen[j.JobID] {
					return Observation{}, "NotReady"
				}
				seen[j.JobID] = true
			}
		}
		b, e := json.Marshal(rows)
		if e != nil || len(b) > MaxResponse-1024 {
			return Observation{}, "CapacityExceeded"
		}
		id, e := UUID()
		if e != nil {
			return Observation{}, "NotReady"
		}
		if !h.observationClockMatches(owner) {
			return Observation{}, "ClockConflict"
		}
		return Observation{InstanceID: owner.InstanceID, SnapshotID: id, SnapshotSHA256: SHA(b), Recovered: true, Complete: complete, Records: rows}, ""
	}
}
func (h *Handler) observationClockMatches(owner ClockState) bool {
	current, ok := h.opts.Clock.ReadExpected(owner.InstanceID, owner.Generation)
	return ok && current.Instant == owner.Instant && current.Healthy && owner.Healthy
}
func pairKey(r journal.RecordView) string {
	parts := []string{r.Namespace.Kind}
	if r.Namespace.Kind == "app" {
		parts = append(parts, r.Namespace.UserID, r.Namespace.OrgID, r.Namespace.TenantID)
	}
	parts = append(parts, string(r.SubmissionID))
	b, e := json.Marshal(parts)
	if e != nil {
		return ""
	}
	return string(b)
}
