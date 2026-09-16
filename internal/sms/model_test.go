package sms

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fixture() document {
	now := time.Now().UTC()
	return document{Campaign: Campaign{ID: "campaign", Status: Ready, RecipientCount: 2, ReadyCount: 2, CreatedAt: now}, Recipients: []CampaignRecipient{{ID: "recipient1", Status: Ready}, {ID: "recipient2", Status: Ready}}}
}
func step(t *testing.T, d *document, id, action, status, attempt string) Result {
	t.Helper()
	req := PatchRequest{Status: status, AttemptID: attempt}
	if status == Failed {
		req.ErrorCode = "NO_SERVICE"
	}
	r, e := d.apply(id, action, req, time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestDispatchLifecycle(t *testing.T) {
	d := fixture()
	step(t, &d, "", "start", "", "")
	if !step(t, &d, "recipient1", "patch", Sending, "attempt_1").DispatchAllowed {
		t.Fatal("initial claim denied")
	}
	if step(t, &d, "recipient1", "patch", Sending, "attempt_1").DispatchAllowed {
		t.Fatal("duplicate authorized")
	}
	if _, e := d.apply("recipient2", "patch", PatchRequest{Status: Sending, AttemptID: "attempt_2"}, time.Now()); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	step(t, &d, "recipient1", "patch", Sent, "attempt_1")
	step(t, &d, "recipient1", "patch", Sent, "attempt_1")
	if _, e := d.apply("recipient1", "retry", PatchRequest{}, time.Now()); !errors.Is(e, ErrConflict) {
		t.Fatal("SENT reset", e)
	}
	step(t, &d, "recipient2", "patch", Sending, "attempt_2")
	step(t, &d, "recipient2", "patch", Failed, "attempt_2")
	if d.Campaign.Status != PartialFailed || d.Campaign.SentCount != 1 || d.Campaign.FailedCount != 1 {
		t.Fatal(d.Campaign)
	}
	step(t, &d, "recipient2", "retry", "", "")
	step(t, &d, "", "start", "", "")
	if _, e := d.apply("recipient2", "patch", PatchRequest{Status: Sending, AttemptID: "attempt_2"}, time.Now()); !errors.Is(e, ErrConflict) {
		t.Fatal("reused attempt", e)
	}
	step(t, &d, "recipient2", "patch", Sending, "attempt_3")
	step(t, &d, "recipient2", "patch", Sent, "attempt_3")
	if d.Campaign.Status != Completed || len(d.Recipients[1].Attempts) != 2 {
		t.Fatal(d)
	}
}
func TestCancelAndResume(t *testing.T) {
	d := fixture()
	step(t, &d, "", "start", "", "")
	step(t, &d, "recipient1", "patch", Sending, "attempt_1")
	step(t, &d, "", "cancel", "", "")
	step(t, &d, "recipient1", "patch", Sent, "attempt_1")
	if d.Campaign.Status != Cancelled || d.Campaign.SentCount != 1 {
		t.Fatal(d)
	}
	if _, e := d.apply("recipient2", "patch", PatchRequest{Status: Sending, AttemptID: "attempt_2"}, time.Now()); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	step(t, &d, "", "start", "", "")
	if d.Recipients[0].Status != Sent {
		t.Fatal("sent modified")
	}
}
func TestUncertainNeverRetried(t *testing.T) {
	for _, code := range []string{"OUTCOME_UNKNOWN", "PARTIAL_SENT"} {
		d := fixture()
		step(t, &d, "", "start", "", "")
		step(t, &d, "recipient1", "patch", Sending, "attempt_1")
		_, err := d.apply("recipient1", "patch", PatchRequest{Status: Failed, AttemptID: "attempt_1", ErrorCode: code}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.apply("recipient1", "retry", PatchRequest{}, time.Now()); !errors.Is(err, ErrConflict) {
			t.Fatal(code, err)
		}
	}
}
func TestInputValidation(t *testing.T) {
	r := CreateRequest{RequestID: "request_1", Title: "모임", Message: " \n한글\n ", RecipientIDs: []string{"id"}}
	if r.validate() != nil {
		t.Fatal("valid")
	}
	r.Message = strings.Repeat("한", 2001)
	if r.validate() == nil {
		t.Fatal("oversize")
	}
	r.Message = "hello"
	r.RecipientIDs = []string{"id", "id"}
	if r.validate() == nil {
		t.Fatal("duplicate")
	}
}
func TestAttemptLimit(t *testing.T) {
	d := fixture()
	d.Recipients[0].Status = Failed
	d.Recipients[0].AttemptCount = 20
	d.recount(time.Now())
	if _, err := d.apply("recipient1", "retry", PatchRequest{}, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestLateUnknownSentReconciliation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		d := fixture()
		step(t, &d, "", "start", "", "")
		step(t, &d, "recipient1", "patch", Sending, "attempt_1")
		if cancel {
			step(t, &d, "", "cancel", "", "")
		}
		if _, err := d.apply("recipient1", "patch", PatchRequest{Status: Failed, AttemptID: "attempt_1", ErrorCode: "OUTCOME_UNKNOWN", ErrorMessage: "unknown"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := d.apply("recipient1", "patch", PatchRequest{Status: Sent, AttemptID: "attempt_2"}, time.Now()); !errors.Is(err, ErrConflict) {
			t.Fatal("different attempt reconciled", err)
		}
		r := step(t, &d, "recipient1", "patch", Sent, "attempt_1")
		if r.DispatchAllowed || r.Recipient.FailedAt != nil || r.Recipient.SentAt == nil || r.Recipient.ErrorCode != "" || r.Recipient.ErrorMessage != "" || r.Campaign.SentCount != 1 || r.Campaign.FailedCount != 0 || r.Recipient.Attempts[0].Status != Sent {
			t.Fatal(r)
		}
		step(t, &d, "recipient1", "patch", Sent, "attempt_1")
		if cancel && d.Campaign.Status != Cancelled {
			t.Fatal(d.Campaign)
		}
	}
	for _, code := range []string{"NO_SERVICE", "PARTIAL_SENT"} {
		d := fixture()
		step(t, &d, "", "start", "", "")
		step(t, &d, "recipient1", "patch", Sending, "attempt_1")
		d.apply("recipient1", "patch", PatchRequest{Status: Failed, AttemptID: "attempt_1", ErrorCode: code}, time.Now())
		if _, err := d.apply("recipient1", "patch", PatchRequest{Status: Sent, AttemptID: "attempt_1"}, time.Now()); !errors.Is(err, ErrConflict) {
			t.Fatal("ordinary failure rewritten", code, err)
		}
	}
}

func TestRevisionTracksChangesAtIdenticalClockTime(t *testing.T) {
	d := fixture()
	now := d.Campaign.CreatedAt
	d.Campaign.UpdatedAt = now
	startRevision := d.Revision
	if _, err := d.apply("", "start", PatchRequest{}, now); err != nil || d.Revision != startRevision+1 {
		t.Fatal("start change not tracked", err)
	}
	before := d.Revision
	claim, err := d.apply("recipient1", "patch", PatchRequest{Status: Sending, AttemptID: "attempt_1"}, now)
	if err != nil || !claim.DispatchAllowed || d.Revision != before+1 || !d.Campaign.UpdatedAt.Equal(now) {
		t.Fatal("same-clock claim must be persisted", claim, err)
	}
	before = d.Revision
	duplicate, err := d.apply("recipient1", "patch", PatchRequest{Status: Sending, AttemptID: "attempt_1"}, now)
	if err != nil || duplicate.DispatchAllowed || d.Revision != before {
		t.Fatal("duplicate changed revision", duplicate, err)
	}
	if _, err = d.apply("", "cancel", PatchRequest{}, now); err != nil || d.Revision != before+1 {
		t.Fatal("cancel change not tracked", err)
	}
	before = d.Revision
	if _, err = d.apply("recipient1", "patch", PatchRequest{Status: Sent, AttemptID: "attempt_1"}, now); err != nil || d.Revision != before+1 {
		t.Fatal("result change not tracked", err)
	}
}
