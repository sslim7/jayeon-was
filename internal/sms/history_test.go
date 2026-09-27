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
	"github.com/sslim7/nature-was/internal/recipients"
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
	// 🔴 StartedAt 이 있어야 이력에 들어온다 — 시작한 적 없는 캠페인은 통째로 빠진다.
	// 대기 줄의 시각은 r.UpdatedAt(sending)이 아니라 이 값(older)이 된다.
	c := Campaign{ID: "campaign-a", Title: "캠페인 제목", CreatedAt: older, StartedAt: &older}
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
	// 대기 줄 두 개는 StartedAt(older)에 모여 맨 뒤로 가고, 같은 시각이라 ID 내림차순으로 갈린다.
	want := []string{"campaign-a_pending_pending-attempt", "campaign-a_sent_sent-attempt", "campaign-a_retry_failed-attempt", "campaign-a_retry#pending", "campaign-a_never#pending"}
	if got := sweepHistory(t, ctx, s, uid, "", "", ""); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatal("정렬/페이징", got)
	}
	first, e := s.History(ctx, uid, "", 1, "", "", "")
	if e != nil || first.Total != 5 || len(first.Items) != 1 || first.Items[0].Status != Sending || first.NextCursor == nil {
		t.Fatal(first, e)
	}
	second, e := s.History(ctx, uid, "", 1, *first.NextCursor, "", "")
	if e != nil || len(second.Items) != 1 || second.Items[0].Status != Sent || second.NextCursor == nil {
		t.Fatal(second, e)
	}
	last, e := s.History(ctx, uid, "", 5, "", "", "")
	if e != nil || len(last.Items) != 5 || last.Items[2].Status != Failed || last.Items[2].ErrorCode != "NO_SERVICE" || last.Items[4].Status != Ready || last.NextCursor != nil {
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
	tied := []string{"campaign-z_sent_sent-attempt", "campaign-a_sent_sent-attempt", "campaign-a_retry_failed-attempt", "campaign-a_retry#pending", "campaign-a_never#pending"}
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
	// 🔴 대기 줄의 시각은 여기서 정해진다. 아래 수신자들의 UpdatedAt(base 이후)과 일부러 다르게
	// 두었다 — UpdatedAt 으로 되돌아가면 기간 필터·묶기가 조용히 어긋나는 것을 잡기 위해서다.
	startedAt := base.Add(-30 * time.Minute)
	c := Campaign{ID: "c1", Title: "기간 캠페인", CreatedAt: sentAt, StartedAt: &startedAt}
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
	// 대기 줄은 보낸 흔적을 달고 나오면 안 되고, 시각은 그 발송이 시작된 시각이어야 한다.
	waiting := all.Items[byID["c1_ready-only#pending"]]
	if waiting.SentAt != nil || waiting.FailedAt != nil || waiting.ErrorCode != "" {
		t.Fatal("대기 줄에 발송 흔적이 남았다", waiting)
	}
	// 🔴 레코드를 건드린 시각(base)이 아니라 발송 시작 시각(startedAt)이다. 되돌아가면
	// 「9월 23일 발송에서 안 나간 사람」이 오늘 자로 묶여 조용히 날짜가 틀어진다.
	if !waiting.UpdatedAt.Equal(startedAt) || waiting.UpdatedAt.Equal(base) {
		t.Fatalf("대기 줄 시각이 StartedAt 이 아니다: %v (원한 것 %v, 수신자 UpdatedAt %v)", waiting.UpdatedAt, startedAt, base)
	}
	// 같은 발송의 대기 줄은 전부 같은 시각에 모인다 — 화면이 하루·발송 단위로 묶을 수 있어야 한다.
	for _, id := range []string{"c1_retried#pending", "c1_sending-blind#pending"} {
		if got := all.Items[byID[id]].UpdatedAt; !got.Equal(startedAt) {
			t.Fatalf("%s 시각이 StartedAt 이 아니다: %v", id, got)
		}
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
		{"from·to 같은 순간 — 양끝 포함이라 그 줄 하나", rfc(oldFailed), rfc(oldFailed), []string{"c1_retried_old-attempt-1"}},
		{"오프셋 표기가 달라도 같은 순간", kst(oldFailed), kst(oldFailed), []string{"c1_retried_old-attempt-1"}},
		{"대기 줄은 전부 StartedAt 한 점에 모인다", rfc(startedAt), rfc(startedAt), []string{"c1_sending-blind#pending", "c1_retried#pending", "c1_ready-only#pending"}},
		{"from 포함", rfc(startedAt), "", []string{"c1_sending-live_live-attempt-1", "c1_retried_old-attempt-1", "c1_sending-blind#pending", "c1_retried#pending", "c1_ready-only#pending"}},
		{"from 을 1마이크로초 올리면 대기 줄이 통째로 빠진다", rfc(startedAt.Add(time.Microsecond)), "", []string{"c1_sending-live_live-attempt-1", "c1_retried_old-attempt-1"}},
		{"to 포함", "", rfc(oldFailed), []string{"c1_retried_old-attempt-1", "c1_sending-blind#pending", "c1_retried#pending", "c1_ready-only#pending", "c1_done_done-attempt-1"}},
		{"to 를 1마이크로초 내리면 경계 줄이 빠진다", "", rfc(oldFailed.Add(-time.Microsecond)), []string{"c1_sending-blind#pending", "c1_retried#pending", "c1_ready-only#pending", "c1_done_done-attempt-1"}},
		// 🔴 수신자 UpdatedAt(base~base+3분)만 걸치는 기간에는 대기 줄이 하나도 없어야 한다.
		// 여기서 대기 줄이 나오면 historyTime 이 다시 UpdatedAt 으로 돌아간 것이다.
		{"레코드를 건드린 시각으로는 걸리지 않는다", rfc(base), rfc(base.Add(3 * time.Minute)), []string{"c1_sending-live_live-attempt-1"}},
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

// 예약만 해 두고 한 번도 보내지 않은 캠페인은 발송이력에 설 자리가 없다. 그 사람들에게는
// 발송 일자라는 것이 아예 없어서, 넣어 두면 레코드를 마지막으로 건드린 시각이 발송일인 척
// 붙어 「9월 23일 · 미발송 39명」처럼 그날 아무 일도 겪지 않은 사람들이 그 날짜에 뜬다.
// 가르는 기준은 캠페인 상태가 아니라 StartedAt 하나다.
func TestHistoryOnlyIncludesStartedCampaigns(t *testing.T) {
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
	uid := fmt.Sprintf("started-history-%d", time.Now().UnixNano())
	base := time.Now().UTC().Truncate(time.Microsecond)
	// 🔴 수신자 UpdatedAt 은 전부 base 로 둔다. 대기 줄이 StartedAt 이 아니라 UpdatedAt 을
	// 집으면 네 캠페인의 대기 줄이 전부 지금 시각으로 뭉쳐 아래 기대값이 깨진다.
	startedAt := base.Add(-2 * time.Hour)
	startedSentAt := startedAt.Add(time.Minute)
	cancelStartedAt := base.Add(-time.Hour)
	cancelSentAt := cancelStartedAt.Add(time.Minute)

	// 예약만 걸어 둔 캠페인 — StartedAt 이 없다. 이 두 사람은 이력에 나오면 안 된다.
	reserved := Campaign{ID: "s-reserved", Title: "예약만", Status: Ready, Reserved: true, CreatedAt: base.Add(-3 * time.Hour), UpdatedAt: base}
	reservedRows := []CampaignRecipient{
		{ID: "never-1", CampaignID: reserved.ID, RecipientID: "p-n1", Name: "예약대기갑", Phone: "01000000011", Message: "본문", Status: Ready, UpdatedAt: base, CreatedAt: base.Add(-3 * time.Hour)},
		{ID: "never-2", CampaignID: reserved.ID, RecipientID: "p-n2", Name: "예약대기을", Phone: "01000000012", Message: "본문", Status: Ready, UpdatedAt: base, CreatedAt: base.Add(-3 * time.Hour)},
	}
	if _, e = s.collection(uid).Doc(reserved.ID).Set(ctx, document{Campaign: reserved, Recipients: reservedRows}); e != nil {
		t.Fatal(e)
	}
	// 시작해서 아직 도는 중인 캠페인 — 완료 시도 줄과 대기 줄이 둘 다 나온다.
	started := Campaign{ID: "s-started", Title: "진행중", Status: Sending, CreatedAt: startedAt, UpdatedAt: base, StartedAt: &startedAt}
	startedRows := []CampaignRecipient{
		{ID: "r-wait", CampaignID: started.ID, RecipientID: "p-w", Name: "진행대기자", Phone: "01000000021", Message: "본문", Status: Ready, UpdatedAt: base, CreatedAt: startedAt},
		{ID: "r-sent", CampaignID: started.ID, RecipientID: "p-s", Name: "진행성공자", Phone: "01000000022", Message: "본문", Status: Sent, AttemptID: "s-attempt-1", SentAt: &startedSentAt, UpdatedAt: base, CreatedAt: startedAt},
	}
	if _, e = s.collection(uid).Doc(started.ID).Set(ctx, document{Campaign: started, Recipients: startedRows}); e != nil {
		t.Fatal(e)
	}
	// ⚠️ 시작했다가 취소한 캠페인 — 그날 실제로 발송을 돌렸고 남은 사람이 못 나갔다.
	// 그 날짜의 「미발송」이 맞으므로 CANCELLED 라는 이유로 빼면 안 된다.
	cancelled := Campaign{ID: "s-cancelled", Title: "돌리다취소", Status: Cancelled, CreatedAt: cancelStartedAt, UpdatedAt: base, StartedAt: &cancelStartedAt}
	cancelledRows := []CampaignRecipient{
		{ID: "c-left", CampaignID: cancelled.ID, RecipientID: "p-l", Name: "취소남은자", Phone: "01000000031", Message: "본문", Status: Ready, UpdatedAt: base, CreatedAt: cancelStartedAt},
		{ID: "c-sent", CampaignID: cancelled.ID, RecipientID: "p-cs", Name: "취소전성공자", Phone: "01000000032", Message: "본문", Status: Sent, AttemptID: "c-attempt-1", SentAt: &cancelSentAt, UpdatedAt: base, CreatedAt: cancelStartedAt},
	}
	if _, e = s.collection(uid).Doc(cancelled.ID).Set(ctx, document{Campaign: cancelled, Recipients: cancelledRows}); e != nil {
		t.Fatal(e)
	}

	all, e := s.History(ctx, uid, "", 50, "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"s-cancelled_c-sent_c-attempt-1", "s-cancelled_c-left#pending", "s-started_r-sent_s-attempt-1", "s-started_r-wait#pending"}
	got := []string{}
	for _, h := range all.Items {
		got = append(got, h.ID)
	}
	if all.Total != len(want) || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("시작된 발송만 나와야 한다: %v (원한 것 %v)", got, want)
	}
	// 🔴 예약만 해 둔 사람은 이름으로 찾아도 없어야 한다 — 화면의 예약 목록이 맡는 사람들이다.
	for _, q := range []string{"예약대기갑", "0011", "예약대기을"} {
		page, e := s.History(ctx, uid, q, 50, "", "", "")
		if e != nil || page.Total != 0 {
			t.Fatalf("예약만 한 사람이 이력에 나왔다: q=%q → %d건 (%v)", q, page.Total, e)
		}
	}
	byID := map[string]recipients.History{}
	for _, h := range all.Items {
		byID[h.ID] = h
	}
	// 🔴 대기 줄의 시각은 그 발송이 시작된 시각이다. 수신자 UpdatedAt(base)이 아니다.
	for _, tc := range []struct {
		id   string
		want time.Time
	}{{"s-started_r-wait#pending", startedAt}, {"s-cancelled_c-left#pending", cancelStartedAt}} {
		h := byID[tc.id]
		if !h.UpdatedAt.Equal(tc.want) || h.UpdatedAt.Equal(base) {
			t.Fatalf("%s 시각이 StartedAt 이 아니다: %v (원한 것 %v, 수신자 UpdatedAt %v)", tc.id, h.UpdatedAt, tc.want, base)
		}
		if h.SentAt != nil || h.FailedAt != nil {
			t.Fatalf("%s 는 나가지도 실패하지도 않았다: %v", tc.id, h)
		}
		if h.Status != Ready {
			t.Fatalf("%s 상태가 %s 다", tc.id, h.Status)
		}
	}
	// 기간 필터도 StartedAt 을 본다 — 취소된 그날의 발송만 잘라 낼 수 있어야 한다.
	rfc := func(at time.Time) string { return at.Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		why  string
		from string
		to   string
		want []string
	}{
		{"취소된 발송이 일어난 구간", rfc(cancelStartedAt), "", []string{"s-cancelled_c-sent_c-attempt-1", "s-cancelled_c-left#pending"}},
		{"시작 시각 하나에 걸리는 대기 줄", rfc(startedAt), rfc(startedAt), []string{"s-started_r-wait#pending"}},
		{"레코드를 건드린 시각으로는 아무것도 안 걸린다", rfc(base), "", []string{}},
	} {
		if swept := sweepHistory(t, ctx, s, uid, "", tc.from, tc.to); strings.Join(swept, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: %v (원한 것 %v)", tc.why, swept, tc.want)
		}
	}
	// 커서로 끝까지 돌아도 빠짐·겹침이 없다(대기 줄이 줄면서 페이징 경계가 달라졌다).
	if swept := sweepHistory(t, ctx, s, uid, "", "", ""); strings.Join(swept, ",") != strings.Join(want, ",") {
		t.Fatalf("커서 스윕: %v (원한 것 %v)", swept, want)
	}
}
