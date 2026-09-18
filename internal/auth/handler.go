// Package auth 는 **모바일 앱(nature-app)이 쓰는 사용자 인증**이다. 어드민 인증은
// internal/admin 에 따로 있고 토큰도 시크릿도 공유하지 않는다 — 같은 시크릿을 쓰면
// 어떻게 되는지는 internal/admin/token.go 의 ErrSecretReused 에 적혀 있다.
//
// 이 파일이 세 엔드포인트를 연다.
//
//   - POST /auth/login            이메일+비밀번호 → 액세스·리프레시 토큰
//   - POST /auth/refresh          리프레시 토큰 → 새 토큰 한 쌍
//   - POST /auth/change-password  현재 비밀번호를 확인하고 교체(204)
//
// 기기 단위 세션(목록·끊기)은 session_handler.go 가 열고, 그 규칙은 session.go 에 있다.
//
// 🔴 **요청·응답의 필드명과 에러 코드는 앱과 합의된 계약이다**(docs/openapi.yaml).
// `accessToken` 을 `access_token` 으로 눕히는 식의 정리는 서버만 배포해도 이미 깔린
// 앱이 그 자리에서 로그인 불가가 되고, 스토어 심사를 기다리는 동안 되돌릴 수도 없다.
// 이름을 바꿔야 한다면 앱이 두 이름을 모두 읽는 버전을 먼저 내보낸 뒤에 한다.
package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/sslim7/nature-was/internal/credentials"
	"github.com/sslim7/nature-was/internal/httpx"
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
	store AccountStore
	// sessions 는 기기 단위 세션 저장소다. 🔴 **nil 이면 로그인이 500 이다** —
	// 세션 없이 발급한 토큰은 첫 갱신에서 거절되므로(session.go 의 SessionStore 주석),
	// 배선을 빼먹은 채로 도는 것보다 즉시 드러나는 편이 낫다.
	sessions SessionStore
	tokens   *TokenIssuer
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

// Register 는 다섯 라우트를 mux 에 건다.
//
// login·refresh 를 뺀 셋은 인증이 필요한데 그 검사는 여기가 아니라
// TokenIssuer.Middleware 가 한다. 핸들러도 컨텍스트의 userId 가 비었는지를 다시 보는데,
// 미들웨어 배선이 빠지거나 라우트가 미들웨어 밖으로 옮겨져도 무방비로 열리지는 않게
// 하려는 것이다.
//
// 🔴 **세션 라우트를 userguard 로 감싸지 않는다.** 이 저장소의 관례대로 인증 핸들러는
// 그 가드 밖에 있고(internal/userguard 의 AccountReader 주석), 그것이 여기서는 특히
// 중요하다 — 임시 비밀번호를 아직 안 바꾼 계정이나 방금 비활성된 계정이라도 **잃어버린
// 기기를 끊는 것만은 할 수 있어야 한다.** 이 기능이 존재하는 이유가 그 상황이다.
func Register(mux *http.ServeMux, store AccountStore, sessions SessionStore, tokens *TokenIssuer) {
	h := &Handler{store: store, sessions: sessions, tokens: tokens, verifyPassword: credentials.VerifyPassword, now: time.Now}
	mux.HandleFunc("POST /auth/login", h.login)
	mux.HandleFunc("POST /auth/refresh", h.refresh)
	mux.HandleFunc("POST /auth/change-password", h.changePassword)
	mux.HandleFunc("GET /auth/sessions", h.listSessions)
	mux.HandleFunc("DELETE /auth/sessions/{sessionId}", h.revokeSession)
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
// 바꿔도 이미 나간 액세스 토큰이 **최대 AccessTokenTTL(15분) 더 통한다** — 즉 "즉시
// 로그아웃" 이 아니라 "최대 15분 안에 로그아웃" 이고, 비밀번호 변경 화면의 안내
// 문구가 그 사실과 맞아야 한다. (자세한 것은 token.go 의 Claims.Ver 주석)
//
// 세션 id 는 **두 토큰 모두에** 실린다. 리프레시 쪽은 갱신 때 세션이 살아 있는지
// 대조하는 데 쓰고, 액세스 쪽은 세션 목록의 「지금 이 기기」 표시에만 쓴다 — 쓰임이
// 다른 이유는 token.go 의 Claims.Sid 주석에 있다.
//
// 🔴 sid 가 비면 발급하지 않는다. 빈 sid 로 나간 리프레시 토큰은 첫 갱신에서 401 이
// 되는데, 그때는 이미 「로그인 성공」 화면을 지나온 뒤라 사용자에게는 원인 없는 고장으로
// 보인다. 여기서 막으면 로그인이 즉시 500 으로 실패하고 앱은 「다시 시도」를 보여 준다.
func (h *Handler) issue(a Account, sid string) (TokenSet, error) {
	if sid == "" {
		return TokenSet{}, errors.New("auth: 세션 없이 토큰을 발급할 수 없다")
	}
	access, err := h.tokens.IssueAccess(a.UserID, sid)
	if err != nil {
		return TokenSet{}, err
	}
	refresh, err := h.tokens.IssueRefresh(a.UserID, a.TokenVersion, sid)
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
	// 🔴 **세션을 먼저 만들고, 실패하면 로그인을 실패시킨다.**
	//
	// 이 저장소에는 「설정 하나가 없어도 그 기능만 끄고 기동은 계속한다」는 관례가 있다
	// (internal/admin 의 ADMIN_JWT_SECRET, internal/calls 의 CALL_AUDIO_BUCKET). 세션은
	// 그 관례를 따를 수 없다. 이유는 둘이다.
	//
	//  1. **끌 설정이 없다.** 세션에 필요한 것은 이미 필수인 Firestore 클라이언트뿐이라
	//     "설정이 빠진 상태" 자체가 존재하지 않는다. 위 두 기능은 부가 기능이고 없으면
	//     라우트만 닫히지만, 세션은 갱신 경로에 들어가 있어 「없는 상태」가 성립하지 않는다.
	//  2. **세션 없이 통과시키면 조용히 깨진다.** 세션 없는 토큰은 첫 갱신에서 401 이다.
	//     로그인은 성공한 것처럼 보이고 15분 뒤 로그인 화면으로 돌아가는데, 사용자에게는
	//     원인도 안내도 없다. 그렇다고 갱신 쪽에서 세션 없는 토큰을 받아 주면 그 갈래가
	//     **세션 검사를 통째로 우회하는 구멍**으로 남는다 — 끊긴 기기가 그 길로 들어온다.
	//
	// 그래서 500 이다. 앱은 500 을 「다시 시도해 주세요」로 보여 주고 사용자는 그 자리에서
	// 다시 누를 수 있다. 덧붙여 여기까지 온 요청은 이미 Firestore 읽기(FindByEmail)를
	// 성공한 뒤이므로, 이 쓰기가 실패하는 상황은 앱의 다른 화면도 전부 안 되는 상황이다 —
	// 로그인만 통과시켜 얻을 것이 없다.
	now := h.now()
	label := DeviceLabel(r.UserAgent())
	sid, err := h.sessions.CreateSession(r.Context(), a.UserID, Session{
		DeviceLabel:  label,
		UserAgent:    truncateUserAgent(r.UserAgent()),
		CreatedAt:    now,
		LastSeenAt:   now,
		TokenVersion: a.TokenVersion,
		Accesses:     []Access{{At: now, DeviceLabel: label}},
	})
	if err != nil {
		// 🔴 조용히 넘어가지 않는다. 이 로그가 없으면 "가끔 로그인이 안 된다" 는 제보만
		// 남고 원인은 어디에도 드러나지 않는다.
		log.Printf("auth: 세션 생성 실패 — 로그인을 거절한다 (user=%s): %v", a.UserID, err)
		internalError(w)
		return
	}
	set, err := h.issue(a, sid)
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
	id, ver, sid, err := h.tokens.ParseRefresh(req.RefreshToken)
	if err != nil {
		unauthorized(w)
		return
	}
	// 🔴 **세션 id 가 없는 리프레시 토큰은 거절한다.** 세션 장치 이전에 발급된 토큰이다.
	//
	// 받아 주는 갈래를 만들지 않는 이유: 그 갈래가 곧 **세션 검사를 우회하는 길**이다.
	// 끊긴 기기가 옛 토큰을 들고 있으면 거기로 들어오고, 이 기능의 목적이 정면으로
	// 무너진다. 마이그레이션 기간이나 플래그를 두면 그 기간이 끝나는 날을 아무도
	// 기억하지 못한다는 것도 같이 본 판단이다.
	//
	// 대가는 배포 직후 모든 사람이 한 번 다시 로그인하는 것뿐이다. 응답은 기존 인증
	// 실패와 **같은 모양의 401** 이라 앱은 이미 이것을 세션 만료로 처리해 로그인 화면으로
	// 보낸다(nature-app 의 lib/api.ts). ⚠️ 조용히 실패하지 않도록 이유는 로그에 남긴다.
	if sid == "" {
		log.Printf("auth: 세션 id 없는 리프레시 토큰을 거절했다 — 다시 로그인해야 한다 (user=%s)", id)
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
	// 🔴 **「이 기기만 끊기」가 실제로 듣는 지점이 여기다.** 위의 TokenVersion 대조가
	// 계정 전체를 끊는 길이라면, 이 아래가 기기 하나를 끊는 길이다. 둘은 겹치지 않으므로
	// 한쪽을 다른 쪽으로 대신할 수 없다.
	//
	// 그리고 빼도 **아무 에러가 나지 않는다.** 로그인도 갱신도 멀쩡히 돌고, 끊은 기기가
	// 계속 쓰이고 있다는 사실만 조용히 남는다. 그래서 테스트가 이 자리를 못 박고 있다
	// (session_test.go 의 끊긴 세션 갱신 거부).
	sess, err := h.sessions.GetSession(r.Context(), id, sid)
	if errors.Is(err, ErrSessionNotFound) {
		// 세션 문서가 없다. 끊고 나서 문서를 지웠거나, 만들다 만 세션이다.
		// ⚠️ 여기서 "없으면 만들어 준다" 로 너그럽게 굴면 **끊기가 통째로 풀린다** —
		// 끊긴 세션을 정리하려고 문서를 지우는 순간 모든 옛 토큰이 되살아난다.
		log.Printf("auth: 없는 세션의 갱신을 거절했다 (user=%s session=%s)", id, sid)
		unauthorized(w)
		return
	}
	if err != nil {
		internalError(w)
		return
	}
	if !sess.RevokedAt.IsZero() {
		log.Printf("auth: 끊긴 세션의 갱신을 거절했다 (user=%s session=%s revokedAt=%s)", id, sid, sess.RevokedAt.UTC().Format(time.RFC3339))
		unauthorized(w)
		return
	}
	// 세션 id 는 **그대로 물려준다.** 갱신할 때마다 새 세션을 만들면 목록이 갱신 횟수만큼
	// 늘어나고, 사용자는 자기 기기가 어느 줄인지 알 수 없게 된다.
	set, err := h.issue(a, sid)
	if err != nil {
		internalError(w)
		return
	}
	// 흔적 남기기는 토큰을 발급한 **뒤에** 한다. 여기서 실패해도 갱신을 되돌리지 않는다 —
	// 이미 토큰이 나갔으므로 500 을 주면 앱은 로그아웃하는데 발급된 토큰은 살아 있는,
	// 앞뒤가 안 맞는 상태가 된다. 기록이 한 번 빠지는 쪽이 낫다.
	h.touch(r.Context(), id, sid, sess, DeviceLabel(r.UserAgent()))
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, set)
}

// touch 는 갱신 흔적을 세션에 남긴다. **쓸 만한 이유가 있을 때만 쓴다**(shouldTouch).
//
// 🔴 매 갱신마다 쓰면 기기 하나가 하루 최대 96번(액세스 토큰 15분) 문서를 통째로 다시
// 쓴다. 쌓이는 정보는 거의 없는데 쓰기만 나가고, 접속 기록 10건은 전부 「방금 전」이
// 되어 아무것도 알려 주지 못한다. 자세한 판단 기준은 session.go 의 shouldTouch 다.
func (h *Handler) touch(ctx context.Context, userID, sessionID string, prev Session, label string) {
	now := h.now()
	if !shouldTouch(prev, now, label) {
		return
	}
	if err := h.sessions.TouchSession(ctx, userID, sessionID, now, label, pushAccess(prev.Accesses, Access{At: now, DeviceLabel: label})); err != nil {
		// 갱신 자체는 성공했다. 기록만 빠진 것이라 사용자에게 알릴 것이 없고,
		// 반복되면 로그로 드러난다.
		log.Printf("auth: 세션 접속 기록 실패(무시) (user=%s session=%s): %v", userID, sessionID, err)
	}
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
