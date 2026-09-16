package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sslim7/jayeon-was/internal/credentials"
)

// fakeAccounts 는 AccountStore 의 인메모리 구현이다.
//
// auth 가 Firestore 가 아니라 **인터페이스**에 기대는 덕에(account.go 의 AccountStore)
// 이 패키지의 인증 규칙 전부를 에뮬레이터 없이 돌릴 수 있다. 에뮬레이터가 필요한
// 테스트는 FIRESTORE_EMULATOR_HOST 가 없으면 skip 되는데, 아래 테스트들이 못 박는 것은
// 조용히 건너뛰면 안 되는 보안 속성이라 페이크로 둔다.
//
// SetPassword 는 실제 Store 와 같은 효과를 낸다 — 해시 교체, TokenVersion 증가,
// MustChangePassword 해제가 **한 번에** 일어난다(internal/users/store.go 참고).
type fakeAccounts struct {
	account Account
	err     error
	writes  int
	email   string
}

func (s *fakeAccounts) FindByEmail(_ context.Context, email string) (Account, error) {
	s.email = email
	return s.account, s.err
}

func (s *fakeAccounts) Get(context.Context, string) (Account, error) { return s.account, s.err }

func (s *fakeAccounts) SetPassword(_ context.Context, _ string, hash string, _ time.Time) error {
	s.writes++
	s.account.PasswordHash = hash
	s.account.TokenVersion++
	s.account.MustChangePassword = false
	return s.err
}

func testHandler(t *testing.T) (*Handler, *fakeAccounts) {
	t.Helper()
	hash, err := credentials.HashPassword("current-password")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeAccounts{account: Account{UserID: "user-1", Email: "user@example.com", PasswordHash: hash, IsActive: true, MustChangePassword: true}}
	return &Handler{store: s, tokens: NewTokenIssuer("test-secret"), verifyPassword: credentials.VerifyPassword, now: time.Now}, s
}

func request(t *testing.T, h http.HandlerFunc, body any, id string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(b)))
	// id 가 있으면 미들웨어를 통과한 요청처럼 컨텍스트에 userId 를 심는다.
	if id != "" {
		r = r.WithContext(WithUserID(r.Context(), id))
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
}

// TestLoginEnumerationAndDisabled 는 **로그인이 계정 존재 여부를 흘리지 않는다**는 것을
// 못 박는다. 세 가지를 본다.
//
//  1. 이메일이 정규화되어 조회로 넘어간다 — 저장과 조회의 정규화가 어긋나면 대문자로
//     만든 계정에 소문자로 로그인이 안 되는데, 콘솔에서는 문서가 멀쩡해 보인다.
//  2. 🔴 `calls != 1` — 계정이 없을 때도 비밀번호 비교가 **정확히 한 번** 돌았는지다.
//     이 단언이 빠지면 누군가 "계정 없으면 빨리 반환" 최적화를 넣어도 아무도 모른다.
//     응답은 그대로고 테스트도 그대로 통과하는데 응답 시간만 수십 ms 빨라져서, 가입된
//     이메일을 밖에서 훑어낼 수 있게 된다. 그 최적화를 막는 것이 이 줄의 존재 이유다.
//     hash 가 빈 값인지도 함께 보는데, 미끼 해시 경로(credentials.VerifyPassword)를
//     실제로 지나간다는 뜻이다.
//  3. 🔴 상태코드뿐 아니라 **본문을 바이트로 비교한다.** 401 로 맞춰 놓고 문구만
//     「등록되지 않은 이메일이에요」 로 갈라 두면 시간을 잴 것도 없이 본문이 알려 준다.
//
// 마지막 두 줄은 비활성 계정 판정이 비밀번호 확인 **뒤에** 있다는 것이다. 비밀번호가
// 틀리면 비활성이어도 401(=없는 계정과 같은 응답)이고, 맞을 때에야 403 이 된다.
// 순서를 뒤집으면 비밀번호를 모르는 사람이 403 으로 계정의 존재를 알게 된다.
func TestLoginEnumerationAndDisabled(t *testing.T) {
	h, s := testHandler(t)
	wrong := request(t, h.login, map[string]string{"email": " USER@EXAMPLE.COM ", "password": "wrong"}, "")
	expectStatus(t, wrong, 401)
	if s.email != "user@example.com" {
		t.Fatal(s.email)
	}
	s.account = Account{}
	s.err = ErrAccountNotFound
	calls := 0
	h.verifyPassword = func(hash, plain string) bool {
		calls++
		if hash != "" {
			t.Fatal("미끼 해시 경로가 아니다")
		}
		return credentials.VerifyPassword(hash, plain)
	}
	absent := request(t, h.login, map[string]string{"email": "missing@example.com", "password": "wrong"}, "")
	if calls != 1 || absent.Code != wrong.Code || absent.Body.String() != wrong.Body.String() {
		t.Fatal("계정 열거 방어가 깨졌다")
	}
	h, s = testHandler(t)
	s.account.IsActive = false
	expectStatus(t, request(t, h.login, map[string]string{"email": "user@example.com", "password": "wrong"}, ""), 401)
	expectStatus(t, request(t, h.login, map[string]string{"email": "user@example.com", "password": "current-password"}, ""), 403)
}

// TestLoginAndRefreshContract 는 앱과 합의된 토큰 계약과 **리프레시 무효화**를 못 박는다.
//
//   - 로그인 응답은 mustChangePassword 를 싣고 expiresInSec 은 AccessTokenTTL(3600)이다.
//     앱이 이 값으로 재발급 시점을 잡으므로 숫자가 바뀌면 앱이 먼저 깨진다.
//   - 발급된 리프레시 토큰에 그 시점의 TokenVersion 이 실려 있다.
//   - 리프레시 응답에는 mustChangePassword 가 **없다.** 앱이 리프레시 응답으로 그 상태를
//     덮어쓰면 비밀번호 변경 화면이 사라지거나 다시 뜬다.
//   - 매번 새 리프레시 토큰을 준다(회전).
//   - 🔴 TokenVersion 이 오른 뒤의 옛 리프레시는 401 이다. **이번 작업의 핵심 속성**이고,
//     비밀번호 변경이 옛 세션을 끊는 유일한 장치가 여기서 검증된다. handler.refresh 의
//     대조 한 줄을 지워도 다른 테스트는 전부 통과한다 — 이 줄만이 그것을 붙잡는다.
//   - 🔴 리프레시 자리에 액세스 토큰을 넣으면 401 이다. 두 토큰은 서명 키가 같아서
//     `token_use` 대조가 없으면 그대로 통과하고, 그러면 1시간짜리 액세스 토큰이 90일짜리
//     리프레시로 승격되는 셈이 된다(token.go 의 TokenUse).
//   - 비활성 계정은 403, 삭제된 계정은 401(세션 만료)이다.
func TestLoginAndRefreshContract(t *testing.T) {
	h, s := testHandler(t)
	s.account.TokenVersion = 4
	w := request(t, h.login, map[string]string{"email": "user@example.com", "password": "current-password"}, "")
	expectStatus(t, w, 200)
	var body struct {
		TokenSet
		MustChangePassword bool `json:"mustChangePassword"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.MustChangePassword || body.ExpiresInSec != 3600 || body.AccessToken == "" {
		t.Fatal(w.Body.String())
	}
	id, ver, err := h.tokens.ParseRefresh(body.RefreshToken)
	if err != nil || id != "user-1" || ver != 4 {
		t.Fatal(id, ver, err)
	}
	w = request(t, h.refresh, map[string]string{"refreshToken": body.RefreshToken}, "")
	expectStatus(t, w, 200)
	var renewed map[string]any
	json.Unmarshal(w.Body.Bytes(), &renewed)
	if _, ok := renewed["mustChangePassword"]; ok {
		t.Fatal("refresh 응답 계약 위반")
	}
	if renewed["refreshToken"] == body.RefreshToken {
		t.Fatal("새 리프레시를 발급하지 않았다")
	}
	s.account.TokenVersion++
	expectStatus(t, request(t, h.refresh, map[string]string{"refreshToken": body.RefreshToken}, ""), 401)
	expectStatus(t, request(t, h.refresh, map[string]string{"refreshToken": body.AccessToken}, ""), 401)
	s.account.TokenVersion = 4
	s.account.IsActive = false
	expectStatus(t, request(t, h.refresh, map[string]string{"refreshToken": body.RefreshToken}, ""), 403)
	s.err = ErrAccountNotFound
	expectStatus(t, request(t, h.refresh, map[string]string{"refreshToken": body.RefreshToken}, ""), 401)
}

// TestChangePassword 는 비밀번호 교체의 거절 조건과 성공 시의 효과를 못 박는다.
//
// 각 케이스가 막는 것:
//   - 미인증: 미들웨어를 지나지 않은 요청은 401. 핸들러가 스스로 한 번 더 보는 자리다.
//   - 틀린현재: 토큰만으로는 못 바꾼다. 남의 휴대폰을 잠깐 집어 든 사람이 계정을
//     가져가는 것을 막는다.
//   - 짧음: 최소 길이는 **룬**으로 센다.
//   - 같음: 같은 비밀번호로 "바꾸면" 임시 비밀번호가 그대로인 채 mustChangePassword 만
//     거짓이 되어, 바꾸라고 세워 둔 화면을 통과했는데 비밀번호는 안 바뀐 상태가 된다.
//   - 긴바이트: 최대 길이는 **바이트**로 센다. 한글 25자면 bcrypt 의 72바이트를 넘고,
//     여기서 막지 않으면 해싱이 에러를 내 사용자가 이유 모를 500 을 받는다.
//
// 실패한 케이스는 **쓰기가 한 번도 일어나지 않아야 한다**(`s.writes != 0`). 거절해 놓고
// TokenVersion 만 올려 두면 아무 이유 없이 모든 기기가 로그아웃된다.
//
// 성공 케이스는 한 번의 쓰기로 해시·TokenVersion·mustChangePassword 가 함께 바뀌고,
// 본문 없는 204 가 나가는 것까지 본다. 그 뒤 두 단언이 이 작업의 요점이다.
//   - 🔴 옛 리프레시 토큰은 401 이 된다 — 비밀번호를 바꾸면 옛 세션이 끊긴다.
//   - 🔴 **이미 나간 액세스 토큰은 여전히 유효하다.** 사고가 아니라 의도다. 액세스에는
//     TokenVersion 을 싣지 않으므로 최대 1시간 동안 통한다(token.go 의 Claims.Ver).
//     이 단언이 그 대가를 코드에 고정해 둔다 — 여기가 실패하면 액세스 토큰까지 매 요청
//     문서를 읽어 대조하고 있다는 뜻이다.
func TestChangePassword(t *testing.T) {
	cases := []struct {
		name, current, new, id string
		status                 int
	}{
		{"미인증", "current-password", "new-password", "", 401},
		{"틀린현재", "wrong", "new-password", "user-1", 401},
		{"짧음", "current-password", "short", "user-1", 400},
		{"같음", "current-password", "current-password", "user-1", 400},
		{"긴바이트", "current-password", strings.Repeat("가", 25), "user-1", 400},
		{"성공", "current-password", "새로운비밀번호입니다", "user-1", 204},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, s := testHandler(t)
			oldRefresh, _ := h.tokens.IssueRefresh("user-1", 0)
			access, _ := h.tokens.IssueAccess("user-1")
			w := request(t, h.changePassword, map[string]string{"currentPassword": tc.current, "newPassword": tc.new}, tc.id)
			expectStatus(t, w, tc.status)
			if tc.status != 204 {
				if s.writes != 0 {
					t.Fatal("실패 후 쓰기")
				}
				return
			}
			if s.writes != 1 || s.account.TokenVersion != 1 || s.account.MustChangePassword || !credentials.VerifyPassword(s.account.PasswordHash, tc.new) || w.Body.Len() != 0 {
				t.Fatal("비밀번호 갱신 실패")
			}
			expectStatus(t, request(t, h.refresh, map[string]string{"refreshToken": oldRefresh}, ""), 401)
			if _, err := h.tokens.Parse(access, TokenUseAccess); err != nil {
				t.Fatal("기존 액세스 계약 위반", err)
			}
		})
	}
}

// TestStoreFailuresUseInternalCode 는 **저장소 에러가 밖으로 새지 않는다**는 것을 못 박는다.
//
// Firestore 에러 문구에는 프로젝트 ID·컬렉션 이름·인덱스 힌트 같은 내부 사정이 들어 있다.
// 그대로 응답에 실으면 공격자에게 구조를 알려 주는 꼴이고, 한 번 나간 문구는 회수할 수
// 없다. 세 엔드포인트 모두 500 + INTERNAL_ERROR 로 눕히고 원래 문구("private")는 본문
// 어디에도 없어야 한다.
func TestStoreFailuresUseInternalCode(t *testing.T) {
	h, s := testHandler(t)
	s.err = errors.New("private database detail")
	token, _ := h.tokens.IssueRefresh("user-1", 0)
	for _, tc := range []struct {
		handle http.HandlerFunc
		body   map[string]string
		id     string
	}{
		{h.login, map[string]string{"email": "a@b.com", "password": "password"}, ""},
		{h.refresh, map[string]string{"refreshToken": token}, ""},
		{h.changePassword, map[string]string{"currentPassword": "current-password", "newPassword": "next-password"}, "user-1"},
	} {
		w := request(t, tc.handle, tc.body, tc.id)
		expectStatus(t, w, 500)
		if !strings.Contains(w.Body.String(), "INTERNAL_ERROR") || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Body.String())
		}
	}
}

// TestRegisteredRoutesWithBearer 는 **배선**을 못 박는다.
//
// 위 테스트들은 핸들러를 직접 불러서 컨텍스트에 userId 를 심어 두고 도는데, 그러면
// Register 가 경로를 틀리게 걸거나 미들웨어를 안 거쳐도 전부 통과한다. 여기서만 실제
// mux 와 Bearer 헤더를 거쳐 들어가므로, 경로 오타나 미들웨어 누락이 이 테스트에서 걸린다.
func TestRegisteredRoutesWithBearer(t *testing.T) {
	h, s := testHandler(t)
	mux := http.NewServeMux()
	Register(mux, s, h.tokens)
	access, _ := h.tokens.IssueAccess("user-1")
	r := httptest.NewRequest("POST", "/auth/change-password", strings.NewReader(`{"currentPassword":"current-password","newPassword":"next-password"}`))
	r.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	h.tokens.Middleware(mux).ServeHTTP(w, r)
	expectStatus(t, w, 204)
}
