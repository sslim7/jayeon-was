package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 토큰 종류. 클레임의 `token_use` 에 들어가며, 파싱 시 반드시 대조한다.
// (액세스 토큰을 리프레시로 재사용하는 등의 혼용을 막는다)
const (
	TokenUseAccess  = "access"
	TokenUseRefresh = "refresh"
)

// 기본 유효 기간.
//   - 액세스: 짧게 잡아 탈취된 토큰의 수명을 제한한다. 재발급은 리프레시 토큰으로 한다.
//   - 리프레시: 모바일 앱이 재로그인 없이 버티는 기간. 서비스 요구가 정해지면 다시 본다.
const (
	AccessTokenTTL  = time.Hour
	RefreshTokenTTL = 90 * 24 * time.Hour
)

// ErrInvalidToken 은 서명 불일치·만료·종류 불일치를 모두 덮는다.
// 호출자에게 어느 쪽인지 알려 주지 않는 편이 안전하다.
var ErrInvalidToken = errors.New("auth: 유효하지 않은 토큰")

// Claims 는 WAS 가 발급하는 JWT 의 페이로드다.
// Subject 는 사용자 ID 다.
type Claims struct {
	jwt.RegisteredClaims
	TokenUse string `json:"token_use"`
	// Ver 는 발급 시점의 Account.TokenVersion 이다. **리프레시 토큰에만 싣는다.**
	//
	// 리프레시할 때 문서의 TokenVersion 과 대조해서, 비밀번호 변경으로 버전이 오른
	// 뒤에 남아 있던 옛 토큰을 전부 어긋나게 만드는 것이 이 값의 전부다. 리프레시
	// 토큰은 서버에 저장되지 않는 순수 JWT 라 이 대조 말고는 무효화할 방법이 없다.
	//
	// 🔴 **액세스 토큰에는 싣지 않는다.** 실으면 매 요청마다 사용자 문서를 한 번 더
	// 읽어 대조해야 해서 Firestore 읽기 비용이 요청 수만큼 늘어난다. 그 대가로
	// **비밀번호를 바꿔도 이미 발급된 액세스 토큰은 최대 AccessTokenTTL(1시간) 동안
	// 그대로 통한다** — 즉 이 장치는 "즉시 로그아웃" 이 아니라 "최대 한 시간 안에
	// 로그아웃" 이다. 비밀번호 변경 화면의 안내 문구가 그 사실과 맞아야 한다.
	// (Account.TokenVersion 주석에 같은 내용이 적혀 있다)
	//
	// omitempty 라 버전 0(=초기 계정)이면 클레임 자체가 빠지고, 파싱하면 다시 0 이
	// 된다. 대조는 그대로 성립한다.
	Ver int `json:"ver,omitempty"`
}

// TokenIssuer 는 HS256 JWT 를 발급·검증한다. 시크릿은 env JWT_SECRET.
type TokenIssuer struct {
	secret []byte
	// now 는 테스트가 시간을 주입할 수 있도록 필드로 둔다. 발급과 검증이 같은
	// 시계를 보아야 만료 경계를 테스트할 수 있다.
	now func() time.Time
}

// NewTokenIssuer 는 시크릿으로 발급기를 만든다.
func NewTokenIssuer(secret string) *TokenIssuer {
	return &TokenIssuer{secret: []byte(secret), now: time.Now}
}

// IssueAccess 는 이 API 호출용 Bearer 토큰을 발급한다.
// 🔴 버전을 싣지 않는다 — 이유는 Claims.Ver 주석에 있다.
func (t *TokenIssuer) IssueAccess(userID string) (string, error) {
	return t.issue(userID, TokenUseAccess, AccessTokenTTL, 0)
}

// IssueRefresh 는 토큰 세트 재발급용 토큰을 발급한다.
// ver 에는 발급 시점의 Account.TokenVersion 을 넣는다. 이 값을 빠뜨리면(0 으로 두면)
// 버전이 오른 계정의 리프레시가 전부 막히거나, 반대로 무효화가 아예 동작하지 않는다.
func (t *TokenIssuer) IssueRefresh(userID string, ver int) (string, error) {
	return t.issue(userID, TokenUseRefresh, RefreshTokenTTL, ver)
}

func (t *TokenIssuer) issue(subject, use string, ttl time.Duration, ver int) (string, error) {
	now := t.now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		TokenUse: use,
		Ver:      ver,
	}
	// 같은 초에 재발급해도 리프레시 문자열이 바뀌도록 고유 ID를 싣는다.
	// 서버에 사용 이력을 저장하지 않으므로 단일 사용을 보장하는 장치는 아니다.
	if use == TokenUseRefresh {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", err
		}
		claims.ID = hex.EncodeToString(id[:])
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// parse 는 토큰을 검증하고 클레임을 통째로 돌려준다.
// want 와 종류가 다르거나 subject 가 비면 ErrInvalidToken 이다.
func (t *TokenIssuer) parse(token, want string) (Claims, error) {
	var claims Claims
	parser := jwt.NewParser(
		// 🔴 허용 알고리즘을 HS256 하나로 못박는다. 지정하지 않으면 `alg: none`
		// 토큰이나 공개키를 HMAC 키로 쓰는 알고리즘 혼동 공격이 열린다.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(t.now),
		jwt.WithExpirationRequired(),
	)
	if _, err := parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return t.secret, nil
	}); err != nil {
		return Claims{}, ErrInvalidToken
	}
	// 🔴 종류를 대조하지 않으면 액세스 토큰이 리프레시 자리에서 그대로 통한다.
	if claims.TokenUse != want || claims.Subject == "" {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

// Parse 는 토큰을 검증하고 subject 를 돌려준다.
// want 와 종류가 다르면 ErrInvalidToken 이다. 액세스 토큰 검증(미들웨어)이 쓴다.
func (t *TokenIssuer) Parse(token, want string) (string, error) {
	claims, err := t.parse(token, want)
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

// ParseRefresh 는 리프레시 토큰을 검증하고 subject 와 **버전을 함께** 돌려준다.
//
// 버전을 같이 돌려주는 전용 함수로 둔 것은, 호출부가 Parse 로 subject 만 받아 가서
// 버전 대조를 건너뛰는 일을 막기 위해서다. 그렇게 되면 비밀번호를 바꿔도 옛 리프레시
// 토큰이 계속 도는데 **아무 에러도 나지 않아** 밖에서는 드러나지 않는다.
func (t *TokenIssuer) ParseRefresh(token string) (string, int, error) {
	claims, err := t.parse(token, TokenUseRefresh)
	if err != nil {
		return "", 0, err
	}
	return claims.Subject, claims.Ver, nil
}
