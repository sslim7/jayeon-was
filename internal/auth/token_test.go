package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestTokenIssueParseRoundTrip(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")

	cases := []struct {
		name    string
		issue   func(string) (string, error)
		use     string
		subject string
	}{
		{"access", issuer.IssueAccess, TokenUseAccess, "user-1"},
		{"refresh", issuer.IssueRefresh, TokenUseRefresh, "user-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := tc.issue(tc.subject)
			if err != nil {
				t.Fatalf("발급 실패: %v", err)
			}
			got, err := issuer.Parse(token, tc.use)
			if err != nil {
				t.Fatalf("검증 실패: %v", err)
			}
			if got != tc.subject {
				t.Fatalf("subject = %q, want %q", got, tc.subject)
			}
		})
	}
}

// 종류 대조가 없으면 액세스 토큰이 리프레시 자리에서 그대로 통한다.
// token_use 클레임이 존재하는 이유가 이 테스트다.
func TestTokenRejectsWrongTokenUse(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")

	access, err := issuer.IssueAccess("user-1")
	if err != nil {
		t.Fatalf("발급 실패: %v", err)
	}
	if _, err := issuer.Parse(access, TokenUseRefresh); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestTokenExpires(t *testing.T) {
	base := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	now := base
	issuer := NewTokenIssuer("test-secret")
	issuer.now = func() time.Time { return now }

	access, err := issuer.IssueAccess("user-1")
	if err != nil {
		t.Fatalf("발급 실패: %v", err)
	}

	// 만료 직전에는 통과한다.
	now = base.Add(AccessTokenTTL - time.Second)
	if _, err := issuer.Parse(access, TokenUseAccess); err != nil {
		t.Fatalf("만료 전 검증 실패: %v", err)
	}

	// 만료 후에는 거부한다.
	now = base.Add(AccessTokenTTL + time.Second)
	if _, err := issuer.Parse(access, TokenUseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestTokenRejectsOtherSecret(t *testing.T) {
	token, err := NewTokenIssuer("secret-a").IssueAccess("user-1")
	if err != nil {
		t.Fatalf("발급 실패: %v", err)
	}
	if _, err := NewTokenIssuer("secret-b").Parse(token, TokenUseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

// 🔴 서명이 없는 토큰(alg: none)을 거부하는지 본다. 파서에 허용 알고리즘을
// 주지 않으면 이 토큰이 그대로 통과해 누구나 아무 userId 로 위장할 수 있다.
func TestTokenRejectsAlgNone(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
		TokenUse: TokenUseAccess,
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("alg:none 토큰 생성 실패: %v", err)
	}
	if _, err := issuer.Parse(token, TokenUseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

// 🔴 HS256 이외의 알고리즘으로 서명된 토큰을 거부하는지 본다. 같은 시크릿을
// HS512 로 쓴 토큰은 허용 알고리즘을 못박지 않으면 그대로 검증을 통과한다 —
// alg 를 토큰이 스스로 고르게 두면 안 된다는 것이 이 테스트가 지키는 속성이다.
// (alg:none 은 라이브러리 자체 방어에도 걸리지만 이 경우는 걸리지 않는다)
func TestTokenRejectsOtherAlgorithm(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
		TokenUse: TokenUseAccess,
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString(issuer.secret)
	if err != nil {
		t.Fatalf("HS512 토큰 생성 실패: %v", err)
	}
	if _, err := issuer.Parse(token, TokenUseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestTokenRejectsEmptySubject(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")
	token, err := issuer.IssueAccess("")
	if err != nil {
		t.Fatalf("발급 실패: %v", err)
	}
	if _, err := issuer.Parse(token, TokenUseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestTokenTTLs(t *testing.T) {
	if RefreshTokenTTL <= AccessTokenTTL {
		t.Fatalf("리프레시 토큰은 액세스 토큰보다 길어야 한다")
	}
}
