// Package auth 는 **모바일 앱(jayeon-app)이 쓰는 사용자 인증**이다. 어드민 인증은
// internal/admin 에 따로 있고 토큰도 시크릿도 공유하지 않는다 — 같은 시크릿을 쓰면
// 어떻게 되는지는 internal/admin/token.go 의 ErrSecretReused 에 적혀 있다.
//
// 이 파일이 세 엔드포인트를 연다.
//
//   - POST /auth/login            이메일+비밀번호 → 액세스·리프레시 토큰
//   - POST /auth/refresh          리프레시 토큰 → 새 토큰 한 쌍
//   - POST /auth/change-password  현재 비밀번호를 확인하고 교체(204)
//
// 🔴 **요청·응답의 필드명과 에러 코드는 앱과 합의된 계약이다**(docs/openapi.yaml).
// `accessToken` 을 `access_token` 으로 눕히는 식의 정리는 서버만 배포해도 이미 깔린
// 앱이 그 자리에서 로그인 불가가 되고, 스토어 심사를 기다리는 동안 되돌릴 수도 없다.
// 이름을 바꿔야 한다면 앱이 두 이름을 모두 읽는 버전을 먼저 내보낸 뒤에 한다.
package auth

import (
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/sslim7/jayeon-was/internal/credentials"
	"github.com/sslim7/jayeon-was/internal/httpx"
)

const (
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
	CodeAccountDisabled    = "ACCOUNT_DISABLED"
	// 예약된 코드다. 시도 제한을 붙이는 사람이 이 코드로 429를 내고 details에
	// 남은 초를 담는다. 앱은 이미 이 코드를 기다리고 있다.
	//
	// 앱이 모르는 코드로 429 가 나가면 화면에는 「알 수 없는 오류」가 뜨고, 사용자는
	// 비밀번호가 틀린 줄 알고 더 두드려서 더 잠긴다. 그래서 코드를 미리 정해 둔다.
	CodeTooManyAttempts = "TOO_MANY_ATTEMPTS"
	MinPasswordRunes    = 8
	// 계정 존재 여부가 응답으로 새지 않도록 실패 문구를 하나로 유지한다.
	msgLoginFailed = "이메일 또는 비밀번호가 맞지 않아요"
)

type Handler struct {
	store  AccountStore
	tokens *TokenIssuer
	// verifyPassword 는 credentials.VerifyPassword 그대로지만 **필드로 주입한다.**
	// 테스트가 호출 횟수를 세야 하기 때문이다 — 계정이 없을 때도 비교가 한 번 돌았는지는
	// 응답만 봐서는 알 수 없고, 그 한 번이 빠지는 것이 login 의 미끼 비교가 되돌려지는
	// 방식이다(handler_test.go 의 `calls != 1`).
	verifyPassword func(string, string) bool
	now            func() time.Time
}

type TokenSet struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresInSec int    `json:"expiresInSec"`
}

// Register 는 세 라우트를 mux 에 건다.
//
// change-password 만 인증이 필요한데 그 검사는 여기가 아니라 TokenIssuer.Middleware
// 가 한다. 핸들러도 컨텍스트의 userId 가 비었는지를 다시 보는데, 미들웨어 배선이
// 빠지거나 라우트가 미들웨어 밖으로 옮겨져도 무방비로 열리지는 않게 하려는 것이다.
func Register(mux *http.ServeMux, store AccountStore, tokens *TokenIssuer) {
	h := &Handler{store: store, tokens: tokens, verifyPassword: credentials.VerifyPassword, now: time.Now}
	mux.HandleFunc("POST /auth/login", h.login)
	mux.HandleFunc("POST /auth/refresh", h.refresh)
	mux.HandleFunc("POST /auth/change-password", h.changePassword)
}

// 실패 응답을 함수로 좁혀 둔다. 같은 뜻의 실패가 여러 자리에서 나가는데 문구나 코드가
// 한 군데라도 갈리면, login 이 공들여 감춘 계정 존재 여부가 그 자리로 샌다.
func unauthorized(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "로그인이 필요해요")
}
func invalidCredentials(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusUnauthorized, CodeInvalidCredentials, msgLoginFailed)
}
func disabled(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusForbidden, CodeAccountDisabled, "사용할 수 없는 계정이에요")
}
func internalError(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "서버 오류가 생겼어요")
}
func validation(w http.ResponseWriter, reason string) {
	httpx.WriteErrorDetails(w, http.StatusBadRequest, httpx.CodeValidationFailed, "요청 값이 올바르지 않아요", map[string]any{"reason": reason})
}

// issue 는 액세스·리프레시 토큰 한 쌍을 발급한다.
//
// **TokenVersion 은 리프레시에만 싣는다.** 액세스에 실으면 매 요청마다 사용자 문서를
// 한 번 더 읽어 대조해야 해서 Firestore 읽기가 요청 수만큼 늘어난다. 그 대가로 비밀번호를
// 바꿔도 이미 나간 액세스 토큰이 **최대 AccessTokenTTL(1시간) 더 통한다** — 즉 "즉시
// 로그아웃" 이 아니라 "최대 한 시간 안에 로그아웃" 이고, 비밀번호 변경 화면의 안내
// 문구가 그 사실과 맞아야 한다. (자세한 것은 token.go 의 Claims.Ver 주석)
func (h *Handler) issue(a Account) (TokenSet, error) {
	access, err := h.tokens.IssueAccess(a.UserID)
	if err != nil {
		return TokenSet{}, err
	}
	refresh, err := h.tokens.IssueRefresh(a.UserID, a.TokenVersion)
	return TokenSet{AccessToken: access, RefreshToken: refresh, ExpiresInSec: int(AccessTokenTTL / time.Second)}, err
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	req.Email = credentials.NormalizeEmail(req.Email)
	if req.Email == "" || req.Password == "" {
		validation(w, "이메일과 비밀번호를 입력해 주세요")
		return
	}
	a, err := h.store.FindByEmail(r.Context(), req.Email)
	if err != nil && !errors.Is(err, ErrAccountNotFound) {
		internalError(w)
		return
	}
	// 🔴 **계정이 없을 때도 비밀번호 비교를 반드시 한 번 돌린다.** 여기서 곧장 401 을
	// 돌려주면 bcrypt 한 번(수십 ms)이 통째로 빠져 응답이 눈에 띄게 빨라지고, 그 시간
	// 차이만으로 로그인 API 하나에 대고 "이 이메일이 가입돼 있는가" 를 훑어낼 수 있다.
	// 가입자 이메일 목록은 표적 피싱의 출발점이라 시간으로도 흘리지 않는다.
	//
	// 계정이 없으면 a 는 제로값이라 PasswordHash 가 비어 있고, VerifyPassword 가 빈
	// 해시를 미끼 해시로 바꿔 같은 비용을 치른다(internal/credentials). 즉 이 한 줄은
	// **없는 계정에 대해 일부러 하는 낭비**다. "없으면 빨리 반환" 으로 고치지 마라 —
	// 그 낭비가 방어다.
	matched := h.verifyPassword(a.PasswordHash, req.Password)
	// 🔴 계정 없음과 비밀번호 틀림이 **한 분기인 이유는 두 응답이 글자 하나까지 같아야
	// 하기 때문이다.** 갈라 놓으면 바로 위의 미끼 비교도 아래의 IsActive 순서도 통째로
	// 무의미해진다 — 시간을 잴 것도 없이 응답 본문이 가입 여부를 알려 주기 때문이다.
	// 「등록되지 않은 이메일이에요」 라고 친절하게 알려 주고 싶어지는 자리이고, 그 친절의
	// 대가가 가입자 이메일 목록이다. (internal/admin/handler.go 의 login 도 같은 판단이다)
	if errors.Is(err, ErrAccountNotFound) || !matched {
		invalidCredentials(w)
		return
	}
	// 비활성 판정이 비밀번호 확인 **뒤에** 있다. 앞에 두면 비밀번호를 모르는 사람도
	// 403 을 받아 "그 계정은 있고 잠겨 있다" 를 알게 된다 — 위에서 막은 누출을 403 으로
	// 다시 여는 셈이다. 비밀번호가 맞을 때에야 비로소 잠겼다고 알려 준다.
	if !a.IsActive {
		disabled(w)
		return
	}
	set, err := h.issue(a)
	if err != nil {
		internalError(w)
		return
	}
	// 토큰이 앞단(호스팅·CDN) 캐시에 남으면 남의 요청에 실려 나갈 수 있다. 오류 응답에는
	// httpx 가 알아서 붙이지만(httpx.go 의 noStore) 성공 응답은 여기서 붙여야 한다.
	// refresh 와 changePassword 도 같은 이유로 붙인다.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, struct {
		TokenSet
		MustChangePassword bool `json:"mustChangePassword"`
	}{set, a.MustChangePassword})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	id, ver, err := h.tokens.ParseRefresh(req.RefreshToken)
	if err != nil {
		unauthorized(w)
		return
	}
	a, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		unauthorized(w)
		return
	}
	if err != nil {
		internalError(w)
		return
	}
	// 🔴 **비밀번호 변경이 옛 세션을 끊는 지점은 이 한 줄뿐이다.** 리프레시 토큰은 서버에
	// 저장되지 않는 순수 JWT 라 이 대조 말고는 무효화할 방법이 없다. 빼면 SetPassword 의
	// 버전 증가가 통째로 무의미해지고 옛 토큰이 TTL(90일) 동안 계속 돈다.
	//
	// 그런데 **빼도 아무 에러가 나지 않고 로그도 남지 않는다.** 로그인도 리프레시도 정상
	// 동작하는 것처럼 보이고, 비밀번호를 털린 사람이 비밀번호를 바꿔도 공격자의 세션이
	// 살아 있다는 사실만 조용히 남는다. 테스트가 아니면 드러나지 않는 종류의 사고라
	// handler_test.go 가 이 자리를 못 박고 있다.
	if a.TokenVersion != ver {
		unauthorized(w)
		return
	}
	if !a.IsActive {
		disabled(w)
		return
	}
	set, err := h.issue(a)
	if err != nil {
		internalError(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, set)
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	id := UserID(r.Context())
	if id == "" {
		unauthorized(w)
		return
	}
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	a, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		unauthorized(w)
		return
	}
	if err != nil {
		internalError(w)
		return
	}
	// 토큰만으로는 부족하다. 남의 휴대폰을 잠깐 집어 든 사람이 비밀번호를 바꿔 계정을
	// 가져가는 것을 막는 것이 현재 비밀번호 확인의 전부다.
	if !h.verifyPassword(a.PasswordHash, req.CurrentPassword) {
		invalidCredentials(w)
		return
	}
	if !a.IsActive {
		disabled(w)
		return
	}
	// 길이 검사가 룬과 바이트로 갈려 있다. 최소는 사람이 세는 글자 수라 룬이고, 최대는
	// bcrypt 의 한계라 바이트다 — 한글은 한 자가 3바이트여서 **25자면 이미 72바이트를
	// 넘는다.** 여기서 막지 않으면 credentials.HashPassword 가 에러를 내고 사용자는
	// 이유를 알 수 없는 500 을 받는다.
	if utf8.RuneCountInString(req.NewPassword) < MinPasswordRunes {
		validation(w, "새 비밀번호는 8자 이상이어야 해요")
		return
	}
	// 같은 비밀번호로 "바꾸면" 초대 때 받은 임시 비밀번호가 그대로인 채 mustChangePassword
	// 만 거짓이 된다 — 바꾸라고 세워 둔 화면을 통과했는데 비밀번호는 안 바뀐 상태다.
	if req.CurrentPassword == req.NewPassword {
		validation(w, "현재 비밀번호와 다른 비밀번호를 입력해 주세요")
		return
	}
	if len(req.NewPassword) > 72 {
		validation(w, "새 비밀번호는 72바이트 이하여야 해요")
		return
	}
	hash, err := credentials.HashPassword(req.NewPassword)
	if err != nil {
		internalError(w)
		return
	}
	// 토큰을 발급받은 뒤 계정이 지워진 경우다. 404 가 아니라 401 을 준다 — 앱이 401 만
	// 세션 만료로 처리한다(users.go 의 me 와 같은 판단).
	if err = h.store.SetPassword(r.Context(), id, hash, h.now()); errors.Is(err, ErrAccountNotFound) {
		unauthorized(w)
		return
	} else if err != nil {
		internalError(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
