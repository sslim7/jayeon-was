package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/recipients"
)

func TestExternalSendAtomicIdempotencyHistoryAndAndroidCount(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	uid := fmt.Sprintf("external-%d", time.Now().UnixNano())
	rs := &recipients.Store{FS: fs}
	s := &FirestoreStore{Client: fs}
	r, e := rs.Save(ctx, uid, "", recipients.Input{Name: "외부홍길동", Phone: "01012345678"})
	if e != nil {
		t.Fatal(e)
	}
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	in := recipients.ExternalSendInput{RequestID: "external-one", SentAt: past}
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount := 0
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, created, e := rs.ExternalSend(ctx, uid, r.ID, in)
			if e != nil || out.Recipient.SentCount != 1 || out.History.Source != "EXTERNAL" || out.History.CampaignID != "" {
				t.Error(out, e)
			}
			if created {
				mu.Lock()
				createdCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatal(createdCount)
	}
	if _, _, e = rs.ExternalSend(ctx, uid, r.ID, recipients.ExternalSendInput{RequestID: in.RequestID, SentAt: past.Add(-time.Hour)}); e != recipients.ErrExternalConflict {
		t.Fatal(e)
	}
	if _, _, e = rs.ExternalSend(ctx, uid+"other", r.ID, in); e == nil {
		t.Fatal("다른계정수신자 접근")
	}
	p, e := rs.List(ctx, uid, "", "", "", 50, false)
	if e != nil || len(p.Items) != 0 {
		t.Fatal("발송포함필터", p, e)
	}
	h, e := rs.History(ctx, uid, r.ID)
	if e != nil || len(h) != 1 || h[0].Source != "EXTERNAL" {
		t.Fatal(h, e)
	}
	global, e := s.History(ctx, uid, "홍길동", 50, "")
	if e != nil || global.Total != 1 || global.Items[0].Source != "EXTERNAL" {
		t.Fatal(global, e)
	}
	// 기존 native 결과와 외부 등록은 같은 성공 집계에 합산된다.
	c, _, e := s.Create(ctx, uid, CreateRequest{RequestID: "external-native", Title: "앱 발송", Message: "본문", RecipientIDs: []string{r.ID}})
	if e != nil {
		t.Fatal(e)
	}
	d, _ := s.Get(ctx, uid, c.ID)
	rid := d.Recipients[0].ID
	for _, step := range []struct {
		id, action string
		req        PatchRequest
	}{{"", "start", PatchRequest{}}, {rid, "patch", PatchRequest{Status: Sending, AttemptID: "android-after-external"}}, {rid, "patch", PatchRequest{Status: Sent, AttemptID: "android-after-external"}}, {rid, "patch", PatchRequest{Status: Sent, AttemptID: "android-after-external"}}} {
		if _, e = s.Apply(ctx, uid, c.ID, step.id, step.action, step.req); e != nil {
			t.Fatal(e)
		}
	}
	// 과거 일시 외부 등록은 횟수만 증가시키고 더 최근 native 발송일시는 유지한다.
	out, _, e := rs.ExternalSend(ctx, uid, r.ID, recipients.ExternalSendInput{RequestID: "external-older", SentAt: past.Add(-time.Hour)})
	if e != nil || out.Recipient.SentCount != 3 || !out.Recipient.LatestSentAt.After(past) {
		t.Fatal(out, e)
	}
	latest := *out.Recipient.LatestSentAt
	r, e = rs.Save(ctx, uid, r.ID, recipients.Input{Name: "현재이름", Phone: "01012345678"})
	if e != nil || r.SentCount != 3 {
		t.Fatal(r, e)
	}
	global, e = s.History(ctx, uid, "외부홍길동", 50, "")
	if e != nil || global.Total != 3 || global.Items[0].Source != "ANDROID" || global.Items[1].Source != "EXTERNAL" {
		t.Fatal(global, e)
	}
	mux := http.NewServeMux()
	recipients.Register(mux, fs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), uid)))
		})
	})
	path := "/recipients/" + r.ID + "/external-sends"
	for _, tc := range []struct {
		body string
		code int
	}{{`{"requestId":"external-invalid","sentAt":"bad"}`, 400}, {fmt.Sprintf(`{"requestId":"external-future","sentAt":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339Nano)), 400}, {fmt.Sprintf(`{"requestId":"external-http","sentAt":%q}`, past.Format(time.RFC3339Nano)), 201}, {fmt.Sprintf(`{"requestId":"external-http","sentAt":%q}`, past.Format(time.RFC3339Nano)), 200}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(tc.body)))
		if w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
		if tc.code < 300 {
			var v recipients.ExternalSendResult
			if e = json.Unmarshal(w.Body.Bytes(), &v); e != nil || v.Recipient.SentCount != 4 || !v.Recipient.LatestSentAt.Equal(latest) {
				t.Fatal(v, e)
			}
		}
	}
	rs.Delete(ctx, uid, r.ID)
	global, e = s.History(ctx, uid, "외부홍길동", 50, "")
	if e != nil || global.Total != 3 {
		t.Fatal("삭제후스냅샷", global, e)
	}
}
