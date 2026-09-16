// Package recipients는 사용자별 수신자를 관리한다. 캠페인은 수신자 정보를 복사하므로
// 원본을 수정하거나 삭제해도 기존 캠페인의 스냅샷은 변경되지 않는다.
package recipients

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CustomField struct {
	Name  string `json:"name" firestore:"name"`
	Value string `json:"value" firestore:"value"`
}
type Recipient struct {
	CustomFields []CustomField `json:"customFields" firestore:"customFields"`
	SentCount    int           `json:"sentCount" firestore:"sentCount"`
	LatestSentAt *time.Time    `json:"latestSentAt" firestore:"latestSentAt"`
	ID           string        `json:"id" firestore:"id"`
	Name         string        `json:"name" firestore:"name"`
	Phone        string        `json:"phone" firestore:"phone"`
	GroupID      string        `json:"groupId" firestore:"groupId"`
	CreatedAt    time.Time     `json:"createdAt" firestore:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt" firestore:"updatedAt"`
}

type Input struct {
	CustomFields []CustomField `json:"customFields"`
	Name         string        `json:"name"`
	Phone        string        `json:"phone"`
	GroupID      string        `json:"groupId"`
}

var ErrDuplicate = errors.New("phone already exists")
var ErrNotFound = errors.New("recipient not found")

// ValidateID는 사용자 하위 컬렉션 경로에 임의의 경로가 삽입되는 것을 막는다.
func ValidateID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func DocumentRef(fs *firestore.Client, userID, id string) *firestore.DocumentRef {
	return fs.Collection("users").Doc(userID).Collection("recipients").Doc(id)
}
func collection(fs *firestore.Client, userID string) *firestore.CollectionRef {
	return fs.Collection("users").Doc(userID).Collection("recipients")
}
func phoneRef(fs *firestore.Client, userID, phone string) *firestore.DocumentRef {
	hash := sha256.Sum256([]byte(phone))
	return fs.Collection("users").Doc(userID).Collection("recipientPhones").Doc(hex.EncodeToString(hash[:]))
}

// NormalizePhone은 입력 번호를 010으로 시작하는 11자리 숫자로 제한한다.
func NormalizePhone(value string) (string, error) {
	p := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	if len(p) != 11 || !strings.HasPrefix(p, "010") {
		return "", errors.New("전화번호는 010으로 시작하는 11자리 숫자여야 합니다.")
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return "", errors.New("전화번호에는 숫자와 하이픈만 입력해 주세요.")
		}
	}
	return p, nil
}

// StoredPhone은 이전 버전 E.164 저장값만 국내형으로 변환한다. 신규 입력에는 사용하지 않는다.
func StoredPhone(p string) string {
	if strings.HasPrefix(p, "+8210") && len(p) == 13 {
		return "0" + p[3:]
	}
	return p
}
func phoneKeys(phone string) []string {
	canonical := StoredPhone(phone)
	keys := []string{canonical}
	if len(canonical) == 11 && strings.HasPrefix(canonical, "010") {
		keys = append(keys, "+82"+canonical[1:])
	}
	return keys
}

func validate(in Input) (Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.GroupID = strings.TrimSpace(in.GroupID)
	if !utf8.ValidString(in.Name) || utf8.RuneCountInString(in.Name) < 1 || utf8.RuneCountInString(in.Name) > 100 || !utf8.ValidString(in.GroupID) || utf8.RuneCountInString(in.GroupID) > 100 {
		return in, errors.New("이름은 1~100자, 그룹은 100자 이하여야 합니다.")
	}
	if in.CustomFields != nil {
		if len(in.CustomFields) > 20 {
			return in, errors.New("추가 항목은 최대 20개입니다.")
		}
		seen := map[string]bool{}
		for i := range in.CustomFields {
			f := &in.CustomFields[i]
			f.Name = strings.TrimSpace(f.Name)
			if f.Name == "" || utf8.RuneCountInString(f.Name) > 100 || utf8.RuneCountInString(f.Value) > 1000 || seen[f.Name] || !utf8.ValidString(f.Name) || !utf8.ValidString(f.Value) {
				return in, errors.New("추가 항목 이름/값 또는 중복 이름을 확인해 주세요.")
			}
			seen[f.Name] = true
		}
		b, _ := json.Marshal(in.CustomFields)
		if len(b) > 2000 {
			return in, errors.New("추가 항목 합계는 UTF-8 JSON 2000bytes 이하여야 합니다.")
		}
	}
	var err error
	in.Phone, err = NormalizePhone(in.Phone)
	return in, err
}

type Store struct{ FS *firestore.Client }

func (s *Store) Save(ctx context.Context, uid, id string, in Input) (Recipient, error) {
	var err error
	in, err = validate(in)
	if err != nil {
		return Recipient{}, err
	}
	create := id == ""
	var ref *firestore.DocumentRef
	if create {
		ref = collection(s.FS, uid).NewDoc()
	} else {
		ref = DocumentRef(s.FS, uid, id)
	}
	baseline := sentStats{}
	if !create {
		stats, e := s.legacyStats(ctx, uid)
		if e != nil {
			return Recipient{}, e
		}
		baseline = stats[id]
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	out := Recipient{CustomFields: in.CustomFields, ID: ref.ID, Name: in.Name, Phone: in.Phone, GroupID: in.GroupID, CreatedAt: now, UpdatedAt: now}
	err = s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		var old Recipient
		if !create {
			doc, e := tx.Get(ref)
			if status.Code(e) == codes.NotFound {
				return ErrNotFound
			}
			if e != nil {
				return e
			}
			if e = doc.DataTo(&old); e != nil {
				return e
			}
			out.CreatedAt = old.CreatedAt
			out.LatestSentAt = old.LatestSentAt
			out.SentCount = old.SentCount
			if baseline.Count > out.SentCount {
				out.SentCount = baseline.Count
			}
			if !baseline.Latest.IsZero() && (out.LatestSentAt == nil || baseline.Latest.After(*out.LatestSentAt)) {
				out.LatestSentAt = &baseline.Latest
			}
			if in.CustomFields == nil {
				out.CustomFields = old.CustomFields
			}
		}
		lock := phoneRef(s.FS, uid, in.Phone)
		for _, key := range phoneKeys(in.Phone) {
			doc, e := tx.Get(phoneRef(s.FS, uid, key))
			if e == nil {
				owner, _ := doc.Data()["recipientId"].(string)
				if owner != ref.ID {
					return ErrDuplicate
				}
			} else if status.Code(e) != codes.NotFound {
				return e
			}
		}

		if !create && old.Phone != in.Phone {
			for _, key := range phoneKeys(old.Phone) {
				if key == in.Phone {
					continue
				}
				if e := tx.Delete(phoneRef(s.FS, uid, key)); e != nil {
					return e
				}
			}
		}
		if e := tx.Set(lock, map[string]any{"recipientId": ref.ID}); e != nil {
			return e
		}
		if out.CustomFields == nil {
			out.CustomFields = []CustomField{}
		}
		return tx.Set(ref, out)
	})

	return out, err
}
func (s *Store) Delete(ctx context.Context, uid, id string) error {
	return s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		ref := DocumentRef(s.FS, uid, id)
		doc, e := tx.Get(ref)
		if status.Code(e) == codes.NotFound {
			return ErrNotFound
		}
		if e != nil {
			return e
		}
		var item Recipient
		if e = doc.DataTo(&item); e != nil {
			return e
		}
		for _, key := range phoneKeys(item.Phone) {
			if e = tx.Delete(phoneRef(s.FS, uid, key)); e != nil {
				return e
			}
		}
		return tx.Delete(ref)
	})
}

type cursor struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Filter string `json:"filter"`
}
type Page struct {
	Total      int         `json:"total"`
	Items      []Recipient `json:"items"`
	NextCursor *string     `json:"nextCursor"`
}

func filterHash(q, group string) string {
	v, _ := json.Marshal([]string{q, group})
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:])
}
func encodeCursor(id, filter string) string {
	b, _ := json.Marshal(cursor{ID: id, Filter: filter})
	return base64.RawURLEncoding.EncodeToString(b)
}
func searchPhone(q string) string { return strings.ReplaceAll(strings.TrimSpace(q), "-", "") }

// List는 이름/ID 순서로 정렬하고 필터 적용 후 전체 개수와 다음 커서를 반환한다.
func (s *Store) List(ctx context.Context, uid, q, group, rawCursor string, limit int, includeSentValues ...bool) (Page, error) {
	out := Page{Items: []Recipient{}}
	if limit < 1 || limit > 100 {
		return out, errors.New("잘못된 limit입니다.")
	}
	q = strings.ToLower(strings.TrimSpace(q))
	includeSent := true
	if len(includeSentValues) > 0 {
		includeSent = includeSentValues[0]
	}
	filterGroup := group
	if !includeSent {
		filterGroup += "\x00unsent"
	}
	filter := filterHash(q, filterGroup)
	var c cursor
	if rawCursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(rawCursor)
		if e != nil || json.Unmarshal(b, &c) != nil || !ValidateID(c.ID) || c.Filter != filter {
			return out, errors.New("잘못된 cursor입니다.")
		}
	}
	docs, e := collection(s.FS, uid).Documents(ctx).GetAll()
	if e != nil {
		return out, e
	}
	stats, e := s.legacyStats(ctx, uid)
	if e != nil {
		return out, e
	}
	phoneQuery := searchPhone(q)
	all := []Recipient{}
	for _, d := range docs {
		var item Recipient
		if e = d.DataTo(&item); e != nil {
			return out, e
		}
		item.ID = d.Ref.ID
		item.Phone = StoredPhone(item.Phone)
		if item.CustomFields == nil {
			item.CustomFields = []CustomField{}
		}
		v := stats[item.ID]
		if !v.Latest.IsZero() && (item.LatestSentAt == nil || v.Latest.After(*item.LatestSentAt)) {
			item.LatestSentAt = &v.Latest
		}
		if v.Count > item.SentCount {
			item.SentCount = v.Count
		}
		if (includeSent || item.LatestSentAt == nil) && (group == "" || item.GroupID == group) && (q == "" || strings.Contains(strings.ToLower(item.Name), q) || (phoneQuery != "" && strings.Contains(item.Phone, phoneQuery))) {
			all = append(all, item)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name == all[j].Name {
			return all[i].ID < all[j].ID
		}
		return all[i].Name < all[j].Name
	})
	out.Total = len(all)
	for _, item := range all {
		if rawCursor != "" && (item.Name < c.Name || item.Name == c.Name && item.ID <= c.ID) {
			continue
		}
		if len(out.Items) == limit {
			last := out.Items[len(out.Items)-1]
			b, _ := json.Marshal(cursor{Name: last.Name, ID: last.ID, Filter: filter})
			v := base64.RawURLEncoding.EncodeToString(b)
			out.NextCursor = &v
			break
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func Register(mux *http.ServeMux, fs *firestore.Client, guard func(http.Handler) http.Handler) {
	h := &handler{&Store{fs}}
	registerExtensions(mux, h, guard)
	mux.Handle("GET /recipients", guard(http.HandlerFunc(h.list)))
	mux.Handle("POST /recipients", guard(http.HandlerFunc(h.save)))
	mux.Handle("PUT /recipients/{id}", guard(http.HandlerFunc(h.save)))
	mux.Handle("DELETE /recipients/{id}", guard(http.HandlerFunc(h.delete)))
}

type handler struct{ store *Store }

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, 404, "NOT_FOUND", "수신자를 찾을 수 없습니다.")
	case errors.Is(err, ErrDuplicate):
		httpx.WriteError(w, 409, "PHONE_ALREADY_EXISTS", "이미 등록된 전화번호입니다.")
	default:
		httpx.WriteError(w, 500, "INTERNAL_ERROR", "요청 처리에 실패했습니다.")
	}
}
func prepare(w http.ResponseWriter, r *http.Request) string {
	w.Header().Set("Cache-Control", "no-store")
	uid := auth.UserID(r.Context())
	if uid == "" {
		httpx.WriteError(w, 401, "UNAUTHORIZED", "인증이 필요합니다.")
	}
	return uid
}
func (h *handler) save(w http.ResponseWriter, r *http.Request) {
	uid := prepare(w, r)
	if uid == "" {
		return
	}
	id := r.PathValue("id")
	if r.Method == http.MethodPut && !ValidateID(id) {
		httpx.WriteError(w, 400, "VALIDATION_FAILED", "잘못된 수신자 ID입니다.")
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	in, err := validate(in)
	if err != nil {
		httpx.WriteError(w, 400, "VALIDATION_FAILED", err.Error())
		return
	}
	item, err := h.store.Save(r.Context(), uid, id, in)
	if err != nil {
		fail(w, err)
		return
	}
	code := 200
	if r.Method == http.MethodPost {
		code = 201
	}
	httpx.WriteJSON(w, code, item)
}
func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	uid := prepare(w, r)
	if uid == "" {
		return
	}
	id := r.PathValue("id")
	if !ValidateID(id) {
		httpx.WriteError(w, 400, "VALIDATION_FAILED", "잘못된 수신자 ID입니다.")
		return
	}
	if err := h.store.Delete(r.Context(), uid, id); err != nil {
		fail(w, err)
		return
	}
	httpx.WriteJSON(w, 204, nil)
}
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	uid := prepare(w, r)
	if uid == "" {
		return
	}
	v := r.URL.Query()
	limit := 50
	if v.Has("limit") {
		n, e := strconv.Atoi(v.Get("limit"))
		if e != nil || n < 1 || n > 100 {
			httpx.WriteError(w, 400, "VALIDATION_FAILED", "limit은 1~100이어야 합니다.")
			return
		}
		limit = n
	}
	includeSent := true
	if v.Has("includeSent") {
		if v.Get("includeSent") != "true" && v.Get("includeSent") != "false" {
			httpx.WriteError(w, 400, "VALIDATION_FAILED", "includeSent는 true 또는 false입니다.")
			return
		}
		includeSent = v.Get("includeSent") == "true"
	}
	q, group, c := v.Get("q"), v.Get("groupId"), v.Get("cursor")
	if utf8.RuneCountInString(q) > 100 || utf8.RuneCountInString(group) > 100 || len(c) > 1024 {
		httpx.WriteError(w, 400, "VALIDATION_FAILED", "검색 조건이 너무 깁니다.")
		return
	}
	// 저장소 오류와 구분하여 잘못된 커서에는 입력 검증 오류를 반환한다.
	if c != "" {
		var decoded cursor
		b, e := base64.RawURLEncoding.DecodeString(c)
		if e != nil || json.Unmarshal(b, &decoded) != nil || !ValidateID(decoded.ID) || decoded.Filter != filterHash(strings.ToLower(strings.TrimSpace(q)), func() string {
			if !includeSent {
				return group + "\x00unsent"
			}
			return group
		}()) {
			httpx.WriteError(w, 400, "VALIDATION_FAILED", "잘못된 cursor입니다.")
			return
		}
	}
	page, err := h.store.List(r.Context(), uid, q, group, c, limit, includeSent)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.WriteJSON(w, 200, page)
}
