package application

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/JustIkra/umir-feedback/internal/domain"
)

func (c *Challenges) ProcessOne(ctx context.Context, sender CodeSender) (bool, error) {
	if ctx == nil || sender == nil {
		return false, ErrInvalidChallengeRequest
	}
	c.mu.Lock()
	if err := c.readyLocked(); err != nil {
		c.mu.Unlock()
		return false, err
	}
	now := moscowTime(c.clock.Now())
	ids := make([]domain.ChallengeID, 0, len(c.items))
	for id := range c.items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := c.items[ids[i]], c.items[ids[j]]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return ids[i] < ids[j]
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	for _, id := range ids {
		challenge := domain.CloneEmailChallenge(c.items[id])
		if challenge.State != domain.ChallengePending {
			continue
		}
		if !now.Before(challenge.ExpiresAt) {
			if err := c.expireAndSaveLocked(ctx, challenge, now); err != nil {
				c.mu.Unlock()
				return false, err
			}
			continue
		}
		for i := range challenge.Issues {
			issue := challenge.Issues[i]
			job := issue.Job
			if job.State != domain.Pending || now.Before(job.NextAttemptAt) {
				continue
			}
			code, err := c.openCode(challenge)
			if err != nil {
				c.mu.Unlock()
				return false, err
			}
			digest := c.secrets.Digest(codeScope(challenge.ID, challenge.Purpose), code)
			if !validCode(code) || !c.secrets.Equal(digest, challenge.CodeDigest) {
				c.mu.Unlock()
				return false, ErrChallengeCrypto
			}
			challenge.Issues[i].Job.State = domain.InFlight
			challenge.Issues[i].Job.Attempts++
			challenge.Issues[i].Job.StartedAt = now
			challenge.Issues[i].Job.ConfirmedAt = time.Time{}
			challenge.Issues[i].Job.ErrorCode = ""
			if err := c.saveLocked(ctx, challenge); err != nil {
				c.mu.Unlock()
				return false, err
			}
			c.items[id] = domain.CloneEmailChallenge(challenge)
			if err := c.pruneLocked(ctx, now); err != nil {
				c.mu.Unlock()
				return false, err
			}
			key := jobKey(id, issue.ID)
			remaining := challenge.ExpiresAt.Sub(now)
			sendCtx, cancel := context.WithTimeout(ctx, remaining)
			c.active[key] = cancel
			mail := domain.CodeMail{
				ChallengeID: challenge.ID,
				IssuanceID:  issue.ID,
				Email:       challenge.Email,
				Code:        code,
				CreatedAt:   challenge.CreatedAt,
				ExpiresAt:   challenge.ExpiresAt,
			}
			c.mu.Unlock()
			result := sender.SendOTP(sendCtx, mail)
			cancel()
			return c.finishIssue(ctx, id, issue.ID, result)
		}
	}
	c.mu.Unlock()
	return false, nil
}

func (c *Challenges) finishIssue(ctx context.Context, id domain.ChallengeID, issueID domain.IssuanceID, result domain.DeliveryResult) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, jobKey(id, issueID))
	if err := c.readyLocked(); err != nil {
		return true, err
	}
	challenge, ok := c.items[id]
	if !ok {
		return true, nil
	}
	challenge = domain.CloneEmailChallenge(challenge)
	issueIndex := -1
	for i := range challenge.Issues {
		if challenge.Issues[i].ID == issueID {
			issueIndex = i
			break
		}
	}
	if issueIndex < 0 || challenge.Issues[issueIndex].Job.State != domain.InFlight {
		return true, nil
	}
	now := moscowTime(c.clock.Now())
	job := &challenge.Issues[issueIndex].Job
	job.StartedAt = time.Time{}
	job.ErrorCode = result.Code
	if !now.Before(challenge.ExpiresAt) && challenge.State == domain.ChallengePending {
		c.expire(&challenge)
		markUnsentFailed(&challenge, now)
		c.cancelChallengeJobsLocked(id)
	} else if result.Success && challenge.State == domain.ChallengePending {
		job.State = domain.Sent
		job.ConfirmedAt = now
		job.ErrorCode = ""
	} else if challenge.State != domain.ChallengePending || result.Permanent || job.Attempts >= 3 {
		job.State = domain.Failed
		job.ConfirmedAt = now
	} else {
		job.State = domain.Pending
		delay := time.Duration(1<<(job.Attempts-1)) * 10 * time.Second
		if result.RetryAfter > delay {
			delay = result.RetryAfter
		}
		job.NextAttemptAt = now.Add(delay)
		if !job.NextAttemptAt.Before(challenge.ExpiresAt) {
			job.State = domain.Failed
			job.ConfirmedAt = now
		}
	}
	if err := c.saveLocked(context.WithoutCancel(ctx), challenge); err != nil {
		return true, fmt.Errorf("save email challenge delivery result: %w", err)
	}
	c.items[id] = domain.CloneEmailChallenge(challenge)
	if err := c.pruneLocked(context.WithoutCancel(ctx), now); err != nil {
		return true, err
	}
	return true, nil
}

func jobKey(id domain.ChallengeID, issueID domain.IssuanceID) string {
	return string(id) + ":" + string(issueID)
}
