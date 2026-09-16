package messaging

import (
	"bytes"
	"cloud.google.com/go/firestore"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/sslim7/nature-was/internal/auth"
	"image"
	"image/png"
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
	if _, e = s.Upload(ctx, uid, Upload{Name: "위조", MimeType: "image/png", DataBase64: base64.StdEncoding.EncodeToString([]byte("fake"))}); e != ErrInvalid {
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
