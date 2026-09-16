package admin

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenUse 는 어드민 토큰의 `tokenUse` 클레임 값이다.
//
// 일반 사용자 토큰(internal/auth)의 `token_use` 와 **키 이름부터 다르다** — 어드민 쪽
// 응답과 클레임은 전부 camelCase 이고, 두 토큰이 우연히 같은 모양이 되면 한쪽을
// 다른 쪽으로 착각해 파싱하는 코드가 언제든 생긴다.
const TokenUse = "admin_access"

// 유효 기간.
//
//   - 기본 12시간: 어드민은 근무 시간에 앉아서 쓰는 화면이라 하루를 넘길 이유가 없다.
//     길게 잡으면 공용 PC 에 남은 localStorage 하나가 그대로 열쇠가 된다.
//   - rememberMe 6주: 로그인 화면의 「로그인 상태 유지」 를 켠 경우다. 리프레시 토큰이
//     없는 구조라 유지 기간을 액세스 토큰 자체로 늘린다. 재발급 흐름을 나중에 붙이더라도
//     이 상수부터 줄여야 한다.
const (
	AccessTokenTTL     = 12 * time.Hour
	RememberMeTokenTTL = 6 * 7 * 24 * time.Hour
)

// ErrInvalidToken 은 서명 불일치·만료·종류 불일치를 모두 덮는다.
// 어느 쪽인지 호출자에게 알려 주지 않는다(internal/auth 와 같은 판단).
var ErrInvalidToken = errors.New("admin: 유효하지 않은 토큰")

// ErrSecretReused 는 ADMIN_JWT_SECRET 이 JWT_SECRET 과 같다는 뜻이다. 기동을 막는다.
//
// 🔴 **두 키가 같으면 어드민 토큰이 일반 사용자 미들웨어에서도 서명 검증을 통과한다.**
// internal/auth/middleware.go 는 통과한 토큰의 Subject 를 그대로 userId 로 컨텍스트에
// 넣는데, 어드민 토큰의 `sub` 는 문자열이 아니라 객체다. 그러면 auth.Claims 디코딩이
// 실패해 조용히 미인증으로 떨어지거나(운 좋은 경우), 클레임 모양이 조금만 달라지면
// 어드민 토큰 하나로 남의 사용자 API 를 호출할 수 있게 된다. 키를 가르는 것이 이 문제를
// 구조적으로 없애는 유일한 방법이라 경고가 아니라 기동 거부다.
var ErrSecretReused = errors.New("admin: ADMIN_JWT_SECRET 이 JWT_SECRET 과 같다")

// ErrSecretMissing 은 서명 키가 비어 있다는 뜻이다.
var ErrSecretMissing = errors.New("admin: ADMIN_JWT_SECRET 이 비어 있다")

// Subject 는 어드민 토큰의 `sub` 클레임이다. **문자열이 아니라 객체다.**
//
// SPA 가 이 구조를 그대로 읽어 사이드바와 라우트 가드를 그린다 — 서버 왕복 없이 화면을
// 그리기 위해서다. 그래서 필드 이름 하나만 바꿔도 어드민 화면이 통째로 비어 버리고,
// 컴파일도 되고 로그인도 되므로 아무 흔적이 남지 않는다.
//
// ⚠ **passwordHash 를 여기에 넣지 마라.** JWT 페이로드는 서명만 되어 있고 암호화되어
// 있지 않다 — base64 만 풀면 누구나 읽는다.
type Subject struct {
	AdminID string `json:"adminId"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"isAdmin"`
	// Permissions 는 메뉴 키 → 허용 여부다. 값이 false 인 키는 없는 키와 같다.
	// nil 이면 SPA 에서 `null` 로 읽혀 `Object.keys` 가 터지므로 발급 시 빈 맵으로 채운다.
	Permissions        map[string]bool `json:"permissions"`
	MustChangePassword bool            `json:"mustChangePassword"`
}

// Allows 는 이 어드민이 menu 키의 메뉴를 쓸 수 있는지다.
//
// **서버가 최종 판정자다.** 사이드바 숨김은 편의일 뿐이고, 권한 없는 호출은 여기서
// 403 이 된다.
//
// menu 가 빈 문자열이면 "로그인만 되어 있으면 된다" 는 뜻이다 — 비밀번호 변경처럼
// 메뉴에 속하지 않는 엔드포인트가 쓴다. `Permissions[""]` 를 조회하는 실수를 막으려고
// 여기서 명시적으로 갈라 둔다.
func (s Subject) Allows(menu string) bool {
	if s.IsAdmin {
		return true
	}
	if menu == "" {
		return true
	}
	return s.Permissions[menu]
}

// Claims 는 어드민 JWT 의 페이로드다.
//
// jwt.RegisteredClaims 를 묻어 쓰지 않는다 — 거기의 `Subject` 는 string 이라 위 Subject
// 객체를 담을 수 없고, 시간 클레임 이름도 우리가 필요한 것과 같은 `iat`/`exp` 라서
// 묻어 두면 같은 키가 두 번 직렬화된다. 대신 jwt.Claims 인터페이스를 직접 만족시킨다.
type Claims struct {
	Sub Subject `json:"sub"`
	// IsAdmin 은 Sub.IsAdmin 과 같은 값이다. SPA 가 sub 객체를 열어 보지 않고도
	// 라우터에서 한 번에 보도록 최상위에도 둔다.
	// **두 값을 다르게 넣지 마라.** 발급은 Issue 한 곳에서만 한다.
	IsAdmin   bool   `json:"isAdmin"`
	TokenUse  string `json:"tokenUse"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// ── jwt.Claims 구현 ───────────────────────────────────────────
// 라이브러리 검증기(만료 확인)가 쓰는 접근자다. 우리는 iss/nbf/aud 를 싣지 않으므로
// 그 셋은 nil 을 돌려준다 — jwt/v5 는 nil 을 "그 클레임 없음" 으로 보고 넘어간다.

func (c Claims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.ExpiresAt, 0)), nil
}

func (c Claims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.IssuedAt, 0)), nil
}

func (c Claims) GetNotBefore() (*jwt.NumericDate, error) { return nil, nil }
func (c Claims) GetIssuer() (string, error)              { return "", nil }
func (c Claims) GetAudience() (jwt.ClaimStrings, error)  { return nil, nil }

// GetSubject 는 인터페이스를 채우기 위한 것이고 실제 subject 는 Sub 객체다.
// 사람이 로그에서 알아볼 수 있도록 adminId 를 돌려준다.
func (c Claims) GetSubject() (string, error) { return c.Sub.AdminID, nil }

// TokenIssuer 는 어드민 HS256 JWT 를 발급·검증한다. 시크릿은 env ADMIN_JWT_SECRET.
type TokenIssuer struct {
	secret []byte
	now    func() time.Time
}

// NewTokenIssuer 는 어드민 토큰 발급기를 만든다.
//
// userSecret(=JWT_SECRET)을 함께 받는 이유는 **두 값이 같은지 여기서 한 번에 막기**
// 위해서다. 호출부마다 비교하게 두면 새 호출부가 생기는 날 그 검사가 빠진다.
// 같으면 ErrSecretReused 를 돌려주고, main.go 는 그 에러로 기동을 멈춘다.
func NewTokenIssuer(adminSecret, userSecret string) (*TokenIssuer, error) {
	if adminSecret == "" {
		return nil, ErrSecretMissing
	}
	if adminSecret == userSecret {
		return nil, ErrSecretReused
	}
	return &TokenIssuer{secret: []byte(adminSecret), now: time.Now}, nil
}

// Issue 는 로그인·비밀번호 변경 뒤에 내려 줄 액세스 토큰을 만든다.
//
// sub 는 **발급 시점의 Firestore 문서에서 만든 값이어야 한다.** 오래된 토큰의 sub 를
// 그대로 다시 서명하면 권한을 회수당한 어드민이 토큰을 갱신하며 권한을 되살릴 수 있다.
func (t *TokenIssuer) Issue(sub Subject, rememberMe bool) (string, error) {
	ttl := AccessTokenTTL
	if rememberMe {
		ttl = RememberMeTokenTTL
	}
	if sub.Permissions == nil {
		sub.Permissions = map[string]bool{}
	}
	now := t.now()
	claims := Claims{
		Sub:       sub,
		IsAdmin:   sub.IsAdmin,
		TokenUse:  TokenUse,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// Parse 는 토큰을 검증하고 클레임을 돌려준다.
//
// 서명 방식을 HS256 으로 못 박는다 — 지정하지 않으면 `alg: none` 토큰이나 공개키를
// HMAC 키로 쓰는 혼동 공격이 열린다.
func (t *TokenIssuer) Parse(token string) (*Claims, error) {
	var claims Claims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(t.now),
	)
	if _, err := parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return t.secret, nil
	}); err != nil {
		return nil, ErrInvalidToken
	}
	// 종류와 주체를 함께 본다. 같은 키로 다른 용도의 토큰을 만들 일이 생기더라도
	// 그것이 어드민 토큰 자리에 들어오지 못하게 한다.
	if claims.TokenUse != TokenUse || claims.Sub.AdminID == "" {
		return nil, ErrInvalidToken
	}
	if claims.Sub.Permissions == nil {
		claims.Sub.Permissions = map[string]bool{}
	}
	return &claims, nil
}
