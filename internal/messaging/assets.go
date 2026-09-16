// Package messaging는 템플릿과 소형 이미지 첨부를 사용자별로 관리한다.
package messaging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
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

type ValidationError struct{ Message string }

func (e ValidationError) Error() string { return e.Message }

var ErrInvalid = errors.New("invalid messaging input")
var ErrMissing = errors.New("messaging item not found")

const MaxImageBytes = 300 << 10
const MaxTotalBytes = 600 << 10

type Attachment struct {
	ID        string    `json:"id" firestore:"id"`
	Name      string    `json:"name" firestore:"name"`
	MimeType  string    `json:"mimeType" firestore:"mimeType"`
	Size      int       `json:"size" firestore:"size"`
	CreatedAt time.Time `json:"createdAt" firestore:"createdAt"`
}
type asset struct {
	Attachment Attachment `firestore:"attachment"`
	Data       []byte     `firestore:"data"`
	Deleted    bool       `firestore:"deleted"`
}
type Upload struct {
	Name       string `json:"name"`
	MimeType   string `json:"mimeType"`
	DataBase64 string `json:"dataBase64"`
}
type Template struct {
	ID          string       `json:"id" firestore:"id"`
	Name        string       `json:"name" firestore:"name"`
	Message     string       `json:"message" firestore:"message"`
	Attachments []Attachment `json:"attachments" firestore:"attachments"`
	CreatedAt   time.Time    `json:"createdAt" firestore:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt" firestore:"updatedAt"`
}

// 응답 직렬화 경계에서 레거시 nil 첨부도 빈 배열로 보장한다.
func (t Template) MarshalJSON() ([]byte, error) {
	type plain Template
	v := plain(t)
	if v.Attachments == nil {
		v.Attachments = []Attachment{}
	}
	return json.Marshal(v)
}

type TemplateInput struct {
	Name          string   `json:"name"`
	Message       string   `json:"message"`
	AttachmentIDs []string `json:"attachmentIds"`
}
type Store struct{ FS *firestore.Client }

func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func Collection(fs *firestore.Client, uid, name string) *firestore.CollectionRef {
	return fs.Collection("users").Doc(uid).Collection(name)
}
func (s *Store) Upload(ctx context.Context, uid string, in Upload) (Attachment, error) {
	data, err := base64.StdEncoding.DecodeString(in.DataBase64)
	if err != nil || len(data) == 0 || len(data) > MaxImageBytes || strings.TrimSpace(in.Name) == "" || utf8.RuneCountInString(in.Name) > 150 {
		return Attachment{}, ErrInvalid
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 10000 || cfg.Height > 10000 || int64(cfg.Width)*int64(cfg.Height) > 25000000 {
		return Attachment{}, ErrInvalid
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return Attachment{}, ErrInvalid
	}
	mime := "image/" + kind
	if kind == "jpeg" {
		mime = "image/jpeg"
	}
	if (mime != "image/jpeg" && mime != "image/png") || in.MimeType != mime {
		return Attachment{}, ErrInvalid
	}
	ref := Collection(s.FS, uid, "smsAttachments").NewDoc()
	out := Attachment{ref.ID, strings.TrimSpace(in.Name), mime, len(data), time.Now().UTC()}
	_, err = ref.Create(ctx, asset{Attachment: out, Data: data})
	return out, err
}

// Resolve는 트랜잭션 안에서 소유권/삭제/합계 크기를 검증한다. 이미지 바이트는 snapshot에 복제하지 않는다.
func Resolve(tx *firestore.Transaction, fs *firestore.Client, uid string, ids []string) ([]Attachment, error) {
	out := make([]Attachment, 0, len(ids))
	seen := map[string]bool{}
	total := 0
	if len(ids) > 3 {
		return nil, ErrInvalid
	}
	for _, id := range ids {
		if !ValidID(id) || seen[id] {
			return nil, ErrInvalid
		}
		seen[id] = true
		doc, e := tx.Get(Collection(fs, uid, "smsAttachments").Doc(id))
		if status.Code(e) == codes.NotFound {
			return nil, ErrMissing
		}
		if e != nil {
			return nil, e
		}
		var a asset
		if e = doc.DataTo(&a); e != nil {
			return nil, e
		}
		if a.Deleted {
			return nil, ErrMissing
		}
		total += a.Attachment.Size
		out = append(out, a.Attachment)
	}
	if total > MaxTotalBytes {
		return nil, ErrInvalid
	}
	return out, nil
}
func (s *Store) SaveTemplate(ctx context.Context, uid, id string, in TemplateInput) (Template, error) {
	if strings.TrimSpace(in.Name) == "" || utf8.RuneCountInString(in.Name) > 100 || utf8.RuneCountInString(in.Message) > 2000 || (strings.TrimSpace(in.Message) == "" && len(in.AttachmentIDs) == 0) {
		return Template{}, ErrInvalid
	}
	create := id == ""
	ref := Collection(s.FS, uid, "smsTemplates").NewDoc()
	if !create {
		if !ValidID(id) {
			return Template{}, ErrInvalid
		}
		ref = Collection(s.FS, uid, "smsTemplates").Doc(id)
	}
	var out Template
	err := s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		now := time.Now().UTC()
		out = Template{ID: ref.ID, Name: strings.TrimSpace(in.Name), Message: in.Message, CreatedAt: now, UpdatedAt: now}
		if !create {
			doc, e := tx.Get(ref)
			if status.Code(e) == codes.NotFound {
				return ErrMissing
			}
			if e != nil {
				return e
			}
			var old Template
			if e = doc.DataTo(&old); e != nil {
				return e
			}
			out.CreatedAt = old.CreatedAt
		}
		a, e := Resolve(tx, s.FS, uid, in.AttachmentIDs)
		if e != nil {
			return e
		}
		out.Attachments = a
		return tx.Set(ref, out)
	})
	return out, err
}
func DecodeUpload(w http.ResponseWriter, r *http.Request, max int) (Upload, bool) {
	var in Upload
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(max*4/3+4096)))
	if e := dec.Decode(&in); e != nil {
		Fail(w, ErrInvalid)
		return in, false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		Fail(w, ErrInvalid)
		return in, false
	}
	return in, true
}
func Fail(w http.ResponseWriter, e error) {
	var validation ValidationError
	if errors.As(e, &validation) {
		httpx.WriteError(w, 400, "VALIDATION_FAILED", validation.Message)
		return
	}
	switch {
	case errors.Is(e, ErrInvalid):
		httpx.WriteError(w, 400, "VALIDATION_FAILED", "파일 또는 입력값을 확인해 주세요.")
	case errors.Is(e, ErrMissing) || status.Code(e) == codes.NotFound:
		httpx.WriteError(w, 404, "NOT_FOUND", "자료를 찾을 수 없습니다.")
	default:
		httpx.WriteError(w, 500, "INTERNAL_ERROR", "요청 처리에 실패했습니다.")
	}
}
func Register(mux *http.ServeMux, fs *firestore.Client, guard func(http.Handler) http.Handler) {
	s := &Store{fs}
	bind := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if auth.UserID(r.Context()) == "" {
				httpx.WriteError(w, 401, "UNAUTHORIZED", "로그인이 필요합니다.")
				return
			}
			fn(w, r)
		})))
	}
	bind("POST /sms/attachments", func(w http.ResponseWriter, r *http.Request) {
		in, ok := DecodeUpload(w, r, MaxImageBytes)
		if !ok {
			return
		}
		a, e := s.Upload(r.Context(), auth.UserID(r.Context()), in)
		if e != nil {
			Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 201, a)
	})
	getAsset := func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ValidID(id) {
			Fail(w, ErrInvalid)
			return
		}
		doc, e := Collection(fs, auth.UserID(r.Context()), "smsAttachments").Doc(id).Get(r.Context())
		if e != nil {
			Fail(w, e)
			return
		}
		var a asset
		if e = doc.DataTo(&a); e != nil {
			Fail(w, e)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			httpx.WriteJSON(w, 200, struct {
				Attachment
				DataBase64 string `json:"dataBase64"`
			}{a.Attachment, base64.StdEncoding.EncodeToString(a.Data)})
		} else {
			httpx.WriteJSON(w, 200, a.Attachment)
		}
	}
	bind("GET /sms/attachments/{id}", getAsset)
	bind("GET /sms/attachments/{id}/content", getAsset)
	bind("DELETE /sms/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ValidID(id) {
			Fail(w, ErrInvalid)
			return
		}
		_, e := Collection(fs, auth.UserID(r.Context()), "smsAttachments").Doc(id).Update(r.Context(), []firestore.Update{{Path: "deleted", Value: true}})
		if e != nil {
			Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 204, nil)
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var in TemplateInput
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		out, e := s.SaveTemplate(r.Context(), auth.UserID(r.Context()), r.PathValue("id"), in)
		if e != nil {
			Fail(w, e)
			return
		}
		code := 200
		if r.Method == "POST" {
			code = 201
		}
		httpx.WriteJSON(w, code, out)
	}
	bind("POST /sms/templates", save)
	bind("PUT /sms/templates/{id}", save)
	bind("GET /sms/templates", func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 100 {
				Fail(w, ErrInvalid)
				return
			}
			limit = n
		}
		q := Collection(fs, auth.UserID(r.Context()), "smsTemplates").OrderBy(firestore.DocumentID, firestore.Asc)
		c := r.URL.Query().Get("cursor")
		if c != "" {
			if !ValidID(c) {
				Fail(w, ErrInvalid)
				return
			}
			q = q.StartAfter(c)
		}
		docs, e := q.Limit(limit + 1).Documents(r.Context()).GetAll()
		if e != nil {
			Fail(w, e)
			return
		}
		items := []Template{}
		var next *string
		for i, d := range docs {
			if i == limit {
				v := items[limit-1].ID
				next = &v
				break
			}
			var t Template
			if e = d.DataTo(&t); e != nil {
				Fail(w, e)
				return
			}
			items = append(items, t)
		}
		httpx.WriteJSON(w, 200, struct {
			Items      []Template `json:"items"`
			NextCursor *string    `json:"nextCursor"`
		}{items, next})
	})
	bind("GET /sms/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ValidID(id) {
			Fail(w, ErrInvalid)
			return
		}
		doc, e := Collection(fs, auth.UserID(r.Context()), "smsTemplates").Doc(id).Get(r.Context())
		if e != nil {
			Fail(w, e)
			return
		}
		var t Template
		if e = doc.DataTo(&t); e != nil {
			Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, t)
	})
	bind("DELETE /sms/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !ValidID(id) {
			Fail(w, ErrInvalid)
			return
		}
		_, e := Collection(fs, auth.UserID(r.Context()), "smsTemplates").Doc(id).Delete(r.Context())
		if e != nil {
			Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 204, nil)
	})
}
