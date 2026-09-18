package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/httpx"
)

// fakeSessions 는 SessionStore 의 인메모리 구현이다.
//
// 🔴 **키가 `uid + "/" + sid` 다.** 실제 저장소가 `users/{uid}/sessions/{sid}` 경로로
// 남의 세션에 아예 닿지 못하는 것과 같은 성질을 흉내 낸 것이다. 여기를 sid 만으로 키를
// 잡으면 "남의 세션은 404" 테스트가 **핸들러를 고쳐도 계속 통과해** 아무것도 못 지킨다.
type fakeSessions struct {
	items   map[string]Session
	next    int
	creates int
	touches int
	revokes int
	// err 가 있으면 모든 메서드가 그것을 낸다.
	err error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{items: map[string]Session{}}
}

func fakeKey(uid, sid string) string { return uid + "/" + sid }

func (f *fakeSessions) CreateSession(_ context.Context, uid string, s Session) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.creates++
	f.next++
	s.ID = fmt.Sprintf("sess-%d", f.next)
	f.items[fakeKey(uid, s.ID)] = s
	return s.ID, nil
}

func (f *fakeSessions) GetSession(_ context.Context, uid, sid string) (Session, error) {
	if f.err != nil {
		return Session{}, f.err
	}
	s, ok := f.items[fakeKey(uid, sid)]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

func (f *fakeSessions) ListSessions(_ context.Context, uid string, limit int) ([]Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []Session
	for k, s := range f.items {
		if strings.HasPrefix(k, uid+"/") {
			out = append(out, s)
		}
	}
	// 실제 저장소는 lastSeenAt 내림차순이다. 건수가 적어 단순 정렬로 흉내 낸다.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastSeenAt.After(out[i].LastSeenAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSessions) TouchSession(_ context.Context, uid, sid string, at time.Time, label string, accesses []Access) error {
	if f.err != nil {
		return f.err
	}
	s, ok := f.items[fakeKey(uid, sid)]
	if !ok {
		return ErrSessionNotFound
	}
	f.touches++
	s.LastSeenAt = at
	s.DeviceLabel = label
	s.Accesses = accesses
	// 🔴 RevokedAt 을 건드리지 않는다 — 실제 저장소가 Set 이 아니라 Update 를 쓰는 이유다.
	f.items[fakeKey(uid, sid)] = s
	return nil
}

func (f *fakeSessions) RevokeSession(_ context.Context, uid, sid string, at time.Time) error {
	if f.err != nil {
		return f.err
	}
	s, ok := f.items[fakeKey(uid, sid)]
	if !ok {
		return ErrSessionNotFound
	}
	f.revokes++
	s.RevokedAt = at
	f.items[fakeKey(uid, sid)] = s
	return nil
}

var _ SessionStore = (*fakeSessions)(nil)

// ── 헬퍼 ────────────────────────────────────────────────────────────────────

// loginAt 은 주어진 시각과 User-Agent 로 로그인하고 토큰 세트를 돌려준다.
func loginAt(t *testing.T, h *Handler, at time.Time, ua string) TokenSet {
	t.Helper()
	h.now = func() time.Time { return at }
	h.tokens.now = func() time.Time { return at }
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"user@example.com","password":"current-password"}`))
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	w := httptest.NewRecorder()
	h.login(w, r)
	expectStatus(t, w, 200)
	var body struct {
		TokenSet
		MustChangePassword bool `json:"mustChangePassword"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.TokenSet
}

// refreshAt 은 주어진 시각과 User-Agent 로 갱신한다.
func refreshAt(t *testing.T, h *Handler, at time.Time, ua, token string) *httptest.ResponseRecorder {
	t.Helper()
	h.now = func() time.Time { return at }
	h.tokens.now = func() time.Time { return at }
	body, _ := json.Marshal(map[string]string{"refreshToken": token})
	r := httptest.NewRequest("POST", "/auth/refresh", strings.NewReader(string(body)))
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	w := httptest.NewRecorder()
	h.refresh(w, r)
	return w
}

func refreshTokenOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var set TokenSet
	if err := json.Unmarshal(w.Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	return set.RefreshToken
}

// listSessionsAs 는 인증된 요청처럼 세션 목록을 부른다.
func listSessionsAs(t *testing.T, h *Handler, uid, sid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/auth/sessions", nil)
	ctx := WithSessionID(WithUserID(r.Context(), uid), sid)
	w := httptest.NewRecorder()
	h.listSessions(w, r.WithContext(ctx))
	return w
}

// revokeSessionAs 는 인증된 요청처럼 세션 하나를 끊는다.
func revokeSessionAs(t *testing.T, h *Handler, uid, currentSid, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("DELETE", "/auth/sessions/"+target, nil)
	ctx := WithSessionID(WithUserID(r.Context(), uid), currentSid)
	r = r.WithContext(ctx)
	r.SetPathValue("sessionId", target)
	w := httptest.NewRecorder()
	h.revokeSession(w, r)
	return w
}

func decodeSessions(t *testing.T, w *httptest.ResponseRecorder) []sessionDTO {
	t.Helper()
	var body struct {
		Sessions []sessionDTO `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return body.Sessions
}

// 2026-09-19 09:00 KST. 날짜 경계 테스트가 읽기 쉽도록 KST 정오 근처를 기준으로 잡는다.
var sessionBase = time.Date(2026, 9, 19, 0, 0, 0, 0, kst)

// ── 테스트 ──────────────────────────────────────────────────────────────────

// TestLoginCreatesSession 은 **로그인이 세션을 만들고 그 id 를 두 토큰에 싣는다**는 것을
// 못 박는다. 이것이 빠지면 로그인은 멀쩡히 성공하는데 15분 뒤 갱신에서 전부 401 이 된다.
//
// 액세스 토큰에도 세션 id 가 실리는지를 함께 본다 — 그것이 없으면 세션 목록의 「지금 이
// 기기」가 영원히 뜨지 않고, 사용자는 어느 줄을 끊으면 자기가 로그아웃되는지 알 수 없다.
func TestLoginCreatesSession(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	set := loginAt(t, h, sessionBase, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")
	if sessions.creates != 1 {
		t.Fatalf("세션 생성 = %d", sessions.creates)
	}
	if set.ExpiresInSec != 900 {
		t.Fatalf("expiresInSec = %d — 앱이 이 값으로 재발급 시점을 잡는다", set.ExpiresInSec)
	}
	_, _, sid, err := h.tokens.ParseRefresh(set.RefreshToken)
	if err != nil || sid == "" {
		t.Fatal("리프레시 토큰에 세션 id 가 없다", sid, err)
	}
	uid, asid, err := h.tokens.ParseAccess(set.AccessToken)
	if err != nil || uid != "user-1" || asid != sid {
		t.Fatal("액세스 토큰의 세션 id 가 다르다", uid, asid, err)
	}
	s, err := sessions.GetSession(context.Background(), "user-1", sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.DeviceLabel != "Chrome · macOS" {
		t.Fatalf("기기 이름 = %q", s.DeviceLabel)
	}
	if !s.CreatedAt.Equal(sessionBase) || !s.LastSeenAt.Equal(sessionBase) || len(s.Accesses) != 1 {
		t.Fatal("세션 초기 상태가 다르다", s)
	}
	if !s.RevokedAt.IsZero() {
		t.Fatal("새 세션이 끊긴 상태다")
	}
}

// TestLoginFailsWhenSessionStoreFails 는 **세션을 못 만들면 로그인이 실패한다**는 판단을
// 못 박는다.
//
// 🔴 통과시키면 더 나쁘다. 세션 없이 나간 토큰은 첫 갱신에서 401 이라, 사용자는 로그인
// 성공 화면을 보고 15분 뒤 아무 안내 없이 로그인 화면으로 돌아온다. 그렇다고 갱신 쪽이
// 세션 없는 토큰을 받아 주면 그 갈래가 세션 검사를 통째로 우회하는 구멍이 된다.
// 500 이면 앱이 「다시 시도해 주세요」를 보여 주고 사용자가 그 자리에서 다시 누른다.
func TestLoginFailsWhenSessionStoreFails(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	sessions.err = errors.New("firestore unavailable")
	w := request(t, h.login, map[string]string{"email": "user@example.com", "password": "current-password"}, "")
	expectStatus(t, w, 500)
	if strings.Contains(w.Body.String(), "firestore") {
		t.Fatal("저장소 에러 문구가 응답으로 샜다", w.Body.String())
	}
}

// TestRefreshRejectsSessionlessToken 은 **세션 장치 이전에 발급된 리프레시 토큰을 거절**
// 하는지 본다.
//
// 🔴 받아 주는 갈래를 만들지 않기로 한 결정이 이 한 줄에 걸려 있다. 그 갈래는 「세션
// 검사를 건너뛰는 길」이고, 끊긴 기기가 옛 토큰을 들고 그리로 들어온다. 배포 직후 모두
// 한 번 다시 로그인하는 것이 그 대가다.
//
// 응답이 **기존 인증 실패와 같은 401** 이어야 앱이 이미 있는 경로로 로그인 화면에 보낸다.
func TestRefreshRejectsSessionlessToken(t *testing.T) {
	h, s, _ := testHandlerWithSessions(t)
	s.account.TokenVersion = 2
	legacy, err := h.tokens.issue("user-1", TokenUseRefresh, RefreshTokenTTL, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	w := refreshAt(t, h, sessionBase, "", legacy)
	expectStatus(t, w, 401)
	// 코드 문자열까지 본다. 앱은 코드 하나로 분기하므로 401 만 맞고 코드가 다르면
	// 화면에는 「알 수 없는 오류」가 뜨고 사용자는 로그인 화면으로 가지 못한다.
	if !strings.Contains(w.Body.String(), httpx.CodeUnauthorized) {
		t.Fatal("앱이 세션 만료로 읽을 수 없는 응답이다", w.Body.String())
	}
}

// TestRefreshChecksSessionAndLimitsWrites 는 이 기능의 중심 두 가지를 한 번에 본다.
//
//  1. 갱신이 세션을 확인하고 **세션 id 를 그대로 물려준다.** 갱신마다 새 세션을 만들면
//     목록이 갱신 횟수만큼 늘어나 사용자가 자기 기기를 찾을 수 없게 된다.
//  2. 🔴 **매 갱신마다 쓰지 않는다.** 액세스 토큰 15분이면 기기 하나가 하루 최대 96번
//     갱신한다. 그때마다 문서를 다시 쓰면 얻는 정보 없이 쓰기만 나가고, 접속 기록 10건은
//     전부 「방금 전」이 되어 아무것도 알려 주지 못한다.
//
// 쓰는 조건은 날짜(KST) 변경과 기기 표시 변경이다. 같은 날 여러 번 갱신해도 쓰기는 없다.
func TestRefreshChecksSessionAndLimitsWrites(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	const chrome = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	set := loginAt(t, h, sessionBase, chrome)
	_, _, sid, _ := h.tokens.ParseRefresh(set.RefreshToken)

	token := set.RefreshToken
	// 같은 날 네 번 갱신 — 한 번도 쓰지 않아야 한다.
	for i := 1; i <= 4; i++ {
		w := refreshAt(t, h, sessionBase.Add(time.Duration(i)*time.Hour), chrome, token)
		expectStatus(t, w, 200)
		token = refreshTokenOf(t, w)
		_, _, got, _ := h.tokens.ParseRefresh(token)
		if got != sid {
			t.Fatalf("갱신이 세션을 바꿨다: %s → %s", sid, got)
		}
	}
	if sessions.touches != 0 {
		t.Fatalf("같은 날 갱신에 쓰기가 %d번 일어났다 — 하루 최대 96번 갱신을 생각하라", sessions.touches)
	}

	// 다음 날(KST) 갱신 — 한 번 쓴다.
	next := sessionBase.Add(25 * time.Hour)
	expectStatus(t, refreshAt(t, h, next, chrome, token), 200)
	if sessions.touches != 1 {
		t.Fatalf("날짜가 바뀌었는데 쓰기 = %d", sessions.touches)
	}
	s, _ := sessions.GetSession(context.Background(), "user-1", sid)
	if !s.LastSeenAt.Equal(next) || len(s.Accesses) != 2 || !s.Accesses[0].At.Equal(next) {
		t.Fatal("마지막 접속과 기록이 갱신되지 않았다", s)
	}

	// 기기 표시가 바뀌면 같은 날이어도 쓴다. 안 그러면 목록이 옛 이름으로 굳는다.
	expectStatus(t, refreshAt(t, h, next.Add(time.Hour), "NatureApp/1.4.0 (Linux; Android 14) Chrome/140", token), 200)
	if sessions.touches != 2 {
		t.Fatalf("기기 표시가 바뀌었는데 쓰기 = %d", sessions.touches)
	}
	s, _ = sessions.GetSession(context.Background(), "user-1", sid)
	if s.DeviceLabel != "네이처 앱 1.4.0 · 안드로이드" {
		t.Fatalf("기기 표시 = %q", s.DeviceLabel)
	}
}

// TestAccessHistoryStaysAtTen 은 **접속 기록이 10건을 넘지 않는다**는 것을 못 박는다.
//
// 🔴 상한이 없으면 세션 문서가 접속 횟수만큼 자란다. 별도 컬렉션에 한 건씩 쌓지 않기로
// 한 대신 문서 하나에 담기 때문에, 이 상한이 곧 문서 크기의 상한이다.
// 최신이 앞이라는 것도 함께 본다 — 순서가 뒤집히면 목록에 옛날 기록이 먼저 뜬다.
func TestAccessHistoryStaysAtTen(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	const ua = "Mozilla/5.0 (X11; Linux x86_64) Firefox/131.0"
	set := loginAt(t, h, sessionBase, ua)
	_, _, sid, _ := h.tokens.ParseRefresh(set.RefreshToken)
	token := set.RefreshToken
	// 25일 동안 하루 한 번씩 갱신한다.
	for day := 1; day <= 25; day++ {
		at := sessionBase.Add(time.Duration(day) * 24 * time.Hour)
		w := refreshAt(t, h, at, ua, token)
		expectStatus(t, w, 200)
		token = refreshTokenOf(t, w)
	}
	s, _ := sessions.GetSession(context.Background(), "user-1", sid)
	if len(s.Accesses) != maxAccesses {
		t.Fatalf("접속 기록 = %d건, want %d", len(s.Accesses), maxAccesses)
	}
	for i := 1; i < len(s.Accesses); i++ {
		if !s.Accesses[i-1].At.After(s.Accesses[i].At) {
			t.Fatal("최신이 앞이 아니다", s.Accesses)
		}
	}
	if sessions.touches != 25 {
		t.Fatalf("하루 한 번 쓰기가 아니다: %d", sessions.touches)
	}
}

// TestRevokedSessionCannotRefresh 는 **이 기능의 전부**다.
//
// 끊은 세션의 리프레시가 401 이 되지 않으면 「이 기기만 끊기」는 아무 일도 하지 않는다.
// 그리고 이 한 줄을 지워도 **다른 테스트는 전부 통과하고 아무 에러도 나지 않는다** —
// 로그인도 갱신도 멀쩡히 돌고, 끊은 기기가 계속 쓰이고 있다는 사실만 조용히 남는다.
//
// 함께 보는 것: 끊은 뒤에도 **다른 기기의 갱신은 멀쩡해야 한다.** 여기가 깨지면
// 「기기 하나 끊기」가 실제로는 「전부 끊기」다.
func TestRevokedSessionCannotRefresh(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	phone := loginAt(t, h, sessionBase, "NatureApp/1.4.0 (Linux; Android 14) Chrome/140")
	laptop := loginAt(t, h, sessionBase, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/140.0.0.0 Safari/537.36")
	_, _, phoneSid, _ := h.tokens.ParseRefresh(phone.RefreshToken)
	_, _, laptopSid, _ := h.tokens.ParseRefresh(laptop.RefreshToken)

	at := sessionBase.Add(2 * time.Hour)
	h.now = func() time.Time { return at }
	w := revokeSessionAs(t, h, "user-1", laptopSid, phoneSid)
	expectStatus(t, w, 200)
	var out revokeSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.RevokedAt.Equal(at.UTC()) || !out.AccessibleUntilAtMost.Equal(at.Add(AccessTokenTTL).UTC()) {
		t.Fatal("언제까지 쓸 수 있는지가 응답에 없다", w.Body.String())
	}
	if out.Current {
		t.Fatal("남의 기기를 끊었는데 current 가 참이다 — 앱이 자기 토큰을 지운다")
	}

	// 🔴 끊은 기기는 갱신할 수 없다.
	expectStatus(t, refreshAt(t, h, at.Add(time.Minute), "", phone.RefreshToken), 401)
	// 🔴 다른 기기는 멀쩡해야 한다.
	expectStatus(t, refreshAt(t, h, at.Add(time.Minute), "", laptop.RefreshToken), 200)
	if sessions.revokes != 1 {
		t.Fatalf("끊기 쓰기 = %d", sessions.revokes)
	}

	// 지금 이 기기를 끊으면 current 가 참이다. 앱은 그 신호로 저장된 토큰을 버린다 —
	// 안 그러면 남은 액세스 토큰으로 AccessTokenTTL 동안 계속 돌아다닌다.
	self := revokeSessionAs(t, h, "user-1", laptopSid, laptopSid)
	expectStatus(t, self, 200)
	if err := json.Unmarshal(self.Body.Bytes(), &out); err != nil || !out.Current {
		t.Fatal("자기 기기 끊기에 current 가 없다", self.Body.String())
	}
}

// TestRevokeIsIdempotentAndKeepsFirstDeadline 은 **두 번 끊어도 한계 시각이 밀리지
// 않는다**는 것을 못 박는다.
//
// 🔴 지금 시각으로 덮어쓰면 사용자가 버튼을 누를 때마다 「언제까지 쓸 수 있는가」가 뒤로
// 간다 — 화면상으로는 「끊었는데 계속 쓸 수 있다」로 보이고, 실제로 기다려야 하는 시간도
// 늘어난다. 끊긴 세션에는 쓰기를 한 번도 더 하지 않는 것으로 그 사고를 막는다.
func TestRevokeIsIdempotentAndKeepsFirstDeadline(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	set := loginAt(t, h, sessionBase, "NatureApp/1.4.0 (Linux; Android 14) Chrome/140")
	_, _, sid, _ := h.tokens.ParseRefresh(set.RefreshToken)

	first := sessionBase.Add(time.Hour)
	h.now = func() time.Time { return first }
	expectStatus(t, revokeSessionAs(t, h, "user-1", "", sid), 200)

	h.now = func() time.Time { return first.Add(5 * time.Minute) }
	w := revokeSessionAs(t, h, "user-1", "", sid)
	expectStatus(t, w, 200)
	var out revokeSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.RevokedAt.Equal(first.UTC()) || !out.AccessibleUntilAtMost.Equal(first.Add(AccessTokenTTL).UTC()) {
		t.Fatal("다시 끊었더니 한계 시각이 밀렸다", w.Body.String())
	}
	if sessions.revokes != 1 {
		t.Fatalf("끊긴 세션에 쓰기가 다시 일어났다: %d", sessions.revokes)
	}
}

// TestCannotRevokeAnotherUsersSession 은 🔴 **남의 세션을 끊을 수 없다**는 것을 못 박는다.
//
// 403 이 아니라 **404** 다. 403 은 「그 id 는 실제로 있다」를 알려 주는 것과 같고, 세션
// id 는 목록 응답에 그대로 나가는 값이라 찍어 볼 수 있다(internal/calls/audio.go 의 load).
// 모양이 이상한 id 도 400 이 아니라 404 로 눕힌다 — 응답이 갈리면 그 자체가 정보다.
func TestCannotRevokeAnotherUsersSession(t *testing.T) {
	h, _, sessions := testHandlerWithSessions(t)
	other, err := sessions.CreateSession(context.Background(), "user-2", Session{
		DeviceLabel: "Chrome · Windows", CreatedAt: sessionBase, LastSeenAt: sessionBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{other, "존재하지않음", "", "../users", strings.Repeat("a", 200)} {
		w := revokeSessionAs(t, h, "user-1", "", target)
		expectStatus(t, w, 404)
		if !strings.Contains(w.Body.String(), CodeSessionNotFound) {
			t.Fatal(w.Body.String())
		}
	}
	// 남의 세션은 여전히 살아 있어야 한다. 404 를 주고 뒤에서 끊어 버리면 최악이다.
	s, err := sessions.GetSession(context.Background(), "user-2", other)
	if err != nil || !s.RevokedAt.IsZero() {
		t.Fatal("남의 세션이 끊겼다", s, err)
	}
	if w := listSessionsAs(t, h, "user-1", ""); len(decodeSessions(t, w)) != 0 {
		t.Fatal("남의 세션이 목록에 보인다")
	}
}

// TestSessionListShowsCurrentAndDeadline 은 목록이 답해야 하는 두 질문을 본다.
//
//   - 「어느 줄이 지금 이 기기인가」 — 액세스 토큰의 세션 id 로 가린다.
//   - 「끊었는데 진짜 끊긴 건가」 — 끊긴 줄에 revokedAt 과 accessibleUntilAtMost 가 실린다.
//     살아 있는 줄에는 **null 이다.** 끊기 전에는 한계가 없기 때문이다(계속 갱신한다).
//     여기에 값을 채우면 앱이 멀쩡히 쓰는 기기에 대고 「곧 끊깁니다」라고 말한다.
func TestSessionListShowsCurrentAndDeadline(t *testing.T) {
	h, _, _ := testHandlerWithSessions(t)
	phone := loginAt(t, h, sessionBase, "NatureApp/1.4.0 (Linux; Android 14) Chrome/140")
	laptop := loginAt(t, h, sessionBase.Add(time.Minute), "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/140.0.0.0 Safari/537.36")
	_, _, phoneSid, _ := h.tokens.ParseRefresh(phone.RefreshToken)
	_, _, laptopSid, _ := h.tokens.ParseRefresh(laptop.RefreshToken)

	at := sessionBase.Add(time.Hour)
	h.now = func() time.Time { return at }
	expectStatus(t, revokeSessionAs(t, h, "user-1", laptopSid, phoneSid), 200)

	items := decodeSessions(t, listSessionsAs(t, h, "user-1", laptopSid))
	if len(items) != 2 {
		t.Fatalf("세션 %d건", len(items))
	}
	byID := map[string]sessionDTO{}
	for _, s := range items {
		byID[s.SessionID] = s
	}
	cur := byID[laptopSid]
	if !cur.Current || cur.RevokedAt != nil || cur.AccessibleUntilAtMost != nil {
		t.Fatal("살아 있는 현재 기기의 표시가 틀렸다", cur)
	}
	if cur.DeviceLabel != "Chrome · macOS" || cur.UserAgent == "" {
		t.Fatal("기기 이름과 원문이 함께 나가야 한다", cur)
	}
	if len(cur.RecentAccesses) != 1 {
		t.Fatal("접속 기록이 없다", cur)
	}
	dead := byID[phoneSid]
	if dead.Current {
		t.Fatal("끊은 기기가 「지금 이 기기」로 표시됐다")
	}
	if dead.RevokedAt == nil || !dead.RevokedAt.Equal(at.UTC()) {
		t.Fatal("끊은 시각이 없다", dead)
	}
	if dead.AccessibleUntilAtMost == nil || !dead.AccessibleUntilAtMost.Equal(at.Add(AccessTokenTTL).UTC()) {
		t.Fatal("언제까지 쓸 수 있는지가 없다", dead)
	}

	// 한계 시각이 지나면 목록에서 빠진다. 아무것도 할 수 없는 줄이 목록만 채우기 때문이다.
	h.now = func() time.Time { return at.Add(AccessTokenTTL + time.Second) }
	items = decodeSessions(t, listSessionsAs(t, h, "user-1", laptopSid))
	if len(items) != 1 || items[0].SessionID != laptopSid {
		t.Fatal("지난 끊긴 세션이 목록에 남았다", items)
	}

	// 🔴 세션 장치 이전에 발급된 액세스 토큰은 세션 id 가 없다. 그때 **아무 줄도**
	// 「지금 이 기기」가 되면 안 된다 — 엉뚱한 줄이 표시되면 사용자는 그 줄만 남기고
	// 나머지를 끊는다.
	for _, s := range decodeSessions(t, listSessionsAs(t, h, "user-1", "")) {
		if s.Current {
			t.Fatal("세션 id 없는 토큰인데 「지금 이 기기」가 표시됐다")
		}
	}
}

// TestPasswordChangeHidesOldSessions 는 **비밀번호 변경이 여전히 전부 끊는다**는 것과,
// 그때 목록이 거짓말하지 않는다는 것을 함께 본다.
//
// 🔴 비밀번호를 바꾸면 계정의 TokenVersion 이 올라 옛 리프레시가 전부 막힌다. 그런데
// 세션 문서에는 아무 일도 일어나지 않으므로, 세션에 버전을 함께 들고 다니지 않으면
// **이미 죽은 기기가 목록에 「살아 있음」으로 남는다.** 보안 화면이 거짓말을 하는 셈이다.
func TestPasswordChangeHidesOldSessions(t *testing.T) {
	h, s, _ := testHandlerWithSessions(t)
	set := loginAt(t, h, sessionBase, "NatureApp/1.4.0 (Linux; Android 14) Chrome/140")
	if len(decodeSessions(t, listSessionsAs(t, h, "user-1", ""))) != 1 {
		t.Fatal("로그인 직후 세션이 목록에 없다")
	}
	w := request(t, h.changePassword, map[string]string{"currentPassword": "current-password", "newPassword": "새로운비밀번호입니다"}, "user-1")
	expectStatus(t, w, 204)
	if s.account.TokenVersion != 1 {
		t.Fatal("비밀번호 변경이 토큰 버전을 올리지 않았다")
	}
	// 🔴 옛 리프레시는 401 이다. 세션이 살아 있어도 버전 대조가 먼저 막는다.
	expectStatus(t, refreshAt(t, h, sessionBase.Add(time.Minute), "", set.RefreshToken), 401)
	// 그리고 그 세션은 목록에서 사라진다 — 끊을 것이 남아 있지 않기 때문이다.
	if items := decodeSessions(t, listSessionsAs(t, h, "user-1", "")); len(items) != 0 {
		t.Fatal("비밀번호를 바꿨는데 죽은 세션이 목록에 남았다", items)
	}
}

// TestDeviceLabelIsHonest 는 **모르면 모른다고 적는다**는 것을 못 박는다.
//
// 🔴 여기에 그럴듯한 기본값을 넣고 싶어지는 자리다. 그 친절의 대가는 사용자가 목록에서
// 엉뚱한 줄을 끊고, 잃어버린 기기는 살려 둔 채 안심하는 것이다.
//
// 크로뮴 계열 순서도 함께 본다 — 엣지·웨일·삼성인터넷은 전부 UA 에 Chrome 을 달고
// 다니고, iOS 크롬(CriOS)은 Safari 까지 달고 다닌다.
func TestDeviceLabelIsHonest(t *testing.T) {
	cases := []struct{ ua, want string }{
		{"", unknownDevice},
		{"curl/8.7.1", unknownDevice},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36", "Chrome · macOS"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0", "Edge · Windows"},
		{"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/25.0 Chrome/121 Mobile Safari/537.36", "삼성 인터넷 · 안드로이드"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/140.0 Mobile/15E148 Safari/604.1", "Chrome · 아이폰"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1", "Safari · 아이폰"},
		{"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/140 Whale/4.0.0.0 Safari/537.36", "Whale · Windows"},
		// 네이티브 껍데기는 UA 끝에 표식을 붙인다(nature-app 의 web-shell.tsx).
		{"Mozilla/5.0 (Linux; Android 14; SM-S911N) AppleWebKit/537.36 Chrome/140 Mobile Safari/537.36 NatureApp/1.4.0", "네이처 앱 1.4.0 · 안드로이드"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 NatureApp/2.0.1", "네이처 앱 2.0.1 · 아이폰"},
		// 표식은 있는데 버전이 이상하면 버전만 버린다. 껍데기라는 사실이 더 중요하다.
		{"NatureApp/dev (Linux; Android 14)", "네이처 앱 · 안드로이드"},
		// OS 만 알 수 있으면 OS 만 적는다. 🔴 브라우저를 지어내지 않는다.
		{"SomeAgent (Windows NT 10.0)", "Windows"},
	}
	for _, tc := range cases {
		if got := DeviceLabel(tc.ua); got != tc.want {
			t.Errorf("DeviceLabel(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

// TestSessionRoutesRegistered 는 **배선**을 못 박는다.
//
// 위 테스트들은 핸들러를 직접 불러 컨텍스트에 값을 심고 도는데, 그러면 Register 가 경로를
// 틀리게 걸거나 미들웨어를 안 거쳐도 전부 통과한다. 여기서만 실제 mux 와 Bearer 헤더를
// 지나므로 경로 오타·미들웨어 누락·PathValue 이름 불일치가 여기서 걸린다.
//
// 🔴 미들웨어가 액세스 토큰의 세션 id 를 컨텍스트로 옮기지 않으면 current 가 늘 거짓이다.
// 그 사고는 이 테스트에서만 드러난다.
func TestSessionRoutesRegistered(t *testing.T) {
	h, accounts, sessions := testHandlerWithSessions(t)
	set := loginAt(t, h, sessionBase, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/140.0.0.0 Safari/537.36")
	_, _, sid, _ := h.tokens.ParseRefresh(set.RefreshToken)
	h.now = func() time.Time { return sessionBase.Add(time.Minute) }

	mux := http.NewServeMux()
	Register(mux, accounts, sessions, h.tokens)
	serve := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+set.AccessToken)
		w := httptest.NewRecorder()
		h.tokens.Middleware(mux).ServeHTTP(w, r)
		return w
	}
	w := serve("GET", "/auth/sessions")
	expectStatus(t, w, 200)
	items := decodeSessions(t, w)
	if len(items) != 1 || !items[0].Current {
		t.Fatal("미들웨어가 세션 id 를 넘기지 않았다", w.Body.String())
	}
	expectStatus(t, serve("DELETE", "/auth/sessions/"+sid), 200)
	expectStatus(t, serve("DELETE", "/auth/sessions/없는세션"), 404)

	// 인증 없이 부르면 401 이다. 라우트가 미들웨어 밖으로 옮겨져도 무방비로 열리지 않는다.
	for _, tc := range []struct{ method, path string }{{"GET", "/auth/sessions"}, {"DELETE", "/auth/sessions/" + sid}} {
		w := httptest.NewRecorder()
		h.tokens.Middleware(mux).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		expectStatus(t, w, 401)
	}
}
