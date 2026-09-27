package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	urlpkg "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/messaging"
)

// sweepHistory 는 limit 1 로 커서를 끝까지 따라가며 나온 줄의 ID 를 순서대로 모은다.
// 같은 줄이 두 번 나오면 그 자리에서 실패시킨다 — 페이지 경계의 겹침은 총 건수만 보면
// 대기 줄이 늘어난 것과 구분되지 않아 조용히 지나간다.
func sweepHistory(t *testing.T, ctx context.Context, s *FirestoreStore, uid, q, from, to string) []string {
	t.Helper()
	ids := []string{}
	seen := map[string]bool{}
	cursor := ""
	for range 100 {
		page, e := s.History(ctx, uid, q, 1, cursor, from, to)
		if e != nil {
			t.Fatal(e)
		}
		for _, h := range page.Items {
			if seen[h.ID] {
				t.Fatalf("같은 줄이 두 페이지에 겹쳐 나왔다: %s", h.ID)
			}
			seen[h.ID] = true
			ids = append(ids, h.ID)
		}
		if page.NextCursor == nil {
			if page.Total != len(ids) {
				t.Fatalf("total %d 인데 끝까지 돌아 %d 줄만 나왔다 — 페이지 경계에서 샜다: %v", page.Total, len(ids), ids)
			}
			return ids
		}
		cursor = *page.NextCursor
	}
	t.Fatal("커서가 끝나지 않는다")
	return nil
}

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
	// 아직 보내지 않은 사람도 한 줄씩 들어온다. retry 는 「지난 실패」와 「지금 대기」 두 줄이다.
	want := []string{"campaign-a_retry#pending", "campaign-a_pending_pending-attempt", "campaign-a_never#pending", "campaign-a_sent_sent-attempt", "campaign-a_retry_failed-attempt"}
	if got := sweepHistory(t, ctx, s, uid, "", "", ""); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatal("정렬/페이징", got)
	}
	first, e := s.History(ctx, uid, "", 1, "", "", "")
	if e != nil || first.Total != 5 || len(first.Items) != 1 || first.Items[0].Status != Ready || first.NextCursor == nil {
		t.Fatal(first, e)
	}
	second, e := s.History(ctx, uid, "", 1, *first.NextCursor, "", "")
	if e != nil || len(second.Items) != 1 || second.Items[0].Status != Sending || second.NextCursor == nil {
		t.Fatal(second, e)
	}
	last, e := s.History(ctx, uid, "", 5, "", "", "")
	if e != nil || len(last.Items) != 5 || last.Items[4].Status != Failed || last.Items[4].ErrorCode != "NO_SERVICE" || last.NextCursor != nil {
		t.Fatal(last, e)
	}
	filtered, e := s.History(ctx, uid, " 길동 ", 50, "", "", "")
	if e != nil || filtered.Total != 4 || len(filtered.Items) != 4 || filtered.Items[0].CampaignTitle != c.Title || filtered.Items[0].Message != "스냅샷 본문" {
		t.Fatal(filtered, e)
	}
	// 앱의 검색칸이 「이름 또는 폰번호 뒷4자리」라 숫자도 걸려야 한다. 예전에는 이름만 봤다.
	for _, tc := range []struct {
		why  string
		q    string
		want int
	}{
		{"전화번호 뒷4자리", "5678", 1},
		{"하이픈 표기 — 실패 줄과 대기 줄 둘 다", "1111-2222", 2},
		{"+82 표기", "+82 10-1234-5678", 1},
		{"글자+숫자는 둘 다 맞아야 한다", "길동 5678", 1},
		{"글자가 다르면 번호가 맞아도 불일치", "영희 5678", 0},
		{"일치 없음", "9999", 0},
	} {
		got, e := s.History(ctx, uid, tc.q, 50, "", "", "")
		if e != nil || got.Total != tc.want {
			t.Fatalf("%s: q=%q → %d건 (%v)", tc.why, tc.q, got.Total, e)
		}
	}
	other, e := s.History(ctx, uid+"-other", "", 50, "", "", "")
	if e != nil || len(other.Items) != 0 {
		t.Fatal(other, e)
	}
	if _, e = s.History(ctx, uid, "다른검색", 50, *first.NextCursor, "", ""); e != ErrValidation {
		t.Fatal("검색조건다른커서", e)
	}
	if _, e = s.History(ctx, uid+"-other", "", 50, *first.NextCursor, "", ""); e != ErrValidation {
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
	// 같은 시각 줄이 늘어나도 (시각, ID) 완전순서 그대로 한 번씩만 나와야 한다.
	tied := []string{"campaign-a_retry#pending", "campaign-a_never#pending", "campaign-z_sent_sent-attempt", "campaign-a_sent_sent-attempt", "campaign-a_retry_failed-attempt"}
	if got := sweepHistory(t, ctx, s, uid, "길동", "", ""); strings.Join(got, ",") != strings.Join(tied, ",") {
		t.Fatal("같은시각 ID 정렬/페이지 유실", got)
	}

	stale := d
	stale.Recipients[0].Status = Sending
	stale.Recipients[0].SentAt = nil
	if _, e = ref.Set(ctx, stale); e != nil {
		t.Fatal(e)
	}
	resolved, e := s.History(ctx, uid, "길동", 50, "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	for _, h := range resolved.Items {
		if h.ID == "campaign-a_sent_sent-attempt" && h.Status != Sent {
			t.Fatal("확정결과가낡은스냅샷으로역행", h)
		}
	}

}

// 화면은 「성공 N · 실패 N · 미발송 N」을 세고 미발송 명단을 펼쳐야 한다. 그러려면 아직
// 보내지 않은 사람도 이력에 있어야 하고, 기간으로 끊을 수 있어야 한다.
func TestHistoryIncludesUnsentRecipientsAndPeriodFilter(t *testing.T) {
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
	uid := fmt.Sprintf("unsent-history-%d", time.Now().UnixNano())
	base := time.Now().UTC().Truncate(time.Microsecond)
	oldFailed := base.Add(-9 * time.Minute)
	sentAt := base.Add(-time.Hour)
	c := Campaign{ID: "c1", Title: "기간 캠페인", CreatedAt: sentAt}
	rows := []CampaignRecipient{
		{ID: "ready-only", CampaignID: c.ID, RecipientID: "p-ready", Name: "대기자", Phone: "01000000001", Message: "본문", Status: Ready, UpdatedAt: base, CreatedAt: sentAt},
		{ID: "retried", CampaignID: c.ID, RecipientID: "p-retry", Name: "재시도자", Phone: "01000000004", Message: "본문", Status: Ready, UpdatedAt: base.Add(time.Minute), CreatedAt: sentAt, Attempts: []Attempt{{ID: "old-attempt-1", Status: Failed, StartedAt: base.Add(-10 * time.Minute), FinishedAt: &oldFailed}}},
		{ID: "sending-live", CampaignID: c.ID, RecipientID: "p-live", Name: "발송중", Phone: "01000000002", Message: "본문", Status: Sending, AttemptID: "live-attempt-1", UpdatedAt: base.Add(2 * time.Minute), CreatedAt: sentAt},
		// 결과를 못 받은 채 AttemptID 조차 없는 상태. 예전에는 이 사람이 목록에서 통째로 사라졌다.
		{ID: "sending-blind", CampaignID: c.ID, RecipientID: "p-blind", Name: "발송중무시도", Phone: "01000000003", Message: "본문", Status: Sending, UpdatedAt: base.Add(3 * time.Minute), CreatedAt: sentAt},
		{ID: "done", CampaignID: c.ID, RecipientID: "p-done", Name: "성공자", Phone: "01000000005", Message: "본문", Status: Sent, AttemptID: "done-attempt-1", SentAt: &sentAt, UpdatedAt: sentAt, CreatedAt: sentAt},
	}
	if _, e = s.collection(uid).Doc(c.ID).Set(ctx, document{Campaign: c, Recipients: rows}); e != nil {
		t.Fatal(e)
	}
	all, e := s.History(ctx, uid, "", 50, "", "", "")
	if e != nil || all.Total != 6 {
		t.Fatal("전원이 내려와야 한다", all.Total, e)
	}
	byID := map[string]int{}
	for i, h := range all.Items {
		byID[h.ID] = i
	}
	for _, tc := range []struct {
		why    string
		id     string
		status string
	}{
		{"한 번도 안 보낸 READY", "c1_ready-only#pending", Ready},
		{"재시도로 READY 가 된 사람의 지금 대기 줄", "c1_retried#pending", Ready},
		{"재시도로 READY 가 된 사람의 지난 실패 줄", "c1_retried_old-attempt-1", Failed},
		{"시도 중인 SENDING", "c1_sending-live_live-attempt-1", Sending},
		{"AttemptID 없는 SENDING", "c1_sending-blind#pending", Sending},
		{"완료된 SENT", "c1_done_done-attempt-1", Sent},
	} {
		i, ok := byID[tc.id]
		if !ok {
			t.Fatalf("%s 줄이 없다: %s (%v)", tc.why, tc.id, byID)
		}
		if all.Items[i].Status != tc.status {
			t.Fatalf("%s 상태가 %s 다", tc.why, all.Items[i].Status)
		}
	}
	// 대기 줄은 보낸 흔적을 달고 나오면 안 된다 — historyTime 이 UpdatedAt 을 봐야 한다.
	waiting := all.Items[byID["c1_ready-only#pending"]]
	if waiting.SentAt != nil || waiting.FailedAt != nil || waiting.ErrorCode != "" || !waiting.UpdatedAt.Equal(base) {
		t.Fatal("대기 줄에 발송 흔적이 남았다", waiting)
	}
	// 재시도자의 두 줄은 같은 사람이지만 ID 가 겹치지 않는다(겹치면 맵에서 하나가 사라진다).
	retried := 0
	for _, h := range all.Items {
		if h.RecipientID == "p-retry" {
			retried++
		}
	}
	if retried != 2 {
		t.Fatal("재시도자는 지난 실패 + 지금 대기 두 줄이다", retried)
	}
	// 검색도 같게 적용된다 — 아직 안 보낸 사람이 이름·번호로 찾아져야 한다.
	for _, tc := range []struct {
		q    string
		want int
	}{{"대기자", 1}, {"0001", 1}, {"재시도자", 2}, {"발송중", 2}} {
		got, e := s.History(ctx, uid, tc.q, 50, "", "", "")
		if e != nil || got.Total != tc.want {
			t.Fatalf("검색 q=%q → %d건 (%v)", tc.q, got.Total, e)
		}
	}
	// 기간은 historyTime 기준이고 양끝을 포함한다.
	rfc := func(at time.Time) string { return at.Format(time.RFC3339Nano) }
	kst := func(at time.Time) string { return at.In(time.FixedZone("KST", 9*3600)).Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		why  string
		from string
		to   string
		want []string
	}{
		{"from·to 같은 순간 — 양끝 포함이라 그 줄 하나", rfc(base), rfc(base), []string{"c1_ready-only#pending"}},
		{"오프셋 표기가 달라도 같은 순간", kst(base), kst(base), []string{"c1_ready-only#pending"}},
		{"from 포함", rfc(base.Add(time.Minute)), "", []string{"c1_sending-blind#pending", "c1_sending-live_live-attempt-1", "c1_retried#pending"}},
		{"from 을 1마이크로초 올리면 경계 줄이 빠진다", rfc(base.Add(time.Minute + time.Microsecond)), "", []string{"c1_sending-blind#pending", "c1_sending-live_live-attempt-1"}},
		{"to 포함", "", rfc(oldFailed), []string{"c1_retried_old-attempt-1", "c1_done_done-attempt-1"}},
		{"to 를 1마이크로초 내리면 경계 줄이 빠진다", "", rfc(oldFailed.Add(-time.Microsecond)), []string{"c1_done_done-attempt-1"}},
		{"기간 밖", rfc(base.Add(time.Hour)), rfc(base.Add(2 * time.Hour)), []string{}},
	} {
		got := sweepHistory(t, ctx, s, uid, "", tc.from, tc.to)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: %v (원한 것 %v)", tc.why, got, tc.want)
		}
	}
	// 기간을 준 채로도 커서가 빠짐·겹침 없이 끝까지 돈다.
	if got := sweepHistory(t, ctx, s, uid, "", rfc(sentAt), rfc(base.Add(3*time.Minute))); len(got) != 6 {
		t.Fatal("기간 + 커서 페이징", got)
	}
	// ⚠️ 커서는 기간에도 귀속된다. 기간만 바꾸고 커서를 재사용하면 앞 페이지가 다른 집합에서
	// 잘린 자리라 줄이 빠지거나 겹친다.
	page, e := s.History(ctx, uid, "", 1, "", rfc(sentAt), "")
	if e != nil || page.NextCursor == nil {
		t.Fatal(page, e)
	}
	if _, e = s.History(ctx, uid, "", 1, *page.NextCursor, "", ""); e != ErrValidation {
		t.Fatal("기간다른커서", e)
	}
	// 잘못된 값은 사유가 담긴 ValidationError 로 거절한다(고정 문구에 덮이면 안 된다).
	for _, tc := range []struct {
		why  string
		from string
		to   string
		hint string
	}{
		{"읽을 수 없는 from", "어제", "", "조회 시작일시"},
		{"날짜만 보낸 to — 시간대를 짐작하지 않는다", "", "2026-09-27", "조회 종료일시"},
		{"뒤바뀐 기간", rfc(base), rfc(base.Add(-time.Hour)), "바꿔서"},
		{"지나치게 긴 값", strings.Repeat("9", 65), "", "조회 시작일시"},
	} {
		_, e := s.History(ctx, uid, "", 50, "", tc.from, tc.to)
		var invalid messaging.ValidationError
		if !errors.As(e, &invalid) || !strings.Contains(invalid.Message, tc.hint) {
			t.Fatalf("%s: %v", tc.why, e)
		}
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
		hint string
	}{
		{"/sms/history?from=" + urlpkg.QueryEscape(rfc(base)) + "&to=" + urlpkg.QueryEscape(rfc(base)), 200, ""},
		{"/sms/history?from=어제", 400, "조회 시작일시"},
		{"/sms/history?to=2026-09-27", 400, "조회 종료일시"},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), tc.hint) {
			t.Fatal("사유가 사라졌다", tc.path, w.Body.String())
		}
	}
}
