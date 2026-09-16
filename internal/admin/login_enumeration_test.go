package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/jayeon-was/internal/credentials"
)

// 이 파일은 실제 Firestore 에뮬레이터를 상대로 도는 통합 테스트다.
//
//	firebase emulators:exec --only firestore --project demo-test \
//	  --config firebase.test.json 'go test ./...'
//
// FIRESTORE_EMULATOR_HOST 가 없으면 skip 한다.

// newEmulatorHandler 는 에뮬레이터에 붙은 Handler 를 만든다.
// 에뮬레이터가 없으면 테스트를 skip 한다.
func newEmulatorHandler(t *testing.T) *Handler {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST 가 없다 — 에뮬레이터 통합 테스트를 건너뛴다")
	}

	ctx := context.Background()
	fs, err := firestore.NewClient(ctx, "demo-test")
	if err != nil {
		t.Fatalf("Firestore 클라이언트 생성 실패: %v", err)
	}
	t.Cleanup(func() { fs.Close() })

	tokens, err := NewTokenIssuer("test-admin-secret", "test-user-secret")
	if err != nil {
		t.Fatalf("토큰 발급기 생성 실패: %v", err)
	}
	return &Handler{
		store:   NewStore(fs),
		audit:   newAuditStore(fs),
		tokens:  tokens,
		now:     time.Now,
		baseCtx: ctx,
	}
}

// postLogin 은 로그인 요청 하나를 보내고 상태코드와 본문을 돌려준다.
func postLogin(t *testing.T, h *Handler, email, password string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("요청 본문 직렬화 실패: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/admin.login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.login(rec, req)
	return rec.Code, rec.Body.String()
}

// TestLoginDoesNotRevealWhetherAccountExists 는 **이 패키지의 계정 열거 방어를 못 박는다.**
//
// 🔴 없는 이메일로 로그인한 응답과, 있는 이메일에 틀린 비밀번호로 로그인한 응답이
// **바이트 단위로 같아야 한다.** 하나라도 갈리면 어드민 이메일 목록을 밖에서 훑을 수 있다.
//
// internal/credentials 의 미끼 해시(dummyHash)는 두 경로의 **응답 시간**을 맞추려고 있고,
// handler.go 가 비활성 판정을 비밀번호 확인 뒤에 두는 것도 같은 목적이다. 그런데 문구를
// 갈라 놓으면 그 둘이 통째로 무의미해진다 — 시간을 잴 것도 없이 본문이 알려 주기 때문이다.
// 실제로 형제 프로젝트 birdieup-was 가 "등록된 어드민 계정이 아닙니다" 와
// "비밀번호를 확인하세요" 로 갈라 두어 두 방어를 스스로 되돌리고 있다.
//
// 문구를 친절하게 고치고 싶어지는 자리라 테스트로 고정해 둔다.
func TestLoginDoesNotRevealWhetherAccountExists(t *testing.T) {
	h := newEmulatorHandler(t)
	ctx := context.Background()

	// 존재하는 계정 하나를 만든다. 이메일은 테스트마다 달라야 다른 실행과 부딪히지 않는다.
	email := credentials.NormalizeEmail("enum-probe-" + time.Now().Format("20060102150405.000000") + "@jayeon.kr")
	hash, err := credentials.HashPassword("correct-password-1234")
	if err != nil {
		t.Fatalf("비밀번호 해싱 실패: %v", err)
	}
	if _, err := h.store.Create(ctx, Admin{
		Email:        email,
		PasswordHash: hash,
		Name:         "열거방어 테스트",
		IsActive:     true,
		IsAdmin:      true,
	}); err != nil {
		t.Fatalf("어드민 계정 생성 실패: %v", err)
	}

	existingStatus, existingBody := postLogin(t, h, email, "wrong-password")
	missingStatus, missingBody := postLogin(t, h, "no-such-admin@jayeon.kr", "wrong-password")

	if existingStatus != http.StatusUnauthorized {
		t.Errorf("있는 계정 + 틀린 비밀번호는 401 이어야 한다: got %d", existingStatus)
	}
	if missingStatus != existingStatus {
		t.Errorf("없는 계정과 틀린 비밀번호의 상태코드가 다르다 — 계정 존재 여부가 샌다: 있음=%d 없음=%d",
			existingStatus, missingStatus)
	}
	if missingBody != existingBody {
		t.Errorf("없는 계정과 틀린 비밀번호의 응답 본문이 다르다 — 계정 존재 여부가 샌다\n"+
			" 있음: %s\n 없음: %s", existingBody, missingBody)
	}
}

// TestLoginInactiveAccountNeedsCorrectPassword 는 비활성 판정이 비밀번호 확인 **뒤에**
// 있다는 것을 못 박는다.
//
// 앞에 두면 비밀번호를 모르는 사람도 "그 계정은 있고 잠겨 있다" 를 알게 된다 — 위
// 테스트가 막는 것과 같은 누출을 403 으로 여는 셈이다.
func TestLoginInactiveAccountNeedsCorrectPassword(t *testing.T) {
	h := newEmulatorHandler(t)
	ctx := context.Background()

	const password = "correct-password-1234"
	email := credentials.NormalizeEmail("inactive-probe-" + time.Now().Format("20060102150405.000000") + "@jayeon.kr")
	hash, err := credentials.HashPassword(password)
	if err != nil {
		t.Fatalf("비밀번호 해싱 실패: %v", err)
	}
	if _, err := h.store.Create(ctx, Admin{
		Email:        email,
		PasswordHash: hash,
		Name:         "비활성 테스트",
		IsActive:     false,
		IsAdmin:      true,
	}); err != nil {
		t.Fatalf("어드민 계정 생성 실패: %v", err)
	}

	// 비밀번호를 모르면 비활성이라는 사실조차 알 수 없어야 한다 — 401 이고,
	// 없는 계정에 대한 응답과 같아야 한다.
	wrongStatus, wrongBody := postLogin(t, h, email, "wrong-password")
	_, missingBody := postLogin(t, h, "no-such-admin@jayeon.kr", "wrong-password")
	if wrongStatus != http.StatusUnauthorized {
		t.Errorf("비활성 계정 + 틀린 비밀번호는 401 이어야 한다(403 이면 계정 존재가 샌다): got %d", wrongStatus)
	}
	if wrongBody != missingBody {
		t.Errorf("비활성 계정에 틀린 비밀번호를 넣었을 때의 응답이 없는 계정과 달라서는 안 된다\n"+
			" 비활성: %s\n 없음  : %s", wrongBody, missingBody)
	}

	// 비밀번호가 맞을 때에야 비로소 비활성이라고 알려 준다.
	rightStatus, rightBody := postLogin(t, h, email, password)
	if rightStatus != http.StatusForbidden {
		t.Errorf("비활성 계정 + 맞는 비밀번호는 403 이어야 한다: got %d body=%s", rightStatus, rightBody)
	}
}
