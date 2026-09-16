package admin

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 메뉴 권한 키 상수는 아직 없다(admin.go 의 권한 키 주석) — 어드민 메뉴가 하나도
// 만들어지지 않았기 때문이다. 이 테스트들이 보는 것은 **판정 규칙**이지 특정 메뉴가
// 아니므로 임의의 키를 쓴다. 메뉴가 생기면 그 상수로 바꾼다.
const (
	testPermA = "menu-a"
	testPermB = "menu-b"
)

// newTestIssuer 는 시계를 고정한 발급기다. 만료 검증을 실제 시간에 기대면
// 테스트가 느려지거나 실행 시점에 따라 흔들린다.
func newTestIssuer(t *testing.T, now time.Time) *TokenIssuer {
	t.Helper()
	iss, err := NewTokenIssuer("admin-secret", "user-secret")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	iss.now = func() time.Time { return now }
	return iss
}

var testSubject = Subject{
	AdminID:            "admin-1",
	Email:              "admin@nature.kr",
	Name:               "홍길동",
	IsAdmin:            true,
	Permissions:        map[string]bool{testPermA: true},
	MustChangePassword: false,
}

// TestSecretReuseRejected 는 이 패키지에서 가장 중요한 못이다.
//
// ADMIN_JWT_SECRET 이 JWT_SECRET 과 같으면 어드민 토큰이 일반 사용자 미들웨어의 서명
// 검증까지 통과한다(token.go ErrSecretReused). 그 조합은 경고가 아니라 기동 거부여야 한다.
func TestSecretReuseRejected(t *testing.T) {
	if _, err := NewTokenIssuer("same", "same"); !errors.Is(err, ErrSecretReused) {
		t.Fatalf("같은 시크릿을 받아들였다: err=%v, want ErrSecretReused", err)
	}
	if _, err := NewTokenIssuer("", "user-secret"); !errors.Is(err, ErrSecretMissing) {
		t.Fatalf("빈 시크릿: err=%v, want ErrSecretMissing", err)
	}
	if _, err := NewTokenIssuer("admin-secret", "user-secret"); err != nil {
		t.Fatalf("다른 시크릿인데 거절됐다: %v", err)
	}
}

// TestIssueParseRoundTrip 은 발급→파싱 왕복에서 sub 객체가 한 칸도 잃지 않는지 본다.
// SPA 가 이 구조를 그대로 읽으므로 필드 하나가 빠지면 화면이 통째로 빈다.
func TestIssueParseRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	iss := newTestIssuer(t, now)

	token, err := iss.Issue(testSubject, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := iss.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got.Sub.AdminID != testSubject.AdminID ||
		got.Sub.Email != testSubject.Email ||
		got.Sub.Name != testSubject.Name ||
		got.Sub.IsAdmin != testSubject.IsAdmin ||
		got.Sub.MustChangePassword != testSubject.MustChangePassword {
		t.Fatalf("sub 가 왕복에서 달라졌다: %+v", got.Sub)
	}
	if !got.Sub.Permissions[testPermA] {
		t.Fatalf("permissions 가 왕복에서 사라졌다: %+v", got.Sub.Permissions)
	}
	if got.TokenUse != TokenUse {
		t.Fatalf("tokenUse = %q, want %q", got.TokenUse, TokenUse)
	}
	// 최상위 isAdmin 은 sub.isAdmin 과 언제나 같아야 한다.
	if got.IsAdmin != got.Sub.IsAdmin {
		t.Fatalf("isAdmin(%v) != sub.isAdmin(%v)", got.IsAdmin, got.Sub.IsAdmin)
	}
	if got.ExpiresAt != now.Add(AccessTokenTTL).Unix() {
		t.Fatalf("exp = %d, want %d", got.ExpiresAt, now.Add(AccessTokenTTL).Unix())
	}
}

// TestIssueClaimShape 는 직렬화된 JSON 키가 SPA 가 읽을 모양과 같은지 본다.
// Go 구조체가 왕복만 맞으면 통과해 버리는 실수(예: json 태그 오타)를 여기서 잡는다.
func TestIssueClaimShape(t *testing.T) {
	iss := newTestIssuer(t, time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	token, err := iss.Issue(testSubject, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var raw jwt.MapClaims
	// **만료 검사를 끈다.** 이 테스트가 보는 것은 직렬화된 JSON 키의 모양이지 유효기간이
	// 아니다. 발급 시각을 2026-09-03 으로 고정해 두었으므로, 끄지 않으면 그날로부터
	// AccessTokenTTL 이 지난 뒤 **코드를 한 줄도 바꾸지 않았는데 갑자기 깨진다**
	// (원본 저장소에서 실제로 그렇게 터진 적이 있다). 만료 자체는 TestParseExpired 가 본다.
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithoutClaimsValidation(),
	)
	if _, err := parser.ParseWithClaims(token, &raw, func(*jwt.Token) (any, error) {
		return []byte("admin-secret"), nil
	}); err != nil {
		t.Fatalf("원시 파싱 실패: %v", err)
	}

	for _, key := range []string{"sub", "isAdmin", "tokenUse", "iat", "exp"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("최상위 클레임 %q 가 없다", key)
		}
	}
	sub, ok := raw["sub"].(map[string]any)
	if !ok {
		t.Fatalf("sub 가 객체가 아니다: %T", raw["sub"])
	}
	for _, key := range []string{"adminId", "email", "name", "isAdmin", "permissions", "mustChangePassword"} {
		if _, ok := sub[key]; !ok {
			t.Errorf("sub.%s 가 없다", key)
		}
	}
	// 해시가 토큰에 실리는 사고를 막는 못. JWT 는 서명만 되어 있고 암호화되어 있지 않다.
	if _, leaked := sub["passwordHash"]; leaked {
		t.Fatal("passwordHash 가 토큰에 실렸다")
	}
}

// TestRememberMeTTL 은 「로그인 상태 유지」가 6주짜리 토큰을 내는지 본다.
func TestRememberMeTTL(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	iss := newTestIssuer(t, now)

	token, err := iss.Issue(testSubject, true)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := iss.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := now.Add(RememberMeTokenTTL).Unix(); got.ExpiresAt != want {
		t.Fatalf("rememberMe exp = %d, want %d", got.ExpiresAt, want)
	}
}

// TestParseExpired 는 만료된 토큰이 거절되는지 본다.
func TestParseExpired(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	iss := newTestIssuer(t, now)
	token, err := iss.Issue(testSubject, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// 만료 직전에는 통과해야 한다 — 경계가 반대로 밀리면 12시간 토큰이 실제로는
	// 훨씬 짧게(혹은 길게) 산다.
	iss.now = func() time.Time { return now.Add(AccessTokenTTL - time.Second) }
	if _, err := iss.Parse(token); err != nil {
		t.Fatalf("만료 1초 전 토큰이 거절됐다: %v", err)
	}

	iss.now = func() time.Time { return now.Add(AccessTokenTTL + time.Minute) }
	if _, err := iss.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("만료 토큰: err=%v, want ErrInvalidToken", err)
	}
}

// TestParseRejectsForeignSignature 는 다른 키로 서명한 토큰을 거절하는지 본다.
//
// 특히 **JWT_SECRET 으로 서명한 토큰**을 거절해야 한다. 두 키를 가르는 이유가 여기 있고,
// 이 검사가 무너지면 사용자 토큰이 어드민 API 를 열 수 있게 된다.
func TestParseRejectsForeignSignature(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	admins := newTestIssuer(t, now)

	// 사용자 시크릿("user-secret")으로 서명한 어드민 모양의 토큰.
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Sub:       testSubject,
		IsAdmin:   true,
		TokenUse:  TokenUse,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
	}).SignedString([]byte("user-secret"))
	if err != nil {
		t.Fatalf("위조 토큰 생성 실패: %v", err)
	}
	if _, err := admins.Parse(forged); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("JWT_SECRET 으로 서명한 토큰이 통과했다: err=%v", err)
	}

	// 서명 없는 토큰(alg: none)도 막아야 한다.
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		Sub:       testSubject,
		TokenUse:  TokenUse,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("alg:none 토큰 생성 실패: %v", err)
	}
	if _, err := admins.Parse(none); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("alg:none 토큰이 통과했다: err=%v", err)
	}
}

// TestParseRejectsWrongTokenUse 는 종류가 다른 토큰을 거절하는지 본다.
func TestParseRejectsWrongTokenUse(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	iss := newTestIssuer(t, now)

	other, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Sub:       testSubject,
		TokenUse:  "access", // internal/auth 의 사용자 액세스 토큰 종류
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
	}).SignedString([]byte("admin-secret"))
	if err != nil {
		t.Fatalf("토큰 생성 실패: %v", err)
	}
	if _, err := iss.Parse(other); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tokenUse 가 다른 토큰이 통과했다: err=%v", err)
	}
}

// TestSubjectAllows 는 권한 판정을 못 박는다. 서버가 최종 판정자이므로
// 이 함수가 틀리면 사이드바에 없는 메뉴도 API 로 열린다.
func TestSubjectAllows(t *testing.T) {
	tests := []struct {
		name string
		sub  Subject
		menu string
		want bool
	}{
		{
			name: "isAdmin 은 전 메뉴",
			sub:  Subject{IsAdmin: true, Permissions: map[string]bool{}},
			menu: testPermB,
			want: true,
		},
		{
			name: "권한 맵에 있는 메뉴",
			sub:  Subject{Permissions: map[string]bool{testPermA: true}},
			menu: testPermA,
			want: true,
		},
		{
			name: "권한 맵에 없는 메뉴",
			sub:  Subject{Permissions: map[string]bool{testPermA: true}},
			menu: testPermB,
			want: false,
		},
		{
			name: "false 로 적힌 키는 없는 키와 같다",
			sub:  Subject{Permissions: map[string]bool{testPermB: false}},
			menu: testPermB,
			want: false,
		},
		{
			name: "권한 맵이 nil 이어도 터지지 않는다",
			sub:  Subject{},
			menu: testPermA,
			want: false,
		},
		{
			name: "빈 메뉴 키는 로그인만 요구한다",
			sub:  Subject{Permissions: map[string]bool{}},
			menu: "",
			want: true,
		},
		{
			// 빈 키를 permissions 에 심어 두는 식의 우회가 통하면 안 된다는 뜻은 아니다 —
			// 빈 키는 애초에 맵을 보지 않는다. 그 사실을 못 박는다.
			name: "빈 메뉴 키는 맵을 보지 않는다",
			sub:  Subject{Permissions: map[string]bool{"": false}},
			menu: "",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sub.Allows(tc.menu); got != tc.want {
				t.Fatalf("Allows(%q) = %v, want %v", tc.menu, got, tc.want)
			}
		})
	}
}
