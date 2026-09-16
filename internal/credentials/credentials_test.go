package credentials

import (
	"strings"
	"testing"
	"time"
)

// TestVerifyPasswordDummyHash 는 계정이 없을 때도 bcrypt 비교가 실제로 돌아가는지 본다.
//
// 미끼 해시가 깨져 있으면 CompareHashAndPassword 가 형식 오류로 곧장 반환해 버려
// 시간 차이가 되살아난다 — 그러면 로그인 API 로 어드민 이메일을 훑을 수 있게 된다.
func TestVerifyPasswordDummyHash(t *testing.T) {
	if VerifyPassword("", "아무거나") {
		t.Fatal("미끼 해시가 비밀번호를 통과시켰다")
	}

	// 실제 비교가 일어났는지는 소요 시간으로 본다. bcrypt cost 10 은 밀리초 단위가 걸리고,
	// 형식 오류로 즉시 반환했다면 이보다 훨씬 빠르다.
	start := time.Now()
	VerifyPassword("", "아무거나")
	if elapsed := time.Since(start); elapsed < time.Millisecond {
		t.Fatalf("bcrypt 비교가 실제로 돌지 않았다(%s) — dummyHash 형식을 확인하라", elapsed)
	}

	hash, err := HashPassword("hunter22")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "hunter22") {
		t.Fatal("맞는 비밀번호를 거절했다")
	}
	if VerifyPassword(hash, "hunter23") {
		t.Fatal("틀린 비밀번호를 통과시켰다")
	}
}

// TestNormalizeEmail 은 저장과 조회가 같은 표준형을 쓰는지 못 박는다.
// 여기가 갈리면 "콘솔에는 계정이 있는데 로그인은 안 된다" 가 된다.
func TestNormalizeEmail(t *testing.T) {
	tests := map[string]string{
		"Admin@NaTure.KR": "admin@nature.kr",
		"  admin@j.kr  ":  "admin@j.kr",
		"admin@nature.kr": "admin@nature.kr",
		"\tADMIN@J.KR\n":  "admin@j.kr",
	}
	for in, want := range tests {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHashPasswordTooLong(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Fatal("72바이트 초과 비밀번호 허용")
	}
}
