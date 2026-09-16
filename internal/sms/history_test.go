package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
)

func TestGlobalHistoryOrderingFilteringAndIsolation(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	s := &FirestoreStore{Client: fs}
	uid := fmt.Sprintf("global-history-%d", time.Now().UnixNano())
	at := time.Now().UTC().Truncate(time.Microsecond)
	older := at.Add(-time.Hour)
	failed := at.Add(-time.Minute)
	sending := at.Add(time.Minute)
	c := Campaign{ID: "campaign-a", Title: "캠페인 제목", CreatedAt: older}
	sentRow := CampaignRecipient{ID: "sent", CampaignID: c.ID, RecipientID: "deleted-recipient", Name: "홍길동", Phone: "01012345678", Message: "스냅샷 본문", Status: Sent, AttemptID: "sent-attempt", SentAt: &at, UpdatedAt: sending.Add(time.Hour), CreatedAt: older}
	retryRow := CampaignRecipient{ID: "retry", CampaignID: c.ID, RecipientID: "same-person", Name: "홍길동", Phone: "01011112222", Message: "스냅샷 본문", Status: Ready, UpdatedAt: sending, CreatedAt: older, Attempts: []Attempt{{ID: "failed-attempt", Status: Failed, StartedAt: older, FinishedAt: &failed}}}
	pending := CampaignRecipient{ID: "pending", Name: "김영희", Status: Sending, AttemptID: "pending-attempt", UpdatedAt: sending, CreatedAt: older}
	neverSent := CampaignRecipient{ID: "never", Name: "홍길동", Status: Ready, UpdatedAt: sending, CreatedAt: older}
	d := document{Campaign: c, Recipients: []CampaignRecipient{sentRow, retryRow, pending, neverSent}}
	ref := s.collection(uid).Doc(c.ID)
	if _, e = ref.Set(ctx, d); e != nil {
		t.Fatal(e)
	}
	oldAttempt := retryRow
	oldAttempt.Status = Failed
	oldAttempt.AttemptID = "failed-attempt"
	oldAttempt.FailedAt = &failed
	oldAttempt.ErrorCode = "NO_SERVICE"
	oldAttempt.UpdatedAt = failed
	if _, e = ref.Collection("attempts").Doc("old-failure").Set(ctx, oldAttempt); e != nil {
		t.Fatal(e)
	}
	if _, e = ref.Collection("attempts").Doc("sent-copy").Set(ctx, sentRow); e != nil {
		t.Fatal(e)
	}
	first, e := s.History(ctx, uid, "", 1, "")
	if e != nil || first.Total != 3 || len(first.Items) != 1 || first.Items[0].Status != Sending || first.NextCursor == nil {
		t.Fatal(first, e)
	}
	second, e := s.History(ctx, uid, "", 1, *first.NextCursor)
	if e != nil || len(second.Items) != 1 || second.Items[0].Status != Sent || second.NextCursor == nil {
		t.Fatal(second, e)
	}
	third, e := s.History(ctx, uid, "", 1, *second.NextCursor)
	if e != nil || len(third.Items) != 1 || third.Items[0].Status != Failed || third.Items[0].ErrorCode != "NO_SERVICE" || third.NextCursor != nil {
		t.Fatal(third, e)
	}
	filtered, e := s.History(ctx, uid, " 길동 ", 50, "")
	if e != nil || filtered.Total != 2 || len(filtered.Items) != 2 || filtered.Items[0].CampaignTitle != c.Title || filtered.Items[0].Message != "스냅샷 본문" {
		t.Fatal(filtered, e)
	}
	other, e := s.History(ctx, uid+"-other", "", 50, "")
	if e != nil || len(other.Items) != 0 {
		t.Fatal(other, e)
	}
	if _, e = s.History(ctx, uid, "다른검색", 50, *first.NextCursor); e != ErrValidation {
		t.Fatal("검색조건다른커서", e)
	}
	if _, e = s.History(ctx, uid+"-other", "", 50, *first.NextCursor); e != ErrValidation {
		t.Fatal("소유자다른커서", e)
	}
	mux := http.NewServeMux()
	Register(mux, fs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), uid)))
		})
	})
	for _, tc := range []struct {
		path string
		code int
	}{{"/sms/history?q=길동", 200}, {"/sms/history?limit=0", 400}, {"/sms/history?cursor=invalid", 400}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		if tc.code == 200 {
			var body struct {
				Items []map[string]any `json:"items"`
			}
			if e = json.Unmarshal(w.Body.Bytes(), &body); e != nil {
				t.Fatal(e)
			}
			for _, row := range body.Items {
				if _, ok := row["attachments"].([]any); !ok {
					t.Fatal("첨부가 배열 아님", row)
				}
			}
		}
	}
	tie := d
	tie.Campaign.ID = "campaign-z"
	tie.Recipients = []CampaignRecipient{sentRow}
	if _, e = s.collection(uid).Doc(tie.Campaign.ID).Set(ctx, tie); e != nil {
		t.Fatal(e)
	}
	tied, e := s.History(ctx, uid, "길동", 1, "")
	if e != nil || tied.Items[0].CampaignID != "campaign-z" || tied.NextCursor == nil {
		t.Fatal("같은시각 ID 정렬", tied, e)
	}
	next, e := s.History(ctx, uid, "길동", 1, *tied.NextCursor)
	if e != nil || next.Items[0].CampaignID != "campaign-a" || next.Items[0].Status != Sent {
		t.Fatal("같은시각 페이지 유실", next, e)
	}

	stale := d
	stale.Recipients[0].Status = Sending
	stale.Recipients[0].SentAt = nil
	if _, e = ref.Set(ctx, stale); e != nil {
		t.Fatal(e)
	}
	resolved, e := s.History(ctx, uid, "길동", 50, "")
	if e != nil {
		t.Fatal(e)
	}
	for _, h := range resolved.Items {
		if h.ID == "campaign-a_sent_sent-attempt" && h.Status != Sent {
			t.Fatal("확정결과가낡은스냅샷으로역행", h)
		}
	}

}
