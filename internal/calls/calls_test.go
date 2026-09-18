package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fixture() Record {
	return Record{CallID: "call-1", Contact: Contact{Name: "김고객", Phone: "021234567"}, Call: Metadata{FileName: "call.m4a", RecordedAt: "2026-01-01T14:00:00+09:00"}, CreatedAt: "2026-01-01T14:01:00+09:00", Status: "UPLOADING", Transcript: &Transcript{Text: "견적서 전달해주세요.", Segments: []Segment{{Start: 0, End: 5, Text: "견적서 전달해주세요."}}}, Analysis: &Analysis{SchemaVersion: 1, Summary: "견적 요청", Details: []Detail{}, Todos: []Todo{{Content: "견적서 전달", Source: "견적서 전달해주세요."}}, Decisions: []string{}, Consulting: Consulting{CustomerNeeds: []string{}, Questions: []string{}, Concerns: []string{}, Objections: []string{}, ImportantPoints: []string{}, Followups: []string{}}}, AI: &AI{Model: "local", ModelVersion: "1", ProcessedOnDevice: true}}
}
func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Record)
	}{
		{"external AI", func(r *Record) { r.AI.ProcessedOnDevice = false }},
		{"missing transcript", func(r *Record) { r.Transcript = nil }},
		{"future call", func(r *Record) { r.Call.RecordedAt = time.Now().Add(time.Hour).Format(time.RFC3339) }},
		{"invalid date", func(r *Record) { s := "다음주"; r.Analysis.Todos[0].DueDate = &s }},
		{"wrong ID", func(r *Record) { r.CallID = "other" }},
		{"path injection", func(r *Record) { s := "../other"; r.Contact.RecipientID = &s }},
		{"reverse segment", func(r *Record) { r.Transcript.Segments[0].End = -1 }},
		{"missing array", func(r *Record) { r.Analysis.Decisions = nil }},
		{"oversize analysis", func(r *Record) { r.Analysis.Summary = strings.Repeat("x", 32001) }},
	}
	r := fixture()
	if e := validate(&r, "call-1"); e != nil {
		t.Fatal(e)
	}
	if r.Status != "COMPLETED" {
		t.Fatal(r.Status)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := fixture()
			c.change(&r)
			if validate(&r, "call-1") == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
	summary := listRecord(r)
	if summary.Transcript != nil || summary.Analysis != nil || summary.Summary == "" {
		t.Fatal("list exposes heavy private content or misses summary")
	}
}

type fakeRepo struct {
	uid   string
	calls int
	q     string
	limit int
}

func (s *fakeRepo) Save(_ context.Context, uid string, p *payload) (Record, error) {
	s.uid = uid
	s.calls++
	return p.summary, nil
}
func (s *fakeRepo) Get(_ context.Context, uid, id string) (Record, error) {
	s.uid = uid
	s.calls++
	return fixture(), nil
}
func (s *fakeRepo) List(_ context.Context, uid, q string, limit int, cursor string) (Page, error) {
	s.uid = uid
	s.q, s.limit = q, limit
	s.calls++
	return Page{Items: []Record{}}, nil
}
func TestHTTPAuthAndValidation(t *testing.T) {
	store := &fakeRepo{}
	mux := http.NewServeMux()
	register(mux, store, func(h http.Handler) http.Handler { return h })
	req := httptest.NewRequest("GET", "/calls", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 401 || store.calls != 0 {
		t.Fatal("unauthenticated repository access")
	}
	req = httptest.NewRequest("GET", "/calls/call-1", nil)
	req = req.WithContext(auth.WithUserID(req.Context(), "owner-a"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 || store.uid != "owner-a" {
		t.Fatal("owner not derived from auth")
	}
	b, _ := json.Marshal(fixture())
	// 형식 오류는 400, 크기 초과는 413 이어야 클라이언트가 재시도 여부를 판단할 수 있다.
	for _, c := range []struct {
		name string
		body string
		code int
	}{
		{"trailing garbage", string(b) + ` {}`, 400},
		{"unknown field", strings.Replace(string(b), `"call_id"`, `"owner_id":"other","call_id"`, 1), 400},
		{"oversized body", strings.Repeat(" ", 6<<20) + string(b), 413},
		{"stored bytes over cap", strings.Replace(string(b), `"견적서 전달해주세요."`, `"`+strings.Repeat("\u2028a", 900000)+`"`, 1), 413},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := store.calls
			req := httptest.NewRequest("PUT", "/calls/call-1", strings.NewReader(c.body))
			req = req.WithContext(auth.WithUserID(req.Context(), "owner-a"))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != c.code || store.calls != before {
				t.Fatalf("status %d (want %d), store calls %d", w.Code, c.code, store.calls-before)
			}
		})
	}
	// PUT 응답은 목록용 요약뿐이다. 6 MiB 본문을 되돌려 주면 응답 직렬화에 같은 크기가
	// 한 벌 더 필요해 인스턴스가 OOM 으로 죽는다.
	req = httptest.NewRequest("PUT", "/calls/call-1", strings.NewReader(string(b)))
	req = req.WithContext(auth.WithUserID(req.Context(), "owner-a"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("저장 실패: %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil {
		t.Fatal(e)
	}
	if _, ok := got["transcript"]; ok {
		t.Fatal("PUT 응답이 원문을 되돌려준다")
	}
	if _, ok := got["analysis"]; ok {
		t.Fatal("PUT 응답이 분석을 되돌려준다")
	}
	if got["summary"] == "" || got["status"] != "COMPLETED" {
		t.Fatalf("요약 응답이 비어 있다: %v", got)
	}
}

// 저장 바이트 기준 한도. 요청 크기만 막으면 인코딩 증폭분이 Firestore Commit 한도를
// 넘겨 500 이 나고, 클라이언트는 일시 오류로 보고 무한 재시도한다.
func TestStoredSizeLimit(t *testing.T) {
	r := fixture()
	text := strings.Repeat("<", 1<<20)
	r.Transcript.Text = text
	p, e := prepare(&r, "call-1")
	if e != nil {
		t.Fatalf("HTML escape 없는 원문이 거부됐다: %v", e)
	}
	// `<` 가 \u003c 로 6배 부풀지 않는지 — 여기가 500 의 원인이었다.
	if len(p.transcript) > len(text)+4096 {
		t.Fatalf("escape 증폭이 남아 있다: 원문 %d → 저장 %d", len(text), len(p.transcript))
	}

	// Go 가 여전히 escape 하는 U+2028 은 2배로 늘어난다. 실제 저장 바이트가 막아야 한다.
	r = fixture()
	r.Transcript.Text = strings.Repeat("\u2028a", 900000)
	if _, e = prepare(&r, "call-1"); !errors.Is(e, errTooLarge) {
		t.Fatalf("저장 한도를 넘는 원문을 받았다: %v", e)
	}
}

// 기기 시계가 서버보다 몇 초 빠른 것은 정상이다. 여유가 없으면 그 통화는 영원히
// 업로드되지 않는다.
func TestClockSkewTolerance(t *testing.T) {
	r := fixture()
	r.Call.RecordedAt = time.Now().Add(2 * time.Minute).Format(time.RFC3339Nano)
	if e := validate(&r, "call-1"); e != nil {
		t.Fatalf("허용 범위의 시계 차이를 거부했다: %v", e)
	}
	r = fixture()
	r.Call.RecordedAt = time.Now().Add(10 * time.Minute).Format(time.RFC3339Nano)
	if validate(&r, "call-1") == nil {
		t.Fatal("허용치를 넘는 미래 시각을 받아들였다")
	}
}

// Real transaction, isolation, and >1MiB transcript verification when emulator is available.
func TestFirestoreIsolationIdempotencyAndShards(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore emulator required")
	}
	ctx := context.Background()
	client, e := firestore.NewClient(ctx, "nature-call-tests")
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	s := &Store{client}
	uid := fmtID()
	text := strings.Repeat("긴 통화 원문. ", 100000)
	// prepare 는 직렬화 후 디코딩본을 버리므로 저장할 때마다 새로 만든다.
	build := func(summary string) *payload {
		r := fixture()
		r.Transcript.Text = text
		if summary != "" {
			r.Analysis.Summary = summary
		}
		p, err := prepare(&r, r.CallID)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	saved, e := s.Save(ctx, uid, build(""))
	if e != nil {
		t.Fatal(e)
	}
	if saved.Transcript != nil || saved.Analysis != nil || saved.Summary == "" {
		t.Fatal("저장 응답이 원문/분석을 그대로 되돌려준다")
	}
	if _, e = s.Save(ctx, uid, build("")); e != nil {
		t.Fatalf("retry: %v", e)
	}
	got, e := s.Get(ctx, uid, "call-1")
	if e != nil || got.Transcript.Text != text || len(got.Analysis.Todos) != 1 {
		t.Fatalf("roundtrip: %v", e)
	}
	if _, e = s.Get(ctx, uid+"-other", "call-1"); status.Code(e) != codes.NotFound {
		t.Fatalf("owner isolation: %v", e)
	}
	page, e := s.List(ctx, uid, "", defaultPageSize, "")
	if e != nil || len(page.Items) != 1 || page.Items[0].Transcript != nil || page.NextCursor != "" {
		t.Fatalf("list: %v", e)
	}
	if _, e = s.Save(ctx, uid, build("changed")); e != ErrConflict {
		t.Fatalf("overwrite should conflict: %v", e)
	}
}

// 저장된 문서가 기대한 모양이 아니어도 panic 으로 인스턴스를 끊지 않는다.
// panic 하나면 그 요청뿐 아니라 같은 인스턴스의 SMS·로그인 요청까지 함께 죽는다.
func TestFirestoreMalformedDocuments(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore emulator required")
	}
	ctx := context.Background()
	client, e := firestore.NewClient(ctx, "nature-call-tests")
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	s := &Store{client}
	meta, _ := marshalCompact(listRecord(func() Record { r := fixture(); validate(&r, r.CallID); return r }()))

	uid := fmtID() + "-types"
	// transcriptShards/todoCount 가 숫자가 아니다 → 검사 없는 단언이면 panic 이었다.
	if _, e = client.Collection("users").Doc(uid).Collection("calls").Doc("call-1").Set(ctx, map[string]any{
		"record": meta, "digest": "x", "recordedAt": time.Now(), "transcriptShards": "one", "todoCount": nil,
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, uid, "call-1"); !errors.Is(e, errStored) {
		t.Fatalf("타입이 어긋난 문서에서 에러가 나지 않았다: %v", e)
	}

	uid = fmtID() + "-corrupt"
	calls := client.Collection("users").Doc(uid).Collection("calls")
	// 손상된 문서 하나가 목록 전체를 막으면 안 된다. 건너뛰고 나머지는 보여 준다.
	if _, e = calls.Doc("call-0").Set(ctx, map[string]any{
		"record": []byte("{not json"), "digest": "x", "recordedAt": time.Now().Add(-time.Hour), "transcriptShards": int64(0), "todoCount": int64(0),
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = calls.Doc("call-1").Set(ctx, map[string]any{
		"record": meta, "digest": "y", "recordedAt": time.Now().Add(-2 * time.Hour), "transcriptShards": int64(0), "todoCount": int64(0),
	}); e != nil {
		t.Fatal(e)
	}
	page, e := s.List(ctx, uid, "", defaultPageSize, "")
	if e != nil {
		t.Fatalf("손상 문서 하나가 목록 전체를 막는다: %v", e)
	}
	if len(page.Items) != 1 || page.Items[0].CallID != "call-1" {
		t.Fatalf("손상 문서를 건너뛰지 못했다: %v", page.Items)
	}
}

// 페이지 경계. 마지막 페이지에는 nextCursor 가 없어야 앱이 순회를 멈춘다.
func TestFirestorePaging(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore emulator required")
	}
	ctx := context.Background()
	client, e := firestore.NewClient(ctx, "nature-call-tests")
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	s := &Store{client}
	uid := fmtID() + "-paging"
	calls := client.Collection("users").Doc(uid).Collection("calls")
	base := time.Now().Add(-time.Hour)
	for i := 0; i < defaultPageSize+1; i++ {
		r := fixture()
		r.CallID = fmt.Sprintf("call-%03d", i)
		if e = validate(&r, r.CallID); e != nil {
			t.Fatal(e)
		}
		meta, _ := marshalCompact(listRecord(r))
		if _, e = calls.Doc(r.CallID).Set(ctx, map[string]any{
			"record": meta, "digest": r.CallID, "recordedAt": base.Add(time.Duration(i) * time.Minute), "transcriptShards": int64(0), "todoCount": int64(0),
		}); e != nil {
			t.Fatal(e)
		}
	}
	first, e := s.List(ctx, uid, "", defaultPageSize, "")
	if e != nil || len(first.Items) != defaultPageSize || first.NextCursor == "" {
		t.Fatalf("첫 페이지: %v, %d건, cursor=%q", e, len(first.Items), first.NextCursor)
	}
	second, e := s.List(ctx, uid, "", defaultPageSize, first.NextCursor)
	if e != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("마지막 페이지: %v, %d건, cursor=%q", e, len(second.Items), second.NextCursor)
	}
	if _, e = s.List(ctx, uid, "", defaultPageSize, "not-a-cursor"); !errors.Is(e, ErrCursor) {
		t.Fatalf("잘못된 커서: %v", e)
	}
}

func fmtID() string { return "test-" + time.Now().Format("20060102150405.000000000") }

// limit 과 검색어는 Firestore 를 만지기 **전에** 걸러야 한다. 잘못된 값으로 쿼리를 날리면
// 읽기 비용만 쓰고 500 이 난다. 그래서 클라이언트 없이도(nil) 검증만 확인할 수 있다.
func TestListParameterValidation(t *testing.T) {
	s := &Store{nil}
	ctx := context.Background()
	for _, c := range []struct {
		why    string
		q      string
		limit  int
		cursor string
	}{
		{"limit 0", "", 0, ""},
		{"limit 음수", "", -1, ""},
		{"limit 상한 초과", "", maxPageSize + 1, ""},
		{"검색어 길이 초과", strings.Repeat("가", maxQueryRunes+1), defaultPageSize, ""},
		{"커서 길이 초과", "", defaultPageSize, strings.Repeat("x", 2049)},
		{"커서 형식 오류", "", defaultPageSize, "not-a-cursor"},
	} {
		if _, e := s.List(ctx, "owner", c.q, c.limit, c.cursor); !errors.Is(e, ErrCursor) {
			t.Errorf("%s: %v", c.why, e)
		}
	}
}

// 핸들러가 limit 과 q 를 그대로 넘기는지. 앱이 한 화면을 30+30 두 번에 나눠 부르던 것을
// 고치는 변경이라, limit 이 조용히 무시되면 고친 것이 아무 효과가 없다.
func TestListQueryParameters(t *testing.T) {
	store := &fakeRepo{}
	mux := http.NewServeMux()
	register(mux, store, func(h http.Handler) http.Handler { return h })
	get := func(url string) int {
		req := httptest.NewRequest("GET", url, nil)
		req = req.WithContext(auth.WithUserID(req.Context(), "owner-a"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if get("/calls") != 200 || store.limit != defaultPageSize || store.q != "" {
		t.Fatalf("기본값: limit=%d q=%q", store.limit, store.q)
	}
	if get("/calls?limit=50&q=%EA%B9%80%202222") != 200 || store.limit != 50 || store.q != "김 2222" {
		t.Fatalf("limit/q 전달: limit=%d q=%q", store.limit, store.q)
	}
	if code := get("/calls?limit=abc"); code != 400 {
		t.Fatalf("숫자가 아닌 limit: %d", code)
	}
	// 범위(1~100) 검증은 Store.List 한 곳에서 한다 — TestListParameterValidation 참고.
}

// 서버 검색. 이름 부분일치·전화번호 뒷자리·표기 차이·스캔 상한·커서 귀속까지 한 번에 본다.
func TestFirestoreSearch(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore emulator required")
	}
	ctx := context.Background()
	client, e := firestore.NewClient(ctx, "nature-call-tests")
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	s := &Store{client}
	uid := fmtID() + "-search"
	calls := client.Collection("users").Doc(uid).Collection("calls")
	base := time.Now().Add(-time.Hour)
	// recordedAt 오름차순으로 넣으므로 목록(최신순)에서는 뒤집혀 나온다.
	people := []Contact{
		{Name: "김고객", Phone: "01011112222"},
		{Name: "이영희", Phone: "01033334444"},
		{Name: "박철수", Phone: "+821055556666"}, // 구버전 저장 형식
		{Name: "김영수", Phone: "01077778888"},
		{Name: "Lee", Phone: "01099990000"},
	}
	for i, c := range people {
		r := fixture()
		r.CallID = fmt.Sprintf("call-%03d", i)
		r.Contact = c
		if e = validate(&r, r.CallID); e != nil {
			t.Fatal(e)
		}
		meta, _ := marshalCompact(listRecord(r))
		if _, e = calls.Doc(r.CallID).Set(ctx, map[string]any{
			"record": meta, "digest": r.CallID, "recordedAt": base.Add(time.Duration(i) * time.Minute), "transcriptShards": int64(0), "todoCount": int64(0),
		}); e != nil {
			t.Fatal(e)
		}
	}
	names := func(p Page) []string {
		out := []string{}
		for _, r := range p.Items {
			out = append(out, r.Contact.Name)
		}
		return out
	}
	for _, c := range []struct {
		why  string
		q    string
		want string
	}{
		{"빈 검색어는 전체", "", "Lee,김영수,박철수,이영희,김고객"},
		{"이름 부분일치", "김", "김영수,김고객"},
		{"이름 대소문자 무시", "lee", "Lee"},
		{"전화번호 뒷4자리", "2222", "김고객"},
		{"하이픈 표기", "7777-8888", "김영수"},
		{"저장된 +82 번호도 뒷자리로 걸린다", "6666", "박철수"},
		{"검색어의 +82 표기", "+82 10-9999-0000", "Lee"},
		{"글자+숫자는 둘 다 맞아야 한다", "김 8888", "김영수"},
		{"일치 없음", "없는사람", ""},
	} {
		p, e := s.List(ctx, uid, c.q, defaultPageSize, "")
		if e != nil {
			t.Fatalf("%s: %v", c.why, e)
		}
		if got := strings.Join(names(p), ","); got != c.want {
			t.Errorf("%s: q=%q → %q, want %q", c.why, c.q, got, c.want)
		}
		if p.NextCursor != "" {
			t.Errorf("%s: 마지막 페이지인데 커서가 남았다", c.why)
		}
	}

	// limit 경계. 1 과 maxPageSize 는 통과해야 한다.
	if p, e := s.List(ctx, uid, "", 1, ""); e != nil || len(p.Items) != 1 || p.NextCursor == "" {
		t.Fatalf("limit=1: %v, %d건, cursor=%q", e, len(p.Items), p.NextCursor)
	}
	if p, e := s.List(ctx, uid, "", maxPageSize, ""); e != nil || len(p.Items) != len(people) || p.NextCursor != "" {
		t.Fatalf("limit=%d: %v, %d건", maxPageSize, e, len(p.Items))
	}

	// 검색 결과의 페이징. limit 이 차면 커서가 남고, 이어 받으면 나머지가 온다.
	first, e := s.List(ctx, uid, "김", 1, "")
	if e != nil || len(first.Items) != 1 || first.Items[0].Contact.Name != "김영수" || first.NextCursor == "" {
		t.Fatalf("검색 첫 페이지: %v, %v, cursor=%q", e, names(first), first.NextCursor)
	}
	second, e := s.List(ctx, uid, "김", 1, first.NextCursor)
	if e != nil || len(second.Items) != 1 || second.Items[0].Contact.Name != "김고객" || second.NextCursor != "" {
		t.Fatalf("검색 마지막 페이지: %v, %v, cursor=%q", e, names(second), second.NextCursor)
	}
	// 커서는 검색어에 귀속된다. 다른 검색어로 이어 읽으면 앞부분이 통째로 빠진 목록이 나온다.
	if _, e = s.List(ctx, uid, "이", 1, first.NextCursor); !errors.Is(e, ErrCursor) {
		t.Fatalf("검색어가 바뀐 커서: %v", e)
	}
	if _, e = s.List(ctx, uid, "", 1, first.NextCursor); !errors.Is(e, ErrCursor) {
		t.Fatalf("검색 커서를 검색 없는 조회에 쓰면: %v", e)
	}

	// 스캔 상한. 자리가 남아도 상한에 걸리면 찾은 만큼만 주고 커서를 남긴다 —
	// 그래야 앱이 "여기까지만 찾았다" 를 알고 이어 받는다.
	page, e := s.list(ctx, uid, "김", 10, "", 2)
	if e != nil || len(page.Items) != 1 || page.Items[0].Contact.Name != "김영수" || page.NextCursor == "" {
		t.Fatalf("상한 1회차: %v, %v, cursor=%q", e, names(page), page.NextCursor)
	}
	page, e = s.list(ctx, uid, "김", 10, page.NextCursor, 2)
	if e != nil || len(page.Items) != 0 || page.NextCursor == "" {
		t.Fatalf("상한 2회차(한 건도 못 찾았지만 계속 있다): %v, %v, cursor=%q", e, names(page), page.NextCursor)
	}
	page, e = s.list(ctx, uid, "김", 10, page.NextCursor, 2)
	if e != nil || len(page.Items) != 1 || page.Items[0].Contact.Name != "김고객" || page.NextCursor != "" {
		t.Fatalf("상한 3회차(끝): %v, %v, cursor=%q", e, names(page), page.NextCursor)
	}
}
