package sms

import (
	"bytes"
	"cloud.google.com/go/firestore"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/sslim7/nature-was/internal/recipients"
	"image"
	"image/png"
	"os"
	"testing"
	"time"
)

func TestAttachmentSnapshotHistoryAndIncludeSent(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	uid := fmt.Sprintf("sms-extension-%d", time.Now().UnixNano())
	rs := &recipients.Store{FS: fs}
	r, e := rs.Save(ctx, uid, "", recipients.Input{Name: "이전이름", Phone: "01012345678"})
	if e != nil {
		t.Fatal(e)
	}
	as := &messaging.Store{FS: fs}
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	a, e := as.Upload(ctx, uid, messaging.Upload{Name: "명함.png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(b.Bytes())})
	if e != nil {
		t.Fatal(e)
	}
	s := &FirestoreStore{Client: fs}
	c, _, e := s.Create(ctx, uid, CreateRequest{RequestID: "attachment-request", Title: "명함", RecipientIDs: []string{r.ID}, AttachmentIDs: []string{a.ID}})
	if e != nil || len(c.Attachments) != 1 {
		t.Fatal(c, e)
	}
	d, e := s.Get(ctx, uid, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	rid := d.Recipients[0].ID
	rs.Save(ctx, uid, r.ID, recipients.Input{Name: "새이름", Phone: "01099998888"})
	messaging.Collection(fs, uid, "smsAttachments").Doc(a.ID).Update(ctx, []firestore.Update{{Path: "deleted", Value: true}})
	if _, _, e = s.Create(ctx, uid, CreateRequest{RequestID: "attachment-other", Title: "삭제첨부", RecipientIDs: []string{r.ID}, AttachmentIDs: []string{a.ID}}); e != ErrNotFound {
		t.Fatal(e)
	}
	if _, e = s.Apply(ctx, uid, c.ID, "", "start", PatchRequest{}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(ctx, uid, c.ID, rid, "patch", PatchRequest{Status: Sending, AttemptID: "attempt-one"}); e != nil {
		t.Fatal(e)
	}
	out, e := s.Apply(ctx, uid, c.ID, rid, "patch", PatchRequest{Status: Sent, AttemptID: "attempt-one", Transport: "MMS"})
	if e != nil || out.Recipient.Name != "이전이름" || len(out.Recipient.Attachments) != 1 {
		t.Fatal(out, e)
	}
	p, e := rs.List(ctx, uid, "", "", "", 50, false)
	if e != nil || len(p.Items) != 0 {
		t.Fatal(p, e)
	}
	p, e = rs.List(ctx, uid, "", "", "", 50, true)
	if e != nil || len(p.Items) != 1 || (p.Items[0].LatestSentAt == nil || p.Items[0].SentCount != 1) {
		t.Fatal(p, e)
	}
	h, e := rs.History(ctx, uid, r.ID)
	if e != nil || len(h) != 1 || h[0].Name != "이전이름" || h[0].Transport != "MMS" || h[0].CampaignTitle != "명함" {
		t.Fatal(h, e)
	}
	if _, e = s.Apply(ctx, uid, c.ID, rid, "patch", PatchRequest{Status: Sent, AttemptID: "attempt-one", Transport: "MMS"}); e != nil {
		t.Fatal(e)
	}
	h, e = rs.History(ctx, uid, r.ID)
	if e != nil || len(h) != 1 {
		t.Fatal(h, e)
	}
	p, e = rs.List(ctx, uid, "", "", "", 50, true)
	if e != nil || p.Items[0].SentCount != 1 {
		t.Fatal("멱등결과 중복집계", p, e)
	}
	second, _, e := s.Create(ctx, uid, CreateRequest{RequestID: "second-count", Title: "두번째", Message: "본문", RecipientIDs: []string{r.ID}})
	if e != nil {
		t.Fatal(e)
	}
	sd, _ := s.Get(ctx, uid, second.ID)
	sr := sd.Recipients[0].ID
	s.Apply(ctx, uid, second.ID, "", "start", PatchRequest{})
	s.Apply(ctx, uid, second.ID, sr, "patch", PatchRequest{Status: Sending, AttemptID: "late-attempt"})
	s.Apply(ctx, uid, second.ID, sr, "patch", PatchRequest{Status: Failed, AttemptID: "late-attempt", ErrorCode: "OUTCOME_UNKNOWN"})
	for range 2 {
		if _, e = s.Apply(ctx, uid, second.ID, sr, "patch", PatchRequest{Status: Sent, AttemptID: "late-attempt"}); e != nil {
			t.Fatal(e)
		}
	}
	p, e = rs.List(ctx, uid, "", "", "", 50, true)
	if e != nil || p.Items[0].SentCount != 2 {
		t.Fatal("늦은성공집계", p, e)
	}
	saved, e := rs.Save(ctx, uid, r.ID, recipients.Input{Name: "다시수정", Phone: "01099998888"})
	if e != nil || saved.SentCount != 2 {
		t.Fatal("수정count손실", saved, e)
	}
	third, _, e := s.Create(ctx, uid, CreateRequest{RequestID: "third-retry", Title: "재시도", Message: "본문", RecipientIDs: []string{r.ID}})
	if e != nil {
		t.Fatal(e)
	}
	td, e := s.Get(ctx, uid, third.ID)
	if e != nil {
		t.Fatal(e)
	}
	tr := td.Recipients[0].ID
	for _, step := range []struct {
		rid, action string
		req         PatchRequest
	}{{"", "start", PatchRequest{}}, {tr, "patch", PatchRequest{Status: Sending, AttemptID: "failed-attempt"}}, {tr, "patch", PatchRequest{Status: Failed, AttemptID: "failed-attempt", ErrorCode: "NO_SERVICE"}}, {tr, "retry", PatchRequest{}}, {"", "start", PatchRequest{}}, {tr, "patch", PatchRequest{Status: Sending, AttemptID: "retry-attempt"}}, {tr, "patch", PatchRequest{Status: Sent, AttemptID: "retry-attempt"}}, {tr, "patch", PatchRequest{Status: Sent, AttemptID: "retry-attempt"}}} {
		if _, e = s.Apply(ctx, uid, third.ID, step.rid, step.action, step.req); e != nil {
			t.Fatal(e)
		}
	}
	p, e = rs.List(ctx, uid, "", "", "", 50, true)
	if e != nil || p.Items[0].SentCount != 3 {
		t.Fatal("실패재시도후count", p, e)
	}
	rs.Delete(ctx, uid, r.ID)
	h, e = rs.History(ctx, uid, r.ID)
	if e != nil || len(h) != 4 {
		t.Fatal(h, e)
	}
}
