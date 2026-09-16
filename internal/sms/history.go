package sms

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// History는 계정 하위 캠페인만 읽고 완료 시도 문서와 이전 버전 스냅샷을 합친다.
// 원본 수신자 삭제나 재시도 후 READY 변경이 과거 발송 이력을 지우지 않는다.
func (s *FirestoreStore) History(ctx context.Context, uid, q string, limit int, cursor string) (HistoryPage, error) {
	out := HistoryPage{Items: []recipients.History{}}
	q = strings.ToLower(strings.TrimSpace(q))
	if limit < 1 || limit > 100 || !utf8.ValidString(q) || utf8.RuneCountInString(q) > 100 || len(cursor) > 2048 {
		return out, ErrValidation
	}
	var boundary historyCursor
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(b, &boundary) != nil || boundary.UserID != uid || boundary.Query != q || boundary.At.IsZero() || boundary.ID == "" {
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
		if q == "" || strings.Contains(strings.ToLower(h.Name), q) {
			all = append(all, h)
		}
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
		if cursor != "" && (at.After(boundary.At) || at.Equal(boundary.At) && h.ID >= boundary.ID) {
			continue
		}
		if len(out.Items) == limit {
			last := out.Items[len(out.Items)-1]
			b, _ := json.Marshal(historyCursor{uid, q, historyTime(last), last.ID})
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
		out, e := s.History(r.Context(), uid, r.URL.Query().Get("q"), limit, r.URL.Query().Get("cursor"))
		if e != nil {
			fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, out)
	})))
}
