package recipients

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/messaging"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrExternalConflict = errors.New("external send request changed")

type ExternalSendInput struct {
	RequestID string    `json:"requestId"`
	SentAt    time.Time `json:"sentAt"`
}
type ExternalSendResult struct {
	Recipient Recipient `json:"recipient" firestore:"recipient"`
	History   History   `json:"history" firestore:"history"`
}
type externalRecord struct {
	Result      ExternalSendResult `firestore:"result"`
	Fingerprint string             `firestore:"fingerprint"`
}

func ExternalCollection(fs *firestore.Client, uid string) *firestore.CollectionRef {
	return messaging.Collection(fs, uid, "externalSends")
}
func (s *Store) ExternalSend(ctx context.Context, uid, id string, in ExternalSendInput) (ExternalSendResult, bool, error) {
	var out ExternalSendResult
	created := false
	if !ValidateID(id) || !ValidateID(in.RequestID) || len(in.RequestID) < 8 || in.SentAt.IsZero() || in.SentAt.After(time.Now()) {
		return out, false, messaging.ValidationError{Message: "요청 ID와 발송일시를 확인해 주세요. 미래 일시는 등록할 수 없습니다."}
	}
	in.SentAt = in.SentAt.UTC().Truncate(time.Microsecond)
	encoded, _ := json.Marshal(struct {
		RecipientID string
		SentAt      time.Time
	}{id, in.SentAt})
	fingerprint := sha256.Sum256(encoded)
	key := sha256.Sum256([]byte(in.RequestID))
	ref := ExternalCollection(s.FS, uid).Doc(hex.EncodeToString(key[:]))
	baseline, e := s.legacyStats(ctx, uid)
	if e != nil {
		return out, false, e
	}
	e = s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		created = false
		out = ExternalSendResult{}
		doc, e := tx.Get(ref)
		if e == nil {
			var old externalRecord
			if e = doc.DataTo(&old); e != nil {
				return e
			}
			if old.Fingerprint != hex.EncodeToString(fingerprint[:]) {
				return ErrExternalConflict
			}
			out = old.Result
			if out.Recipient.CustomFields == nil {
				out.Recipient.CustomFields = []CustomField{}
			}
			if out.History.Attachments == nil {
				out.History.Attachments = []messaging.Attachment{}
			}
			return nil
		}
		if status.Code(e) != codes.NotFound {
			return e
		}
		recipientRef := DocumentRef(s.FS, uid, id)
		doc, e = tx.Get(recipientRef)
		if e != nil {
			return e
		}
		var r Recipient
		if e = doc.DataTo(&r); e != nil {
			return e
		}
		r.ID = id
		if r.CustomFields == nil {
			r.CustomFields = []CustomField{}
		}
		st := baseline[id]
		if st.Count > r.SentCount {
			r.SentCount = st.Count
		}
		r.SentCount++
		if !st.Latest.IsZero() && (r.LatestSentAt == nil || st.Latest.After(*r.LatestSentAt)) {
			r.LatestSentAt = &st.Latest
		}
		if r.LatestSentAt == nil || in.SentAt.After(*r.LatestSentAt) {
			r.LatestSentAt = &in.SentAt
		}
		now := time.Now().UTC()
		r.UpdatedAt = now
		h := History{ID: "external_" + ref.ID, Source: "EXTERNAL", CampaignTitle: "외부 발송 등록", RecipientID: id, Name: r.Name, Phone: StoredPhone(r.Phone), Status: "SENT", SentAt: &in.SentAt, CreatedAt: now, UpdatedAt: now, Attachments: []messaging.Attachment{}}
		// 전화번호 잠금을 건드리지 않도록 원본 수신자 번호는 그대로 두고 집계 필드만 갱신한다.
		if e = tx.Update(recipientRef, []firestore.Update{{Path: "sentCount", Value: r.SentCount}, {Path: "latestSentAt", Value: r.LatestSentAt}, {Path: "updatedAt", Value: r.UpdatedAt}}); e != nil {
			return e
		}
		r.Phone = StoredPhone(r.Phone)
		out = ExternalSendResult{r, h}
		if e = tx.Create(recipientRef.Collection("history").Doc(h.ID), h); e != nil {
			return e
		}
		if e = tx.Create(ref, externalRecord{Result: out, Fingerprint: hex.EncodeToString(fingerprint[:])}); e != nil {
			return e
		}
		created = true
		return nil
	})
	return out, created, e
}
func ExternalHistory(ctx context.Context, fs *firestore.Client, uid string) ([]History, error) {
	docs, e := ExternalCollection(fs, uid).Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	out := []History{}
	for _, d := range docs {
		var v externalRecord
		if e = d.DataTo(&v); e != nil {
			return nil, e
		}
		h := v.Result.History
		h.Source = "EXTERNAL"
		if h.Attachments == nil {
			h.Attachments = []messaging.Attachment{}
		}
		out = append(out, h)
	}
	return out, nil
}
func registerExternalSends(mux *http.ServeMux, h *handler, guard func(http.Handler) http.Handler) {
	mux.Handle("POST /recipients/{id}/external-sends", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		var in ExternalSendInput
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		out, created, e := h.store.ExternalSend(r.Context(), uid, r.PathValue("id"), in)
		if errors.Is(e, ErrExternalConflict) {
			httpx.WriteError(w, 409, "IDEMPOTENCY_CONFLICT", "같은 요청 ID의 내용이 다릅니다.")
			return
		}
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		code := 200
		if created {
			code = 201
		}
		httpx.WriteJSON(w, code, out)
	})))
}
