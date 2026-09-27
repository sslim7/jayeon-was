package sms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/sslim7/nature-was/internal/recipients"
)

type HistoryPage struct {
	Items      []recipients.History `json:"items"`
	NextCursor *string              `json:"nextCursor"`
	Total      int                  `json:"total"`
}
type historyCursor struct {
	UserID string    `json:"u"`
	Query  string    `json:"q"`
	From   string    `json:"fr"`
	To     string    `json:"to"`
	At     time.Time `json:"t"`
	ID     string    `json:"id"`
}

func historyTime(h recipients.History) time.Time {
	if h.SentAt != nil {
		return *h.SentAt
	}
	if h.FailedAt != nil {
		return *h.FailedAt
	}
	return h.UpdatedAt
}
func historyItem(c Campaign, r CampaignRecipient) recipients.History {
	attachments := r.Attachments
	if attachments == nil {
		attachments = []messaging.Attachment{}
	}
	return recipients.History{Source: "ANDROID", ID: c.ID + "_" + r.ID + "_" + r.AttemptID, CampaignID: c.ID, CampaignTitle: c.Title, RecipientID: r.RecipientID, Name: r.Name, Phone: r.Phone, Message: r.Message, Status: r.Status, Transport: r.Transport, SentAt: r.SentAt, FailedAt: r.FailedAt, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Attachments: attachments}
}

// pendingHistoryID 는 아직 완료된 시도가 없는 사람의 「대기 줄」 ID다.
//
// 🔴 보통 규칙(c.ID+"_"+r.ID+"_"+AttemptID)을 그대로 쓰면 안 된다. 대기 줄은 AttemptID 가
// 비어 있어서 키가 c.ID+"_"+r.ID+"_" 로 끝나는데, 이러면 같은 사람의 다른 줄과 키가 겹칠 수
// 있고 merged 맵에서 **조용히 서로를 덮어쓴다** — 지난 실패 기록이 오류 하나 없이 사라진다.
// AttemptID 는 identifier([A-Za-z0-9_-]{8,128})라 '#' 을 절대 담을 수 없으므로, '#' 으로
// 갈라 두면 어떤 시도 ID 와도 겹치지 않는다.
func pendingHistoryID(c Campaign, r CampaignRecipient) string {
	return c.ID + "_" + r.ID + "#pending"
}

// 기간 파라미터가 읽히지 않을 때 화면에 그대로 보여 줄 예시다.
const historyBoundExample = "2026-09-27T00:00:00+09:00"

// parseHistoryBound 는 from/to 한 개를 읽는다.
//
// 🔴 「YYYY-MM-DD」가 아니라 RFC3339 로 고정했다. 화면은 KST 기준으로 하루씩 묶는데 날짜만
// 오면 서버가 어느 시간대의 하루인지 짐작해야 한다. 짐작이 9시간 어긋나도 아무 오류가 안 나고
// 경계에 걸린 발송만 옆 날짜로 조용히 새 나간다. 오프셋을 달고 오면 짐작할 것이 없다.
//
// ⚠️ 거절은 ErrValidation 이 아니라 사유가 담긴 messaging.ValidationError 다. fail() 이
// 이 문장을 그대로 400 으로 내보내므로, 「요청 값이 올바르지 않아요」 로 덮이지 않는다.
func parseHistoryBound(label, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if len(value) > 64 || !utf8.ValidString(value) {
		return time.Time{}, messaging.ValidationError{Message: fmt.Sprintf("%s 값이 너무 길어요. 「%s」 같은 형식으로 보내 주세요.", label, historyBoundExample)}
	}
	at, e := time.Parse(time.RFC3339, value)
	if e != nil {
		return time.Time{}, messaging.ValidationError{Message: fmt.Sprintf("%s 「%s」을(를) 읽을 수 없어요. 「%s」 처럼 시간대까지 붙여서 보내 주세요.", label, value, historyBoundExample)}
	}
	return at.UTC(), nil
}

// boundKey 는 커서에 박아 둘 기간의 정규화 표기다. 같은 순간을 가리키는 다른 표기
// (+09:00 과 Z)가 서로 다른 커서로 갈리지 않게 UTC 한 가지로 적는다.
func boundKey(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

// History는 계정 하위 캠페인만 읽고 완료 시도 문서와 이전 버전 스냅샷을 합친다.
// 원본 수신자 삭제나 재시도 후 READY 변경이 과거 발송 이력을 지우지 않는다.
//
// 🔴 **한 번이라도 시작된 발송(Campaign.StartedAt != nil)의, 실제로 시도한 사람만** 들어온다.
// 성공·실패·통과·중단은 전부 시도가 있었던 것이고, 결과를 못 받은 SENDING 도 마찬가지다.
// 아직 차례가 오지 않은 READY 는 들어오지 않는다 — 이력은 「보낸 것」의 기록이라, 7명 예약에서
// 1명만 보냈을 때 나머지 6명이 그날의 미발송으로 섞이면 이미 보낸 사람과 구분이 사라진다.
// 그 6명은 **예약 목록**이 맡는다(아직 안 보낸 사람이 남은 캠페인을 그쪽이 모은다).
//
// 예약만 해 두고 한 번도 보내지 않은 캠페인은 통째로 뺀다. 그 사람들에게는 **발송 일자라는 것이
// 아예 없는데**, 넣어 두면 historyTime() 이 UpdatedAt(레코드를 마지막으로 건드린 시각)을 집어
// 「9월 23일 · 미발송 39명」처럼 그날 아무 일도 겪지 않은 사람들이 그 날짜에 붙어 버린다.
// 예약은 「발송 이력」이 아니라 화면의 예약 목록이 맡는다.
//
// from/to 는 historyTime() 기준이다. 🔴 정렬·묶기와 **같은 시각**을 봐야 「목록엔 있는데
// 기간에서 빠진다」가 안 생긴다. 다른 시각(CreatedAt 등)으로 자르면 아무 오류 없이 어긋난다.
func (s *FirestoreStore) History(ctx context.Context, uid, q string, limit int, cursor, fromRaw, toRaw string) (HistoryPage, error) {
	out := HistoryPage{Items: []recipients.History{}}
	q = strings.ToLower(strings.TrimSpace(q))
	if limit < 1 || limit > 100 || !utf8.ValidString(q) || utf8.RuneCountInString(q) > 100 || len(cursor) > 2048 {
		return out, ErrValidation
	}
	from, e := parseHistoryBound("조회 시작일시(from)", fromRaw)
	if e != nil {
		return out, e
	}
	to, e := parseHistoryBound("조회 종료일시(to)", toRaw)
	if e != nil {
		return out, e
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return out, messaging.ValidationError{Message: "조회 시작일시가 종료일시보다 늦어요. from 과 to 를 바꿔서 보내 주세요."}
	}
	var boundary historyCursor
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		// ⚠️ 커서는 사용자·검색어에 더해 **기간에도** 귀속된다. 기간만 바꾸고 커서를 그대로 쓰면
		// 앞 페이지가 다른 집합에서 잘린 자리라 줄이 빠지거나 겹친다. 거절해서 처음부터 받게 한다.
		if e != nil || json.Unmarshal(b, &boundary) != nil || boundary.UserID != uid || boundary.Query != q || boundary.From != boundKey(from) || boundary.To != boundKey(to) || boundary.At.IsZero() || boundary.ID == "" {
			return out, ErrValidation
		}
	}
	docs, e := s.collection(uid).Documents(ctx).GetAll()
	if e != nil {
		return out, e
	}
	merged := map[string]recipients.History{}
	for _, doc := range docs {
		var d document
		if e = doc.DataTo(&d); e != nil {
			return out, e
		}
		d.Campaign.ID = doc.Ref.ID
		// 🔴 발송이 시작된 적 없는 캠페인은 시도 줄도 대기 줄도 만들지 않는다. 「발송 이력」은
		// 실제로 일어난 발송의 기록이라, 아직 아무 일도 없었던 예약은 설 자리가 없다.
		// ⚠️ 상태(Status)로 거르면 안 된다. 시작했다가 CANCELLED 된 캠페인은 그날 실제로 발송을
		// 돌렸고 남은 사람이 못 나간 것이므로 그 날짜의 「미발송」이 맞다 — StartedAt 하나로 가른다.
		started := d.Campaign.StartedAt
		if started == nil {
			continue
		}
		attempts, e := doc.Ref.Collection("attempts").Documents(ctx).GetAll()
		if e != nil {
			return out, e
		}
		for _, a := range attempts {
			var r CampaignRecipient
			if e = a.DataTo(&r); e != nil {
				return out, e
			}
			if r.Status != Sent && r.Status != Failed {
				continue
			}
			h := historyItem(d.Campaign, r)
			merged[h.ID] = h
		}
		for _, r := range d.Recipients {
			// 이전 버전의 시도 상세 문서가 없으면 캠페인의 attempt 타임스탬프로 최소 이력을 보강한다.
			for _, a := range r.Attempts {
				if a.Status != Sent && a.Status != Failed {
					continue
				}
				copy := r
				copy.AttemptID = a.ID
				copy.Status = a.Status
				copy.SentAt = nil
				copy.FailedAt = nil
				copy.ErrorCode = ""
				copy.ErrorMessage = ""
				copy.Transport = ""
				copy.UpdatedAt = a.StartedAt
				if a.FinishedAt != nil {
					copy.UpdatedAt = *a.FinishedAt
					if a.Status == Sent {
						copy.SentAt = a.FinishedAt
					} else {
						copy.FailedAt = a.FinishedAt
					}
				}
				h := historyItem(d.Campaign, copy)
				if _, exists := merged[h.ID]; !exists {
					merged[h.ID] = h
				}
			}
			// 🔴 **아직 차례가 오지 않은 사람(READY)은 이력에 넣지 않는다.** 이력은 「보낸 것」의
			// 기록이고, 그 사람에게는 발송 일자라는 것이 아직 없다. 7명 예약에서 1명만 보냈을 때
			// 나머지 6명이 그날의 「미발송」으로 딸려 들어와, 이미 보낸 사람과 한 줄에 섞여 보였다.
			// 그 6명이 있어야 할 곳은 **예약 목록**이다(→ `jayeon-app` 의 `reservedCampaigns`,
			// 「아직 안 보낸 사람이 남았는가」로 고른다).
			//
			// ⚠️ 다만 **AttemptID 조차 없는 SENDING 은 넣는다.** 그 사람은 보내려고 잠갔는데 결과가
			// 돌아오지 않은 것이라 **시도가 실제로 있었다.** 화면이 「발송중」으로 세우고 사람이
			// 풀어 줘야 하는 자리이므로, 이력에서 빠지면 그 일이 있었다는 사실 자체가 사라진다.
			if r.Status == Sending && r.AttemptID == "" {
				waiting := r
				// 재시도가 이 값들을 지우지만 옛 문서에는 남아 있을 수 있다. 남겨 두면 historyTime 이
				// SentAt 을 집어 「아직 안 보낸 줄」이 예전에 보낸 시각 자리에 꽂힌다.
				waiting.SentAt = nil
				waiting.FailedAt = nil
				waiting.ErrorCode = ""
				waiting.ErrorMessage = ""
				waiting.Transport = ""
				// 🔴 대기 줄의 시각은 **그 발송이 시작된 시각**이다. r.UpdatedAt 을 쓰면 안 된다 —
				// 그 값은 레코드를 마지막으로 건드린 시각이라 재시도로 READY 로 되돌린 순간이나
				// 취소한 순간이 들어온다. 그러면 아무 오류 없이 「오늘 · 미발송 16명」으로 떠서,
				// 실제로는 9월 23일 발송에서 안 나간 사람이 오늘 발송인 척 섞인다.
				// ⚠️ SentAt·FailedAt 은 계속 nil 이다 — 나가지도 실패하지도 않았다. 시각만
				// StartedAt 으로 채워 historyTime() 이 그 발송의 날짜로 묶게 한다.
				waiting.UpdatedAt = *started
				h := historyItem(d.Campaign, waiting)
				h.ID = pendingHistoryID(d.Campaign, r)
				merged[h.ID] = h
			}
			if r.Status != Sent && r.Status != Failed && r.Status != Sending {
				continue
			}
			if r.Status == Sending && r.AttemptID == "" {
				continue
			}
			h := historyItem(d.Campaign, r)
			// 두 문서 조회 사이 결과가 확정되었으면 오래된 SENDING/FAILED 스냅샷으로 되돌리지 않는다.
			rank := func(status string) int {
				switch status {
				case Sent:
					return 3
				case Failed:
					return 2
				case Sending:
					return 1
				}
				return 0
			}
			old, exists := merged[h.ID]
			if !exists || rank(h.Status) >= rank(old.Status) {
				merged[h.ID] = h
			}
		}
	}
	external, e := recipients.ExternalHistory(ctx, s.Client, uid)
	if e != nil {
		return out, e
	}
	for _, h := range external {
		merged[h.ID] = h
	}
	all := []recipients.History{}
	for _, h := range merged {
		// 이름만 보던 자리다. 앱의 검색칸이 「이름 또는 폰번호 뒷4자리」로 통일된 뒤로는
		// 숫자를 쳐도 한 건도 안 나왔다. 규칙은 recipients.MatchesQuery 한 곳에만 둔다.
		if !recipients.MatchesQuery(q, h.Name, h.Phone) {
			continue
		}
		// 기간은 양끝 포함이다(from 이상, to 이하). 화면의 「9월 27일」은 그 날 00:00+09:00 부터
		// 23:59:59.999+09:00 까지를 보내면 된다.
		at := historyTime(h)
		if !from.IsZero() && at.Before(from) {
			continue
		}
		if !to.IsZero() && at.After(to) {
			continue
		}
		all = append(all, h)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := historyTime(all[i]), historyTime(all[j])
		if a.Equal(b) {
			return all[i].ID > all[j].ID
		}
		return a.After(b)
	})
	out.Total = len(all)
	for _, h := range all {
		at := historyTime(h)
		// 🔴 대기 줄이 늘면서 같은 시각(UpdatedAt)에 여러 줄이 몰린다. 정렬이 (시각, ID) 로
		// 완전순서를 이루고 여기 건너뛰기 조건이 그 순서와 정확히 같아야 페이지 경계에서 줄이
		// 빠지거나 겹치지 않는다. ID 는 merged 맵의 키라 전역에서 유일하다.
		if cursor != "" && (at.After(boundary.At) || at.Equal(boundary.At) && h.ID >= boundary.ID) {
			continue
		}
		if len(out.Items) == limit {
			last := out.Items[len(out.Items)-1]
			b, _ := json.Marshal(historyCursor{uid, q, boundKey(from), boundKey(to), historyTime(last), last.ID})
			v := base64.RawURLEncoding.EncodeToString(b)
			out.NextCursor = &v
			break
		}
		out.Items = append(out.Items, h)
	}
	return out, nil
}
func registerHistory(mux *http.ServeMux, fs *firestore.Client, guard func(http.Handler) http.Handler) {
	s := &FirestoreStore{Client: fs}
	mux.Handle("GET /sms/history", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		uid := auth.UserID(r.Context())
		if uid == "" {
			httpx.WriteError(w, 401, httpx.CodeUnauthorized, "로그인이 필요해요")
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil {
				fail(w, ErrValidation)
				return
			}
			limit = n
		}
		out, e := s.History(r.Context(), uid, r.URL.Query().Get("q"), limit, r.URL.Query().Get("cursor"), r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		if e != nil {
			fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, out)
	})))
}
