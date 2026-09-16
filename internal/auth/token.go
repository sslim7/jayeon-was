package auth

import (
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
func (t *TokenIssuer) IssueAccess(userID string) (string, error) {
	return t.issue(userID, TokenUseAccess, AccessTokenTTL)
}

// IssueRefresh 는 토큰 세트 재발급용 토큰을 발급한다.
func (t *TokenIssuer) IssueRefresh(userID string) (string, error) {
	return t.issue(userID, TokenUseRefresh, RefreshTokenTTL)
}

func (t *TokenIssuer) issue(subject, use string, ttl time.Duration) (string, error) {
	now := t.now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		TokenUse: use,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// Parse 는 토큰을 검증하고 subject 를 돌려준다.
// want 와 종류가 다르면 ErrInvalidToken 이다.
func (t *TokenIssuer) Parse(token, want string) (string, error) {
	var claims Claims
	parser := jwt.NewParser(
		// 🔴 허용 알고리즘을 HS256 하나로 못박는다. 지정하지 않으면 `alg: none`
		// 토큰이나 공개키를 HMAC 키로 쓰는 알고리즘 혼동 공격이 열린다.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(t.now),
	)
	if _, err := parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return t.secret, nil
	}); err != nil {
		return "", ErrInvalidToken
	}
	// 🔴 종류를 대조하지 않으면 액세스 토큰이 리프레시 자리에서 그대로 통한다.
	if claims.TokenUse != want || claims.Subject == "" {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}
