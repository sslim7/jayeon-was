package admin

import (
	"encoding/json"
	"github.com/sslim7/nature-was/internal/credentials"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testTarget 은 활동 로그의 도메인 문자열 자리다. 도메인 어드민 메뉴가 아직 없어
// Target* 상수도 이 패키지 자신의 둘뿐이라(audit.go) 가드 테스트는 임의의 값을 쓴다 —
// 여기서 보는 것은 인증·권한 판정이지 어느 도메인인지가 아니다.
const testTarget = "test-domain"

// TestValidateEmail 은 이메일 검증의 경계를 못 박는다.
//
// 형식 검사가 오타 방지만 하는 것이 아니다 — 이 값이 admins_by_email 의 **문서 ID** 가
// 되므로(store.go) `/` 나 공백이 통과하면 잘못된 경로의 문서가 만들어진다.
func TestValidateEmail(t *testing.T) {
	ok := []string{
		"admin@nature.kr",
		"a.b+tag@sub.example.co.kr",
		"admin-1_2@example.com",
	}
	for _, e := range ok {
		if msg := validateEmail(e); msg != "" {
			t.Errorf("validateEmail(%q) = %q, 통과해야 한다", e, msg)
		}
	}

	bad := []string{
		"",
		"admin",
		"admin@",
		"@nature.kr",
		"admin@nature",
		"ad min@nature.kr",
		"admin/x@nature.kr", // Firestore 문서 ID 에 쓸 수 없다
		"admin@jay/eon.kr",
		"Admin@Nature.kr", // NormalizeEmail 을 거치지 않은 값은 거절된다
	}
	for _, e := range bad {
		if msg := validateEmail(e); msg == "" {
			t.Errorf("validateEmail(%q) 가 통과했다", e)
		}
	}

	// 정규화를 거치면 대문자 주소도 통과해야 한다.
	if msg := validateEmail(credentials.NormalizeEmail("  Admin@NaTure.KR ")); msg != "" {
		t.Errorf("정규화한 주소가 거절됐다: %s", msg)
	}
}

// TestValidateName 은 관리자 이름 길이를 룬으로 세는지 본다.
// 바이트로 재면 한글 이름이 실제보다 세 배 길게 계산돼 멀쩡한 이름이 거절된다.
func TestValidateName(t *testing.T) {
	if msg := validateName(""); msg == "" {
		t.Error("빈 이름이 통과했다")
	}
	long := ""
	for i := 0; i < maxNameRunes; i++ {
		long += "가"
	}
	if msg := validateName(long); msg != "" {
		t.Errorf("%d자 이름이 거절됐다: %s", maxNameRunes, msg)
	}
	if msg := validateName(long + "가"); msg == "" {
		t.Errorf("%d자 이름이 통과했다", maxNameRunes+1)
	}
}

// TestToDTOHidesPasswordHash 는 응답에 bcrypt 해시가 실리지 않는지 못 박는다.
//
// 이 패키지에서 어드민 계정이 밖으로 나가는 길은 toDTO 하나뿐이다(handler.go 주석).
// 필드를 하나 더하는 날 이 테스트가 막아 준다.
func TestToDTOHidesPasswordHash(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	dto := toDTO(Admin{
		ID:           "admin-1",
		Email:        "admin@nature.kr",
		PasswordHash: "$2a$10$절대나가면안된다",
		Name:         "홍길동",
		IsActive:     true,
		CreatedAt:    now,
		UpdatedAt:    now,
	})

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, leaked := back["passwordHash"]; leaked {
		t.Fatal("응답에 passwordHash 가 있다")
	}
	if strings.Contains(string(raw), "절대나가면안된다") {
		t.Fatalf("해시 값이 응답 어딘가에 실렸다: %s", raw)
	}
	for _, key := range []string{
		"adminId", "email", "name", "isActive", "isAdmin",
		"permissions", "mustChangePassword", "createdAt", "updatedAt",
	} {
		if _, ok := back[key]; !ok {
			t.Errorf("응답에 %q 가 없다", key)
		}
	}
	// permissions 가 nil 이어도 JSON 에서는 객체여야 한다 — null 이면 SPA 가 터진다.
	if _, ok := back["permissions"].(map[string]any); !ok {
		t.Fatalf("permissions = %#v, want 객체", back["permissions"])
	}
	// 시각은 RFC3339 UTC 문자열이다.
	if back["createdAt"] != "2026-09-03T12:00:00Z" {
		t.Fatalf("createdAt = %v", back["createdAt"])
	}
}

// newGuardOnlyHandler 는 토큰 검증만 필요한 테스트용 핸들러다.
// Firestore 를 붙이지 않는다 — 401·403 은 저장소에 닿기 전에 결판난다.
func newGuardOnlyHandler(t *testing.T) *Handler {
	t.Helper()
	tokens, err := NewTokenIssuer("admin-secret", "user-secret")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	return &Handler{tokens: tokens, now: time.Now}
}

// TestGuardBlocks 는 어드민 라우트가 **차단형**임을 못 박는다.
//
// internal/auth 의 미들웨어는 토큰이 없어도 통과시키고 판정을 핸들러에 맡기는데,
// 그 결을 여기 흉내 내면 권한 확인을 잊은 새 핸들러 하나가 곧바로 구멍이 된다.
func TestGuardBlocks(t *testing.T) {
	h := newGuardOnlyHandler(t)

	viewer, err := h.tokens.Issue(Subject{
		AdminID:     "admin-2",
		Permissions: map[string]bool{testPermA: true},
	}, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	root, err := h.tokens.Issue(Subject{AdminID: "admin-1", IsAdmin: true}, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	reached := false
	pass := func(w http.ResponseWriter, r *http.Request, c *Claims) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}

	tests := []struct {
		name    string
		handler http.HandlerFunc
		auth    string
		want    int
		code    string
	}{
		{"토큰 없음", h.Guarded(testTarget, testPermA, pass), "", http.StatusUnauthorized, codeUnauthorized},
		{"Bearer 아님", h.Guarded(testTarget, testPermA, pass), "Token " + viewer, http.StatusUnauthorized, codeUnauthorized},
		{"서명이 다름", h.Guarded(testTarget, testPermA, pass), "Bearer eyJhbGciOiJIUzI1NiJ9.e30.x", http.StatusUnauthorized, codeUnauthorized},
		{"권한 있는 메뉴", h.Guarded(testTarget, testPermA, pass), "Bearer " + viewer, http.StatusOK, ""},
		{"권한 없는 메뉴", h.Guarded(testTarget, testPermB, pass), "Bearer " + viewer, http.StatusForbidden, codeForbidden},
		{"isAdmin 은 전 메뉴", h.Guarded(testTarget, testPermB, pass), "Bearer " + root, http.StatusOK, ""},
		{"AdminOnly 는 권한 맵으로 열리지 않는다", h.AdminOnly(TargetAdmin, pass), "Bearer " + viewer, http.StatusForbidden, codeForbidden},
		{"AdminOnly 에 isAdmin", h.AdminOnly(TargetAdmin, pass), "Bearer " + root, http.StatusOK, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			// GET 으로 부른다 — 감사 기록은 GET 을 남기지 않으므로 Firestore 가 필요 없다.
			r := httptest.NewRequest(http.MethodGet, "/admin/probe", nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			tc.handler(w, r)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusOK {
				if !reached {
					t.Fatal("핸들러에 도달하지 못했다")
				}
				return
			}
			if reached {
				t.Fatal("거절돼야 하는데 핸들러가 실행됐다")
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("에러 바디가 JSON 이 아니다: %s", w.Body.String())
			}
			if body["code"] != tc.code {
				t.Fatalf("code = %v, want %s", body["code"], tc.code)
			}
			if msg, _ := body["message"].(string); msg == "" {
				t.Fatal("message 가 비어 있다")
			}
		})
	}
}
