package recipients

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/xuri/excelize/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type History struct {
	Source        string                 `json:"source" firestore:"source"`
	ID            string                 `json:"id" firestore:"id"`
	CampaignID    string                 `json:"campaignId,omitempty" firestore:"campaignId"`
	CampaignTitle string                 `json:"campaignTitle" firestore:"campaignTitle"`
	RecipientID   string                 `json:"recipientId" firestore:"recipientId"`
	Name          string                 `json:"name" firestore:"name"`
	Phone         string                 `json:"phone" firestore:"phone"`
	Message       string                 `json:"message" firestore:"message"`
	Status        string                 `json:"status" firestore:"status"`
	Transport     string                 `json:"transport,omitempty" firestore:"transport"`
	SentAt        *time.Time             `json:"sentAt" firestore:"sentAt"`
	FailedAt      *time.Time             `json:"failedAt" firestore:"failedAt"`
	ErrorCode     string                 `json:"errorCode" firestore:"errorCode"`
	ErrorMessage  string                 `json:"errorMessage" firestore:"errorMessage"`
	CreatedAt     time.Time              `json:"createdAt" firestore:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt" firestore:"updatedAt"`
	Attachments   []messaging.Attachment `json:"attachments" firestore:"attachments"`
}
type ImportRow struct {
	CustomFields []CustomField `json:"customFields" firestore:"customFields"`
	Row          int           `json:"row" firestore:"row"`
	Name         string        `json:"name" firestore:"name"`
	Phone        string        `json:"phone" firestore:"phone"`
	GroupID      string        `json:"groupId" firestore:"groupId"`
	Status       string        `json:"status" firestore:"status"`
	Reason       string        `json:"reason" firestore:"reason"`
}
type Import struct {
	ID            string      `json:"id" firestore:"id"`
	AddedCount    int         `json:"addedCount" firestore:"addedCount"`
	ExcludedCount int         `json:"excludedCount" firestore:"excludedCount"`
	Items         []ImportRow `json:"items" firestore:"items"`
	CreatedAt     time.Time   `json:"createdAt" firestore:"createdAt"`
	Confirmed     bool        `json:"confirmed" firestore:"confirmed"`
}

func (v *Import) count() {
	v.AddedCount = 0
	v.ExcludedCount = 0
	for _, r := range v.Items {
		if r.Status == "ADD" {
			v.AddedCount++
		} else {
			v.ExcludedCount++
		}
	}
}
func parseXLSX(data []byte) (out []ImportRow, err error) {
	// 손상된 외부 파일이 파서에서 panic을 일으켜도 요청 단위 검증 오류로 제한한다.
	defer func() {
		if recover() != nil {
			out = nil
			err = messaging.ErrInvalid
		}
	}()
	f, e := excelize.OpenReader(bytes.NewReader(data), excelize.Options{UnzipSizeLimit: 20 << 20, UnzipXMLSizeLimit: 10 << 20})
	if e != nil {
		return nil, messaging.ErrInvalid
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, messaging.ErrInvalid
	}
	rows, e := f.Rows(sheets[0])
	if e != nil {
		return nil, messaging.ErrInvalid
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, messaging.ErrInvalid
	}
	header, e := rows.Columns()
	if e != nil {
		return nil, messaging.ErrInvalid
	}
	cols := map[string]int{}
	extra := []int{}
	headers := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" || headers[h] {
			return nil, messaging.ErrInvalid
		}
		headers[h] = true
		header[i] = h
		key := ""
		switch h {
		case "이름", "name":
			key = "name"
		case "전화번호", "연락처", "phone":
			key = "phone"
		case "그룹", "groupId":
			key = "groupId"
		}
		if key != "" {
			if _, exists := cols[key]; exists {
				return nil, messaging.ErrInvalid
			}
			cols[key] = i
		} else {
			extra = append(extra, i)
		}
	}
	if len(extra) > 20 {
		return nil, messaging.ErrInvalid
	}
	for _, key := range []string{"name", "phone"} {
		if _, ok := cols[key]; !ok {
			return nil, messaging.ErrInvalid
		}
	}
	seen := map[string]bool{}
	out = []ImportRow{}
	rowNo := 1
	for rows.Next() {
		rowNo++
		if rowNo > 201 {
			return nil, messaging.ErrInvalid
		}
		cells, e := rows.Columns()
		if e != nil {
			return nil, messaging.ErrInvalid
		}
		get := func(k string) string {
			i, ok := cols[k]
			if !ok || i >= len(cells) {
				return ""
			}
			return strings.TrimSpace(cells[i])
		}
		if len(cells) > len(header) {
			return nil, messaging.ErrInvalid
		}
		fields := []CustomField{}
		for _, i := range extra {
			v := ""
			if i < len(cells) {
				v = cells[i]
			}
			fields = append(fields, CustomField{Name: header[i], Value: v})
		}
		in := Input{Name: get("name"), Phone: get("phone"), GroupID: get("groupId"), CustomFields: fields}
		blank := true
		for _, v := range cells {
			if strings.TrimSpace(v) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		r := ImportRow{CustomFields: fields, Row: rowNo, Name: in.Name, Phone: in.Phone, GroupID: in.GroupID, Status: "ADD"}
		in, e = validate(in)
		if e != nil {
			r.Status = "EXCLUDED"
			r.Reason = e.Error()
		} else {
			r.Name = in.Name
			r.Phone = in.Phone
			r.GroupID = in.GroupID
			if seen[in.Phone] {
				r.Status = "EXCLUDED"
				r.Reason = "파일 내 중복 전화번호"
			}
			seen[in.Phone] = true
		}
		out = append(out, r)
	}
	if e = rows.Error(); e != nil {
		return nil, messaging.ErrInvalid
	}
	if len(out) == 0 {
		return nil, messaging.ErrInvalid
	}
	return out, nil
}
func (s *Store) Preview(ctx context.Context, uid string, in messaging.Upload) (Import, error) {
	data, e := base64.StdEncoding.DecodeString(in.DataBase64)
	if e != nil || len(data) > 2<<20 || !strings.HasSuffix(strings.ToLower(in.Name), ".xlsx") {
		return Import{}, messaging.ErrInvalid
	}
	rows, e := parseXLSX(data)
	if e != nil {
		return Import{}, e
	}
	for i := range rows {
		if rows[i].Status != "ADD" {
			continue
		}
		for _, key := range phoneKeys(rows[i].Phone) {
			_, e = phoneRef(s.FS, uid, key).Get(ctx)
			if e == nil {
				rows[i].Status = "EXCLUDED"
				rows[i].Reason = "이미 등록된 전화번호"
				break
			} else if status.Code(e) != codes.NotFound {
				return Import{}, e
			}
		}

	}
	ref := messaging.Collection(s.FS, uid, "recipientImports").NewDoc()
	out := Import{ID: ref.ID, Items: rows, CreatedAt: time.Now().UTC()}
	out.count()
	encoded, e := json.Marshal(out)
	if e != nil {
		return Import{}, e
	}
	if len(encoded) > 750<<10 {
		return Import{}, messaging.ValidationError{Message: "추가 항목을 포함한 가져오기 결과가 750KiB를 초과합니다. 파일을 나누어 가져와 주세요."}
	}
	_, e = ref.Create(ctx, out)
	return out, e
}
func (s *Store) Confirm(ctx context.Context, uid, id string) (Import, error) {
	if !ValidateID(id) {
		return Import{}, messaging.ErrInvalid
	}
	ref := messaging.Collection(s.FS, uid, "recipientImports").Doc(id)
	var out Import
	e := s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, e := tx.Get(ref)
		if e != nil {
			return e
		}
		out = Import{}
		if e = doc.DataTo(&out); e != nil {
			return e
		}
		if out.Confirmed {
			return nil
		}
		if time.Since(out.CreatedAt) > 24*time.Hour {
			return messaging.ErrInvalid
		}
		if len(out.Items) > 200 {
			return messaging.ErrInvalid
		}
		// 저장된 미리보기가 이전 버전에서 만들어졌어도 확정 시 최신 입력 규칙을 다시 적용한다.
		seen := map[string]bool{}
		for i := range out.Items {
			r := &out.Items[i]
			if r.Status != "ADD" {
				continue
			}
			in, validationErr := validate(Input{Name: r.Name, Phone: r.Phone, GroupID: r.GroupID, CustomFields: r.CustomFields})
			if validationErr != nil {
				r.Status = "EXCLUDED"
				r.Reason = "확정 시 검증 실패: " + validationErr.Error()
				continue
			}
			if seen[in.Phone] {
				r.Status = "EXCLUDED"
				r.Reason = "확정 시 파일 내 중복 전화번호"
				continue
			}
			seen[in.Phone] = true
			r.Name = in.Name
			r.Phone = in.Phone
			r.GroupID = in.GroupID
			r.CustomFields = in.CustomFields
			if r.CustomFields == nil {
				r.CustomFields = []CustomField{}
			}
		}
		// 모든 번호 잠금을 먼저 읽고 나서 쓰기 시작한다. 다른 import/수동등록과 경합해도 중복되지 않는다.
		for i := range out.Items {
			r := &out.Items[i]
			if r.Status != "ADD" {
				continue
			}
			for _, key := range phoneKeys(r.Phone) {
				_, e = tx.Get(phoneRef(s.FS, uid, key))
				if e == nil {
					r.Status = "EXCLUDED"
					r.Reason = "확정 시 이미 등록된 전화번호"
					break
				} else if status.Code(e) != codes.NotFound {
					return e
				}
			}

		}
		now := time.Now().UTC()
		for _, r := range out.Items {
			if r.Status != "ADD" {
				continue
			}
			dest := collection(s.FS, uid).NewDoc()
			item := Recipient{CustomFields: r.CustomFields, ID: dest.ID, Name: r.Name, Phone: r.Phone, GroupID: r.GroupID, CreatedAt: now, UpdatedAt: now}
			if e = tx.Create(dest, item); e != nil {
				return e
			}
			if e = tx.Set(phoneRef(s.FS, uid, r.Phone), map[string]any{"recipientId": dest.ID}); e != nil {
				return e
			}
		}
		out.Confirmed = true
		out.count()
		return tx.Set(ref, out)
	})
	return out, e
}
func (s *Store) History(ctx context.Context, uid, id string) ([]History, error) {
	if !ValidateID(id) {
		return nil, messaging.ErrInvalid
	}
	docs, e := DocumentRef(s.FS, uid, id).Collection("history").Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	items := []History{}
	seen := map[string]bool{}
	for _, d := range docs {
		var h History
		if e = d.DataTo(&h); e != nil {
			return nil, e
		}
		if h.Attachments == nil {
			h.Attachments = []messaging.Attachment{}
		}
		if h.Source == "" {
			h.Source = "ANDROID"
		}
		items = append(items, h)
		seen[h.ID] = true
	}
	// 확장 이전 캠페인의 최종 결과도 스냅샷으로 읽는다. 원본 수신자 수정/삭제와 무관하다.
	campaigns, e := messaging.Collection(s.FS, uid, "smsCampaigns").Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	for _, d := range campaigns {
		var legacy struct {
			Campaign struct {
				ID    string `firestore:"id"`
				Title string `firestore:"title"`
			} `firestore:"campaign"`
			Recipients []struct {
				History
				AttemptID string `firestore:"attemptId"`
			} `firestore:"recipients"`
		}
		if e = d.DataTo(&legacy); e != nil {
			return nil, e
		}
		for _, r := range legacy.Recipients {
			if r.RecipientID != id || r.Status != "SENT" && r.Status != "FAILED" {
				continue
			}
			h := r.History
			h.ID = d.Ref.ID + "_" + h.ID + "_" + r.AttemptID
			if seen[h.ID] {
				continue
			}
			h.Source = "ANDROID"
			h.CampaignTitle = legacy.Campaign.Title
			h.CampaignID = d.Ref.ID
			if h.Attachments == nil {
				h.Attachments = []messaging.Attachment{}
			}
			items = append(items, h)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := HistoryTime(items[i]), HistoryTime(items[j])
		if a.Equal(b) {
			return items[i].ID > items[j].ID
		}
		return a.After(b)
	})
	return items, nil
}
func registerExtensions(mux *http.ServeMux, h *handler, guard func(http.Handler) http.Handler) {
	registerExternalSends(mux, h, guard)
	mux.Handle("POST /recipients/imports/preview", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		in, ok := messaging.DecodeUpload(w, r, 2<<20)
		if !ok {
			return
		}
		out, e := h.store.Preview(r.Context(), uid, in)
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, out)
	})))
	mux.Handle("POST /recipients/imports/{id}/confirm", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		out, e := h.store.Confirm(r.Context(), uid, r.PathValue("id"))
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, out)
	})))
	mux.Handle("GET /recipients/{id}/history", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 100 {
				messaging.Fail(w, messaging.ErrInvalid)
				return
			}
			limit = n
		}
		items, e := h.store.History(r.Context(), uid, r.PathValue("id"))
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		c := r.URL.Query().Get("cursor")
		start := 0
		if c != "" {
			found := false
			for i, v := range items {
				if v.ID == c {
					start = i + 1
					found = true
					break
				}
			}
			if !found {
				messaging.Fail(w, messaging.ErrInvalid)
				return
			}
		}
		end := start + limit
		var next *string
		if end < len(items) {
			v := items[end-1].ID
			next = &v
		} else {
			end = len(items)
		}
		httpx.WriteJSON(w, 200, struct {
			Items      []History `json:"items"`
			NextCursor *string   `json:"nextCursor"`
		}{items[start:end], next})
	})))
}

// 기존 성공 시도도 집계하여 새 필드가 없던 수신자를 보강한다.
type sentStats struct {
	Latest time.Time
	Count  int
}

func (s *Store) legacyStats(ctx context.Context, uid string) (map[string]sentStats, error) {
	out := map[string]sentStats{}
	docs, e := messaging.Collection(s.FS, uid, "smsCampaigns").Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	for _, d := range docs {
		var v struct {
			Recipients []struct {
				RecipientID string     `firestore:"recipientId"`
				Status      string     `firestore:"status"`
				SentAt      *time.Time `firestore:"sentAt"`
				Attempts    []struct {
					ID         string     `firestore:"id"`
					Status     string     `firestore:"status"`
					FinishedAt *time.Time `firestore:"finishedAt"`
				} `firestore:"attempts"`
			} `firestore:"recipients"`
		}
		if e = d.DataTo(&v); e != nil {
			return nil, e
		}
		for _, r := range v.Recipients {
			st := out[r.RecipientID]
			count := 0
			seen := map[string]bool{}
			for _, a := range r.Attempts {
				if a.Status == "SENT" && !seen[a.ID] {
					seen[a.ID] = true
					count++
					if a.FinishedAt != nil && a.FinishedAt.After(st.Latest) {
						st.Latest = *a.FinishedAt
					}
				}
			}
			if r.Status == "SENT" && count == 0 {
				count = 1
			}
			st.Count += count
			if r.Status == "SENT" && r.SentAt != nil && r.SentAt.After(st.Latest) {
				st.Latest = *r.SentAt
			}
			out[r.RecipientID] = st
		}
	}
	external, e := ExternalHistory(ctx, s.FS, uid)
	if e != nil {
		return nil, e
	}
	for _, h := range external {
		st := out[h.RecipientID]
		st.Count++
		if h.SentAt != nil && h.SentAt.After(st.Latest) {
			st.Latest = *h.SentAt
		}
		out[h.RecipientID] = st
	}
	return out, nil
}

// SuccessfulCount는 이전 버전 문서의 성공 시도를 포함한 집계다.
func (s *Store) SuccessfulCount(ctx context.Context, uid, id string) (int, error) {
	v, e := s.legacyStats(ctx, uid)
	if e != nil {
		return 0, e
	}
	return v[id].Count, nil
}

func HistoryTime(h History) time.Time {
	if h.SentAt != nil {
		return *h.SentAt
	}
	if h.FailedAt != nil {
		return *h.FailedAt
	}
	return h.UpdatedAt
}
