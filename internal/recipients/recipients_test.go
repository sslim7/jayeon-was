package recipients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
)

func TestNormalizePhone(t *testing.T) {
	for _, s := range []string{"010-1234-5678", "01012345678"} {
		p, e := NormalizePhone(s)
		if e != nil || p != "01012345678" {
			t.Fatalf("normalize %q: %q %v", s, p, e)
		}
		if again, err := NormalizePhone(p); err != nil || again != p {
			t.Fatalf("normalization not idempotent: %q %q %v", p, again, err)
		}
	}
	for _, s := range []string{"", "+821012345678", "01112345678", "0101234567", "010123456789", "1234", "*123#", "+01012345678", "+8210abc5678", "+821012345678 ext 1", "++821012345678", "001012345678", "+82001012345678", "+1234567890123456"} {
		if _, e := NormalizePhone(s); e == nil {
			t.Errorf("accepted invalid %q", s)
		}
	}
	if _, e := NormalizePhone("+1 (415) 555-0123"); e == nil {
		t.Fatal("해외번호 허용")
	}

}
func TestValidation(t *testing.T) {
	for _, id := range []string{"", "..", "a/b", strings.Repeat("x", 129)} {
		if ValidateID(id) {
			t.Errorf("accepted %q", id)
		}
	}
	if _, e := validate(Input{Name: strings.Repeat("가", 101), Phone: "01012345678"}); e == nil {
		t.Fatal("long name accepted")
	}
}
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore emulator required")
	}
	fs, e := firestore.NewClient(context.Background(), "demo-jayeon")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { fs.Close() })
	return &Store{fs}, fmt.Sprintf("recipient-test-%d", time.Now().UnixNano())
}
func TestStoreCRUDIsolationAndConcurrentUniqueness(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	a, e := s.Save(ctx, uid, "", Input{Name: "첫 이름", Phone: "01012345678", GroupID: "가족"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Save(ctx, uid, "", Input{Name: "중복", Phone: "010-1234-5678"}); !errors.Is(e, ErrDuplicate) {
		t.Fatalf("duplicate: %v", e)
	}
	if _, e = s.Save(ctx, uid+"-other", a.ID, Input{Name: "공격", Phone: "01011112222"}); !errors.Is(e, ErrNotFound) {
		t.Fatalf("isolation: %v", e)
	}
	b, e := s.Save(ctx, uid, a.ID, Input{Name: "새 이름", Phone: "01099998888"})
	if e != nil || b.CreatedAt != a.CreatedAt || b.GroupID != "" {
		t.Fatalf("update: %+v %v", b, e)
	}
	if _, e = s.Save(ctx, uid, "", Input{Name: "해제된 번호", Phone: "01012345678"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Delete(ctx, uid+"-other", a.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if e = s.Delete(ctx, uid, a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Save(ctx, uid, "", Input{Name: "삭제후", Phone: "01099998888"}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Save(ctx, uid, "", Input{Name: "경쟁", Phone: "01077776666"})
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	successes, duplicates := 0, 0
	for e := range results {
		if e == nil {
			successes++
		} else if errors.Is(e, ErrDuplicate) {
			duplicates++
		} else {
			t.Fatal(e)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatal(successes, duplicates)
	}
}
func TestFilteredPaginationAndHTTP(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		group := "A"
		if i%2 == 0 {
			group = "B"
		}
		_, e := s.Save(ctx, uid, "", Input{Name: "검색 대상", Phone: fmt.Sprintf("0101000%04d", i), GroupID: group})
		if e != nil {
			t.Fatal(e)
		}
	}
	seen := map[string]bool{}
	cursorValue := ""
	for pages := 0; pages < 10; pages++ {
		p, e := s.List(ctx, uid, "검색", "A", cursorValue, 1)
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range p.Items {
			if seen[r.ID] || r.GroupID != "A" {
				t.Fatal("duplicate/unfiltered result")
			}
			seen[r.ID] = true
		}
		if p.NextCursor == nil {
			break
		}
		cursorValue = *p.NextCursor
	}
	if len(seen) != 3 {
		t.Fatal(len(seen))
	}
	p, e := s.List(ctx, uid, "010-1000-0001", "", "", 50)
	if e != nil || len(p.Items) != 1 {
		t.Fatalf("phone search %v %v", p, e)
	}
	if _, e = s.List(ctx, uid, "different", "A", cursorValue, 1); e == nil {
		t.Fatal("cursor filter not bound")
	}
	mux := http.NewServeMux()
	Register(mux, s.FS, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), uid)))
		})
	})
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{{"GET", "/recipients?limit=0", "", 400}, {"GET", "/recipients?cursor=invalid", "", 400}, {"GET", "/recipients?groupId=A", "", 200}, {"POST", "/recipients", `{"name":"신규","phone":"01022223333"}`, 201}, {"POST", "/recipients", `{"name":"중복","phone":"010-2222-3333"}`, 409}, {"PUT", "/recipients/missing", `{"name":"없음","phone":"01044445555"}`, 404}, {"DELETE", "/recipients/missing", "", 404}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatalf("%s %s status %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
	}
}
