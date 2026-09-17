// Package sms는 캠페인 스냅샷과 순차 발송 권한을 관리한다.
// 실제 SMS 발송은 Android 클라이언트만 수행한다.
package sms

import (
	"errors"
	"github.com/sslim7/nature-was/internal/messaging"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrValidation  = errors.New("invalid request")
	ErrNotFound    = errors.New("not found")
	ErrConflict    = errors.New("state conflict")
	ErrIdempotency = errors.New("idempotency conflict")
	identifier     = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
)

const (
	Ready         = "READY"
	Sending       = "SENDING"
	Sent          = "SENT"
	Failed        = "FAILED"
	Completed     = "COMPLETED"
	PartialFailed = "PARTIAL_FAILED"
	Cancelled     = "CANCELLED"
)

type Campaign struct {
	Attachments    []messaging.Attachment `json:"attachments" firestore:"attachments"`
	ID             string                 `json:"id" firestore:"id"`
	Title          string                 `json:"title" firestore:"title"`
	Message        string                 `json:"message" firestore:"message"`
	Status         string                 `json:"status" firestore:"status"`
	RecipientCount int                    `json:"recipientCount" firestore:"recipientCount"`
	ReadyCount     int                    `json:"readyCount" firestore:"readyCount"`
	SendingCount   int                    `json:"sendingCount" firestore:"sendingCount"`
	SentCount      int                    `json:"sentCount" firestore:"sentCount"`
	FailedCount    int                    `json:"failedCount" firestore:"failedCount"`
	Reserved       bool                   `json:"reserved" firestore:"reserved"` // 예약으로 만든 문자 표시. 필드가 없는 과거 문서는 false로 읽힌다.
	CreatedAt      time.Time              `json:"createdAt" firestore:"createdAt"`
	UpdatedAt      time.Time              `json:"updatedAt" firestore:"updatedAt"`
	StartedAt      *time.Time             `json:"startedAt" firestore:"startedAt"`
	CompletedAt    *time.Time             `json:"completedAt" firestore:"completedAt"`
}
type CampaignRecipient struct {
	Transport    string                 `json:"transport,omitempty" firestore:"transport"`
	Attachments  []messaging.Attachment `json:"attachments" firestore:"attachments"`
	ID           string                 `json:"id" firestore:"id"`
	CampaignID   string                 `json:"campaignId" firestore:"campaignId"`
	RecipientID  string                 `json:"recipientId" firestore:"recipientId"`
	Name         string                 `json:"name" firestore:"name"`
	Phone        string                 `json:"phone" firestore:"phone"`
	Message      string                 `json:"message" firestore:"message"`
	Status       string                 `json:"status" firestore:"status"`
	AttemptID    string                 `json:"attemptId" firestore:"attemptId"`
	AttemptCount int                    `json:"attemptCount" firestore:"attemptCount"`
	SentAt       *time.Time             `json:"sentAt" firestore:"sentAt"`
	FailedAt     *time.Time             `json:"failedAt" firestore:"failedAt"`
	ErrorCode    string                 `json:"errorCode" firestore:"errorCode"`
	ErrorMessage string                 `json:"errorMessage" firestore:"errorMessage"`
	CreatedAt    time.Time              `json:"createdAt" firestore:"createdAt"`
	UpdatedAt    time.Time              `json:"updatedAt" firestore:"updatedAt"`
	Attempts     []Attempt              `json:"-" firestore:"attempts"`
}
type Attempt struct {
	ID         string     `firestore:"id"`
	Status     string     `firestore:"status"`
	StartedAt  time.Time  `firestore:"startedAt"`
	FinishedAt *time.Time `firestore:"finishedAt"`
}
type document struct {
	Revision    int64               `firestore:"revision"`
	Campaign    Campaign            `firestore:"campaign"`
	Recipients  []CampaignRecipient `firestore:"recipients"`
	Fingerprint string              `firestore:"fingerprint"`
}
type CreateRequest struct {
	AttachmentIDs []string `json:"attachmentIds,omitempty"`
	RequestID     string   `json:"requestId"`
	Title         string   `json:"title"`
	Message       string   `json:"message"`
	RecipientIDs  []string `json:"recipientIds"`
	Reserved      bool     `json:"reserved,omitempty"` // 생략하면 false. omitempty라 기존 요청의 fingerprint를 바꾸지 않는다.
}
type PatchRequest struct {
	Transport    string `json:"transport,omitempty"`
	Status       string `json:"status"`
	AttemptID    string `json:"attemptId"`
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}
type Result struct {
	Campaign        Campaign          `json:"campaign"`
	Recipient       CampaignRecipient `json:"recipient"`
	DispatchAllowed bool              `json:"dispatchAllowed"`
}

func validID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "/\\") && id != "." && id != ".."
}
func (r CreateRequest) validate() error {
	if !identifier.MatchString(r.RequestID) || strings.TrimSpace(r.Title) == "" || utf8.RuneCountInString(r.Title) > 100 || (strings.TrimSpace(r.Message) == "" && len(r.AttachmentIDs) == 0) || utf8.RuneCountInString(r.Message) > 2000 || len(r.RecipientIDs) < 1 || len(r.RecipientIDs) > 50 {
		return ErrValidation
	}
	if len(r.AttachmentIDs) > 3 {
		return ErrValidation
	}
	for _, id := range r.AttachmentIDs {
		if !messaging.ValidID(id) {
			return ErrValidation
		}
	}
	seen := map[string]bool{}
	for _, id := range r.RecipientIDs {
		if !validID(id) || seen[id] {
			return ErrValidation
		}
		seen[id] = true
	}
	return nil
}
func (r PatchRequest) validate() error {
	if r.Transport != "" && r.Transport != "SMS" && r.Transport != "LMS" && r.Transport != "MMS" {
		return ErrValidation
	}
	if !identifier.MatchString(r.AttemptID) || (r.Status != Sending && r.Status != Sent && r.Status != Failed) || utf8.RuneCountInString(r.ErrorCode) > 100 || utf8.RuneCountInString(r.ErrorMessage) > 300 || (r.Status == Failed && strings.TrimSpace(r.ErrorCode) == "") || (r.Status != Failed && (r.ErrorCode != "" || r.ErrorMessage != "")) {
		return ErrValidation
	}
	return nil
}
func (d *document) recount(now time.Time) {
	d.Revision++
	c := &d.Campaign
	c.ReadyCount = 0
	c.SendingCount = 0
	c.SentCount = 0
	c.FailedCount = 0
	for _, r := range d.Recipients {
		switch r.Status {
		case Ready:
			c.ReadyCount++
		case Sending:
			c.SendingCount++
		case Sent:
			c.SentCount++
		case Failed:
			c.FailedCount++
		}
	}
	c.UpdatedAt = now
	if c.Status != Cancelled && c.ReadyCount == 0 && c.SendingCount == 0 {
		c.Status = Completed
		if c.FailedCount > 0 {
			c.Status = PartialFailed
		}
		c.CompletedAt = &now
	}
}
func (d *document) apply(id, action string, req PatchRequest, now time.Time) (Result, error) {
	c := &d.Campaign
	if action == "start" {
		if c.Status == Sending {
			return Result{Campaign: *c}, nil
		}
		if (c.Status != Ready && c.Status != Cancelled) || c.ReadyCount == 0 {
			return Result{}, ErrConflict
		}
		c.Status = Sending
		d.Revision++
		c.CompletedAt = nil
		if c.StartedAt == nil {
			c.StartedAt = &now
		}
		c.UpdatedAt = now
		return Result{Campaign: *c}, nil
	}
	if action == "cancel" {
		if c.Status == Ready || c.Status == Sending {
			c.Status = Cancelled
			d.Revision++
			c.CompletedAt = &now
			c.UpdatedAt = now
		}
		return Result{Campaign: *c}, nil
	}
	var r *CampaignRecipient
	for i := range d.Recipients {
		if d.Recipients[i].ID == id {
			r = &d.Recipients[i]
			break
		}
	}
	if r == nil {
		return Result{}, ErrNotFound
	}
	allowed := false
	if action == "retry" {
		if r.Status != Failed || c.SendingCount != 0 || r.AttemptCount >= 20 || r.ErrorCode == "OUTCOME_UNKNOWN" || r.ErrorCode == "PARTIAL_SENT" {
			return Result{}, ErrConflict
		}
		r.Status = Ready
		r.AttemptID = ""
		r.SentAt = nil
		r.FailedAt = nil
		r.ErrorCode = ""
		r.ErrorMessage = ""
		c.Status = Ready
		c.CompletedAt = nil
	} else {
		if err := req.validate(); err != nil {
			return Result{}, err
		}
		if req.Status == Sending {
			if r.AttemptID == req.AttemptID && r.Status != Ready {
				return Result{Campaign: *c, Recipient: *r}, nil
			}
			if r.Status != Ready || c.Status != Sending || c.SendingCount != 0 || r.AttemptCount >= 20 {
				return Result{}, ErrConflict
			}
			// 이전 시도까지 포함해 캠페인 안에서 attempt ID 재사용을 막는다.
			for _, other := range d.Recipients {
				for _, a := range other.Attempts {
					if a.ID == req.AttemptID {
						return Result{}, ErrConflict
					}
				}
			}
			r.Status = Sending
			r.AttemptID = req.AttemptID
			r.AttemptCount++
			r.Attempts = append(r.Attempts, Attempt{ID: req.AttemptID, Status: Sending, StartedAt: now})
			allowed = true
		} else {
			if r.AttemptID != req.AttemptID {
				return Result{}, ErrConflict
			}
			if r.Status == req.Status {
				return Result{Campaign: *c, Recipient: *r}, nil
			}
			// 같은 시도의 결과 불명만 늦게 도착한 실제 SENT 결과로 확정할 수 있다.
			lateSent := r.Status == Failed && r.ErrorCode == "OUTCOME_UNKNOWN" && req.Status == Sent
			if r.Status != Sending && !lateSent {
				return Result{}, ErrConflict
			}
			r.Status = req.Status
			r.Transport = req.Transport
			r.ErrorCode = req.ErrorCode
			r.ErrorMessage = req.ErrorMessage
			if r.Status == Sent {
				r.SentAt = &now
				r.FailedAt = nil
			} else {
				r.FailedAt = &now
			}
			a := &r.Attempts[len(r.Attempts)-1]
			a.Status = r.Status
			a.FinishedAt = &now
		}
	}
	r.UpdatedAt = now
	d.recount(now)
	return Result{Campaign: d.Campaign, Recipient: *r, DispatchAllowed: allowed}, nil
}
