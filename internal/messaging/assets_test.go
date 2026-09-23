package messaging

import (
	"bytes"
	"cloud.google.com/go/firestore"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sslim7/nature-was/internal/auth"
	"hash/crc32"
	"image"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAssetsAndTemplates(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	s := &Store{fs}
	uid := fmt.Sprintf("asset-%d", time.Now().UnixNano())
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	a, e := s.Upload(ctx, uid, Upload{Name: "명함.png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(b.Bytes())})
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.SaveTemplate(ctx, uid, "", TemplateInput{Name: "명함", AttachmentIDs: []string{a.ID}})
	if e != nil || len(v.Attachments) != 1 {
		t.Fatal(v, e)
	}
	if _, e = s.SaveTemplate(ctx, uid+"x", "", TemplateInput{Name: "도용", AttachmentIDs: []string{a.ID}}); e != ErrMissing {
		t.Fatal(e)
	}
	// 위조 이미지는 여전히 거절된다. 다만 이제 ErrInvalid 가 아니라 사유가 담긴 ValidationError 다.
	var invalid ValidationError
	if _, e = s.Upload(ctx, uid, Upload{Name: "위조", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString([]byte("fake"))}); !errors.As(e, &invalid) {
		t.Fatal(e)
	}
	if _, e = s.SaveTemplate(ctx, uid, "", TemplateInput{Name: "중복", AttachmentIDs: []string{a.ID, a.ID}}); e != ErrInvalid {
		t.Fatal(e)
	}
}

func TestTemplateHTTPAlwaysArray(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	uid := fmt.Sprintf("template-http-%d", time.Now().UnixNano())
	mux := http.NewServeMux()
	Register(mux, fs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), uid)))
		})
	})
	Collection(fs, uid, "smsTemplates").Doc("legacy").Set(ctx, map[string]any{"id": "legacy", "name": "기존", "message": "본문"})
	Collection(fs, uid, "smsTemplates").Doc("legacy-null").Set(ctx, map[string]any{"id": "legacy-null", "name": "기존", "message": "본문", "attachments": nil})
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{{"GET", "/sms/templates/legacy", "", 200}, {"GET", "/sms/templates/legacy-null", "", 200}, {"GET", "/sms/templates", "", 200}, {"POST", "/sms/templates", `{"name":"신규","message":"본문"}`, 201}, {"PUT", "/sms/templates/legacy", `{"name":"수정","message":"본문"}`, 200}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
		var v map[string]any
		if e = json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
		assert := func(m map[string]any) {
			if _, ok := m["attachments"].([]any); !ok {
				t.Fatalf("배열 아님: %s", w.Body.String())
			}
		}
		if items, ok := v["items"].([]any); ok {
			for _, item := range items {
				assert(item.(map[string]any))
			}
		} else {
			assert(v)
		}
	}
}

// noisePNG는 압축되지 않는 잡음으로 한도 바로 아래 크기의 진짜 PNG를 만든다.
// 단색 이미지는 아무리 커도 PNG로 몇 KB라 크기 한도를 시험할 수 없다.
func noisePNG(t *testing.T, max int) []byte {
	t.Helper()
	for d := 560; d >= 8; d -= 8 {
		img := image.NewRGBA(image.Rect(0, 0, d, d))
		rand.New(rand.NewSource(int64(d))).Read(img.Pix)
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255 // 불투명하게 만들어야 png가 3바이트/픽셀로 저장한다
		}
		var b bytes.Buffer
		if e := png.Encode(&b, img); e != nil {
			t.Fatal(e)
		}
		if b.Len() <= max {
			return b.Bytes()
		}
	}
	t.Fatal("한도 이하 PNG를 만들지 못했다")
	return nil
}

// pngHeader 는 IHDR 만 있는 PNG 를 만든다. DecodeConfig 는 IHDR 까지만 읽으므로 해상도 검증을
// 몇 십 바이트로 시험할 수 있다. 실제로 12000×8000 이미지를 만들면 테스트가 기가바이트를 먹는다.
func pngHeader(t *testing.T, w, h int) []byte {
	t.Helper()
	ihdr := []byte("IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 2, 0, 0, 0) // 8비트 트루컬러, 인터레이스 없음
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)-4))
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}

// heicBytes 는 아이폰 사진의 앞머리(ftyp 박스)만 흉내 낸다. 형식 판별은 여기까지만 본다.
func heicBytes() []byte {
	return append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), []byte("heicmif1miaf")...)
}

// 🔴 아이폰 사진첩 기본 형식이 HEIC 라 실사용에서 제일 자주 걸리는 거절이다. 여기가 뭉뚱그려지면
// 사용자는 파일이 잘못된 줄 모르고 같은 사진을 계속 다시 고른다.
func TestImageFormatRejectionsSayWhat(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	s := &Store{fs}
	uid := fmt.Sprintf("asset-format-%d", time.Now().UnixNano())

	// http.DetectContentType 은 HEIC 를 모른다(application/octet-stream). 그래서 ftyp 브랜드를
	// 직접 읽는다. 이 전제가 깨지면 아래 HEIC 문구가 조용히 「알 수 없는 파일」로 바뀐다.
	if got := http.DetectContentType(heicBytes()); got != "application/octet-stream" {
		t.Fatal("DetectContentType 이 HEIC 를 알아보게 바뀌었다 — undecodableReason 을 다시 보라:", got)
	}
	if heifBrand(heicBytes()) != "HEIC" {
		t.Fatal("HEIC 브랜드를 읽지 못한다")
	}

	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	good := base64.StdEncoding.EncodeToString(b.Bytes())

	seen := map[string]string{}
	for _, tc := range []struct {
		name, want string
		in         Upload
	}{
		{"HEIC", "HEIC", Upload{Name: "아이폰사진.heic", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(heicBytes())}},
		{"GIF", "GIF 이미지", Upload{Name: "움짤.gif", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00,"))}},
		{"깨진 이미지", "깨졌어요", Upload{Name: "잘린.png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(pngHeader(t, 2, 2))}},
		{"형식 불일치", "파일 내용과 형식이 달라요", Upload{Name: "이름만jpg.jpg", MimeType: "image/jpeg", DataBase64: good}},
		{"한 변 초과", "10000픽셀까지", Upload{Name: "대형.png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(pngHeader(t, 12000, 8000))}},
		{"픽셀 수 초과", "2500만 픽셀까지", Upload{Name: "고해상도.png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(pngHeader(t, 9000, 9000))}},
	} {
		_, err := s.Upload(ctx, uid, tc.in)
		var v ValidationError
		if !errors.As(err, &v) {
			t.Fatalf("%s: ValidationError 가 아니다: %v", tc.name, err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: ErrInvalid 로도 잡힌다 — Fail() 이 고정 문구로 덮을 수 있다", tc.name)
		}
		if !strings.Contains(v.Message, tc.want) {
			t.Fatalf("%s: 「%s」가 없다: %s", tc.name, tc.want, v.Message)
		}
		if prev, dup := seen[v.Message]; dup {
			t.Fatalf("%s 와 %s 가 같은 문장이다: %s", tc.name, prev, v.Message)
		}
		seen[v.Message] = tc.name
	}
	// 실제 값이 문장에 들어갔는지 — 한도만 말하면 무엇을 얼마나 줄여야 하는지 알 수 없다.
	for _, want := range []string{"12000×8000", "9000×9000", "8100만 픽셀"} {
		found := false
		for msg := range seen {
			found = found || strings.Contains(msg, want)
		}
		if !found {
			t.Fatal("실제 값이 문장에 없다:", want)
		}
	}
}

// 안내 문장이 상수에서 계산되는지 확인한다. 여기가 깨지면 한도는 바뀌었는데 화면 문구만
// 옛 숫자로 남아 있다는 뜻이다.
func TestSizeLabelsComeFromConstants(t *testing.T) {
	if limitLabel(MaxImageBytes) != "700 KB" {
		t.Fatal(limitLabel(MaxImageBytes))
	}
	if limitLabel(MaxTotalBytes) != "1400 KB" {
		t.Fatal(limitLabel(MaxTotalBytes))
	}
	if limitLabel(2<<20) != "2 MB" {
		t.Fatal(limitLabel(2 << 20))
	}
	// 올림이라야 1바이트 넘긴 파일이 한도와 같은 숫자로 찍히지 않는다.
	if sizeLabel(MaxImageBytes+1) != "701 KB" || sizeLabel(MaxImageBytes) != "700 KB" {
		t.Fatal(sizeLabel(MaxImageBytes+1), sizeLabel(MaxImageBytes))
	}
	if pixelLabel(maxImagePixels) != "2500만" || pixelLabel(maxImagePixels+1) != "2501만" {
		t.Fatal(pixelLabel(maxImagePixels), pixelLabel(maxImagePixels+1))
	}
}

func TestAttachmentLimitsSayWhy(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	s := &Store{fs}
	uid := fmt.Sprintf("asset-limit-%d", time.Now().UnixNano())

	// 옛 한도(300KiB)보다 큰 이미지가 이제 저장된다. Firestore 문서에도 실제로 들어간다.
	big := noisePNG(t, MaxImageBytes)
	if len(big) <= 300<<10 {
		t.Fatal("옛 한도보다 큰 이미지가 아니라 한도 상향을 검증하지 못한다:", len(big))
	}
	ids := []string{}
	for i := 0; i < 3; i++ {
		a, err := s.Upload(ctx, uid, Upload{Name: fmt.Sprintf("큰이미지%d.png", i), MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(big)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}

	rejected := func(_ Attachment, err error) error {
		t.Helper()
		if err == nil {
			t.Fatal("거절됐어야 한다")
		}
		return err
	}
	// 거절은 ErrInvalid가 아니라 ValidationError여야 Fail()이 고정 문구로 덮지 않는다.
	reason := func(err error) string {
		t.Helper()
		var v ValidationError
		if !errors.As(err, &v) {
			t.Fatal("ValidationError가 아니다:", err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Fatal("ErrInvalid로도 잡힌다 — Fail()이 고정 문구로 덮을 수 있다:", err)
		}
		return v.Message
	}

	over := bytes.Repeat([]byte("x"), MaxImageBytes+1)
	msg := reason(rejected(s.Upload(ctx, uid, Upload{Name: "초과.png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(over)})))
	if !strings.Contains(msg, limitLabel(MaxImageBytes)) || !strings.Contains(msg, sizeLabel(len(over))) {
		t.Fatal("한도와 실제 크기가 모두 들어 있지 않다:", msg)
	}

	// 합계 초과도 얼마까지인지와 지금 얼마인지를 말해야 한다.
	_, e = s.SaveTemplate(ctx, uid, "", TemplateInput{Name: "합계초과", AttachmentIDs: ids})
	msg = reason(e)
	if !strings.Contains(msg, limitLabel(MaxTotalBytes)) || !strings.Contains(msg, sizeLabel(len(big)*3)) {
		t.Fatal("합계 안내에 한도와 현재 크기가 모두 들어 있지 않다:", msg)
	}

	// 나머지 거절도 서로 다른 문장이어야 한다. 같으면 사용자는 무엇이 문제인지 알 수 없다.
	seen := map[string]bool{}
	for _, in := range []Upload{
		{Name: "깨짐.png", MimeType: "image/png", DataBase64: "!!!not base64!!!"},
		{Name: "빈.png", MimeType: "image/png", DataBase64: ""},
		{Name: "  ", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(big)},
		{Name: strings.Repeat("가", maxAttachmentNameRunes+1) + ".png", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(big)},
	} {
		m := reason(rejected(s.Upload(ctx, uid, in)))
		if seen[m] {
			t.Fatal("다른 원인인데 같은 문장이다:", m)
		}
		seen[m] = true
	}
}

// Fail()을 거쳐 실제 HTTP 응답까지 사유가 나가는지 확인한다. 여기가 고정 문구면
// 서버 안에서만 친절하고 화면은 그대로인 셈이다.
func TestUploadHTTPSaysWhy(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("emulator required")
	}
	ctx := context.Background()
	fs, e := firestore.NewClient(ctx, "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	defer fs.Close()
	uid := fmt.Sprintf("asset-http-%d", time.Now().UnixNano())
	mux := http.NewServeMux()
	Register(mux, fs, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), uid)))
		})
	})
	post := func(payload int) string {
		body := fmt.Sprintf(`{"name":"big.png","mimeType":"image/png","dataBase64":%q}`, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), payload)))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/sms/attachments", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
		var v struct{ Message string }
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err, w.Body.String())
		}
		if v.Message == "파일 또는 입력값을 확인해 주세요." {
			t.Fatal("고정 문구로 덮였다:", w.Body.String())
		}
		return v.Message
	}
	// 한도를 1바이트 넘긴 요청은 본문 상한에는 걸리지 않고 Upload까지 가서 실제 크기를 말한다.
	if m := post(MaxImageBytes + 1); !strings.Contains(m, limitLabel(MaxImageBytes)) || !strings.Contains(m, sizeLabel(MaxImageBytes+1)) {
		t.Fatal(m)
	}
	// 본문 상한에서 잘리는 큰 요청도 「형식 오류」가 아니라 크기 초과라고 말해야 한다.
	if m := post(2 << 20); !strings.Contains(m, limitLabel(MaxImageBytes)) {
		t.Fatal(m)
	}
}
