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
		{"access", func(id string) (string, error) { return issuer.IssueAccess(id, "sess-1") }, TokenUseAccess, "user-1"},
		{"refresh", func(id string) (string, error) { return issuer.IssueRefresh(id, 0, "sess-1") }, TokenUseRefresh, "user-1"},
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

	access, err := issuer.IssueAccess("user-1", "sess-1")
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

	access, err := issuer.IssueAccess("user-1", "sess-1")
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
	token, err := NewTokenIssuer("secret-a").IssueAccess("user-1", "sess-1")
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
	token, err := issuer.IssueAccess("", "sess-1")
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

func TestRefreshVersionAndUniqueID(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")
	issuer.now = func() time.Time { return time.Unix(1800000000, 0) }
	a, err := issuer.IssueRefresh("user-1", 7, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := issuer.IssueRefresh("user-1", 7, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("같은 초에 새 리프레시 토큰을 발급하지 않았다")
	}
	id, ver, sid, err := issuer.ParseRefresh(a)
	if err != nil || id != "user-1" || ver != 7 || sid != "sess-1" {
		t.Fatal(id, ver, sid, err)
	}
	access, err := issuer.IssueAccess("user-1", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := issuer.parse(access, TokenUseAccess)
	if err != nil || c.Ver != 0 {
		t.Fatal(c, err)
	}
	// 🔴 액세스 토큰은 **버전은 안 싣고 세션 id 는 싣는다.** 둘을 같이 빼면 세션 목록의
	// 「지금 이 기기」가 영원히 안 뜨고, 둘을 같이 실으면 매 요청마다 문서를 읽게 된다
	// (token.go 의 Claims.Sid).
	uid, asid, err := issuer.ParseAccess(access)
	if err != nil || uid != "user-1" || asid != "sess-1" {
		t.Fatal(uid, asid, err)
	}
}

// 🔴 세션 장치 이전에 발급된 리프레시 토큰에는 sid 가 없다. ParseRefresh 가 그것을
// **빈 문자열로 정확히 드러내야** 호출부가 거절할 수 있다. 여기서 빈 값이 아닌 무언가로
// 채워지면(예: "unknown") 그 값이 세션 조회로 넘어가 엉뚱한 404/401 이 된다.
func TestParseRefreshWithoutSessionIsEmpty(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")
	token, err := issuer.issue("user-1", TokenUseRefresh, RefreshTokenTTL, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	id, ver, sid, err := issuer.ParseRefresh(token)
	if err != nil || id != "user-1" || ver != 3 || sid != "" {
		t.Fatal(id, ver, sid, err)
	}
}

// 액세스 토큰 수명이 곧 「끊기」가 듣기까지의 시간이다. 이 숫자가 조용히 늘어나면
// 세션을 끊어도 그만큼 더 쓸 수 있게 되는데, 그 사실은 어떤 테스트에서도 드러나지 않는다.
func TestAccessTokenTTLIsFifteenMinutes(t *testing.T) {
	if AccessTokenTTL != 15*time.Minute {
		t.Fatalf("AccessTokenTTL = %v — 「끊기」의 구멍 크기가 바뀌었다", AccessTokenTTL)
	}
}

func TestTokenRequiresExpiration(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1"}, TokenUse: TokenUseAccess}).SignedString(issuer.secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Parse(token, TokenUseAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("만료 없는 토큰을 허용했다", err)
	}
}
