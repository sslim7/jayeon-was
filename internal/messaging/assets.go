// Package messaging는 템플릿과 소형 이미지 첨부를 사용자별로 관리한다.
package messaging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// 🔴 첨부 한 건은 asset.Data 로 Firestore 문서 하나에 통째로 들어간다. 문서 한도가 1MiB 라
// 메타데이터와 인코딩 여유를 뺀 나머지가 여기 쓸 수 있는 전부다. 이 값을 1MiB 가까이 올리면
// 업로드 검증은 통과하고 Firestore 쓰기에서 실패하는데, 그 실패는 업로드 시점이 아니라
// ref.Create 에서 나므로 사용자에게는 원인이 드러나지 않는 500 으로만 보인다.
const MaxImageBytes = 700 << 10

// 캠페인/템플릿 하나에 붙는 첨부(최대 3개) 크기 합계. 첨부마다 문서가 따로라 문서 한도와는
// 무관하고, 한 번에 실어 보낼 양의 상한이다.
const MaxTotalBytes = 1400 << 10

// ⚠️ 위 두 값은 「저장 한도」지 「발송 가능 크기」가 아니다. 실제 발송 한도는 단말이 SIM 에서
// 읽는 MMS_CONFIG_MAX_MESSAGE_SIZE 이고, 그걸 알 수 없을 때 안드로이드 모듈이 쓰는 보수적
// 기본값이 300KiB 다. 즉 여기를 올려도 통신사가 더 작은 값으로 거절할 수 있어서, 앱이 첨부
// 시점에 이미지를 줄인다. 두 숫자는 서로 다른 것을 지키므로 한쪽만 보고 다른 쪽을 맞추면 안 된다.

// 첨부 파일 이름 길이 상한(글자 수). 안내 문장이 이 값을 그대로 읽어 쓴다.
const maxAttachmentNameRunes = 150

// limitLabel 은 한도를 사람이 읽는 단위로 적는다. MiB 로 딱 떨어질 때만 MB 로 쓴다.
// ⚠️ 안내 문장에 숫자를 손으로 적으면 상수만 바뀌고 문구는 옛 숫자로 남는다. 반드시 여기를 거쳐라.
func limitLabel(n int) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return strconv.Itoa(n>>20) + " MB"
	}
	return strconv.Itoa(n>>10) + " KB"
}

// sizeLabel 은 실제 크기를 KB 로 올림해 적는다. 내림하면 한도를 1바이트 넘긴 파일이
// 「700 KB까지인데 이 파일은 700 KB」라는 말이 안 되는 안내가 된다.
func sizeLabel(n int) string { return strconv.Itoa((n+(1<<10)-1)>>10) + " KB" }

// 해상도 상한. 큰 값 자체가 위험한 게 아니라, 디코딩이 잡는 메모리가 픽셀 수에 비례해서 걸어 둔다.
const maxImageSide = 10000
const maxImagePixels = 25000000

// pixelLabel 은 픽셀 수를 「만」 단위로 올려 적는다. 자릿수를 그대로 늘어놓으면 읽히지 않는다.
// ⚠️ 내림하면 한도를 갓 넘긴 이미지가 한도와 같은 숫자로 찍혀 말이 안 되는 안내가 된다.
func pixelLabel(n int64) string { return strconv.FormatInt((n+9999)/10000, 10) + "만" }

// 🔴 http.DetectContentType 은 HEIC/HEIF/AVIF 를 모른다. ftyp 박스 안에서 mp4 계열 브랜드만
// 보기 때문에 그 외에는 application/octet-stream 으로 뭉뚱그린다. 그대로 두면 아이폰 사진첩의
// 기본 형식인 HEIC 가 「알 수 없는 파일」이 되어, 실사용에서 제일 흔한 거절이 제일 불친절해진다.
// 그래서 브랜드를 직접 읽는다.
func heifBrand(data []byte) string {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return ""
	}
	switch string(data[8:12]) {
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs":
		return "HEIC"
	case "mif1", "msf1", "heif":
		return "HEIF"
	case "avif", "avis":
		return "AVIF"
	}
	return ""
}

// 사용자가 알아볼 만한 이름으로만 옮긴다. 여기 없는 형식은 굳이 MIME 문자열을 보여 주지 않는다 —
// 「application/octet-stream 입니다」는 아무것도 알려 주지 않으면서 겁만 준다.
var contentLabels = map[string]string{
	"image/gif":                 "GIF 이미지",
	"image/webp":                "WEBP 이미지",
	"image/bmp":                 "BMP 이미지",
	"image/vnd.microsoft.icon":  "아이콘(ICO) 파일",
	"application/pdf":           "PDF 문서",
	"application/zip":           "압축 파일",
	"video/mp4":                 "동영상",
	"text/plain; charset=utf-8": "글자만 들어 있는 파일",
	"text/html; charset=utf-8":  "웹 문서",
}

// 🔴 image.DecodeConfig 에는 jpeg/png 만 등록돼 있어서, 형식이 다른 파일과 깨진 파일이 똑같이
// 여기로 떨어진다. 사용자는 둘을 구분할 방법이 없으므로 내용을 직접 들여다보고 무엇인지 지목한다.
// 지목하지 못하면 사용자는 같은 사진을 계속 다시 고르게 된다.
func undecodableReason(data []byte) ValidationError {
	if brand := heifBrand(data); brand != "" {
		note := "아이폰과 최근 안드로이드가 사진에 쓰는 형식"
		if brand == "AVIF" {
			note = "최근 기기가 쓰는 사진 형식"
		}
		return ValidationError{Message: fmt.Sprintf("JPG 또는 PNG 이미지만 올릴 수 있어요. 이 파일은 %s(%s)입니다. 사진 앱에서 JPEG로 저장하거나 내보낸 뒤 올려 주세요.", brand, note)}
	}
	if label := contentLabels[http.DetectContentType(data)]; label != "" {
		return ValidationError{Message: fmt.Sprintf("JPG 또는 PNG 이미지만 올릴 수 있어요. 이 파일은 %s입니다.", label)}
	}
	return ValidationError{Message: "이미지로 읽을 수 없는 파일이에요. 파일이 깨지지 않았는지 확인하고, JPG 또는 PNG 이미지를 선택해 주세요."}
}

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
	// 🔴 아래 거절들은 원인이 서로 다르다. 한 덩어리 조건으로 묶어 ErrInvalid 를 던지면
	// Fail() 이 「파일 또는 입력값을 확인해 주세요.」 하나로 덮어 버려서, 사용자는 파일이 큰 건지
	// 이름이 긴 건지 알 길이 없다. ValidationError 는 이 문장이 그대로 화면까지 간다.
	data, err := base64.StdEncoding.DecodeString(in.DataBase64)
	if err != nil {
		return Attachment{}, ValidationError{Message: "파일을 읽을 수 없어요. 다시 선택해 주세요."}
	}
	if len(data) == 0 {
		return Attachment{}, ValidationError{Message: "빈 파일이에요. 다른 이미지를 선택해 주세요."}
	}
	if len(data) > MaxImageBytes {
		return Attachment{}, ValidationError{Message: fmt.Sprintf("이미지 한 장은 %s까지예요. 이 파일은 %s입니다. 사진을 줄여서 올려 주세요.", limitLabel(MaxImageBytes), sizeLabel(len(data)))}
	}
	if strings.TrimSpace(in.Name) == "" {
		return Attachment{}, ValidationError{Message: "파일 이름이 없어요."}
	}
	if utf8.RuneCountInString(in.Name) > maxAttachmentNameRunes {
		return Attachment{}, ValidationError{Message: fmt.Sprintf("파일 이름은 %d자까지예요.", maxAttachmentNameRunes)}
	}
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Attachment{}, undecodableReason(data)
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return Attachment{}, ValidationError{Message: "이미지 크기를 읽을 수 없어요. 다른 이미지를 선택해 주세요."}
	}
	if cfg.Width > maxImageSide || cfg.Height > maxImageSide {
		return Attachment{}, ValidationError{Message: fmt.Sprintf("이미지 한 변은 %d픽셀까지예요. 이 이미지는 %d×%d입니다. 크기를 줄여서 올려 주세요.", maxImageSide, cfg.Width, cfg.Height)}
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px > maxImagePixels {
		return Attachment{}, ValidationError{Message: fmt.Sprintf("이미지는 %s 픽셀까지예요. 이 이미지는 %d×%d(%s 픽셀)입니다. 크기를 줄여서 올려 주세요.", pixelLabel(maxImagePixels), cfg.Width, cfg.Height, pixelLabel(px))}
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		// 머리말은 읽혔는데 본문에서 실패했다 — 형식 문제가 아니라 전송 중 잘렸거나 내용이 상한 파일이다.
		return Attachment{}, ValidationError{Message: "이미지가 중간에 깨졌어요. 파일을 다시 받아서 올리거나 다른 이미지를 선택해 주세요."}
	}
	mime := "image/" + kind
	if kind == "jpeg" {
		mime = "image/jpeg"
	}
	// ⚠️ DecodeConfig 에 jpeg/png 만 등록돼 있어 지금은 이 줄에 닿지 않는다. 포맷을 하나라도 더
	// import 하면 닿게 되므로, 그때 조용히 「입력값 확인」으로 새지 않도록 사유를 남겨 둔다.
	if mime != "image/jpeg" && mime != "image/png" {
		return Attachment{}, ValidationError{Message: "JPG 또는 PNG 이미지만 올릴 수 있어요."}
	}
	if in.MimeType != mime {
		return Attachment{}, ValidationError{Message: "파일 내용과 형식이 달라요. 파일을 다시 선택해 주세요."}
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
		// 얼마까지인지와 지금 얼마인지를 함께 말한다. 한쪽만 말하면 몇 장을 빼야 하는지 알 수 없다.
		return nil, ValidationError{Message: fmt.Sprintf("첨부 합계는 %s까지예요. 지금 %s입니다. 사진을 빼거나 줄여서 올려 주세요.", limitLabel(MaxTotalBytes), sizeLabel(total))}
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
	// base64 는 원본보다 4/3 크고 JSON 필드도 함께 오므로 여유를 둔다.
	// ⚠️ 여기서 잘린 요청은 아래 Upload 의 크기 안내까지 가지 못하므로, 크기 초과라는 사실을
	// 이 자리에서 직접 말해야 한다. 안 그러면 「너무 큰 파일」이 「형식 오류」로 둔갑한다.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(max*4/3+4096)))
	if e := dec.Decode(&in); e != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(e, &tooLarge) {
			Fail(w, ValidationError{Message: fmt.Sprintf("올린 파일이 너무 커요. %s까지 올릴 수 있어요.", limitLabel(max))})
			return in, false
		}
		Fail(w, ValidationError{Message: "요청을 읽을 수 없어요. 파일을 다시 선택해 주세요."})
		return in, false
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(e, &tooLarge) {
			Fail(w, ValidationError{Message: fmt.Sprintf("올린 파일이 너무 커요. %s까지 올릴 수 있어요.", limitLabel(max))})
			return in, false
		}
		Fail(w, ValidationError{Message: "요청 형식이 올바르지 않아요. 파일을 다시 선택해 주세요."})
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
