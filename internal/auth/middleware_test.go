package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware(t *testing.T) {
	issuer := NewTokenIssuer("test-secret")

	valid, err := issuer.IssueAccess("user-1", "sess-1")
	if err != nil {
		t.Fatalf("발급 실패: %v", err)
	}
	// 리프레시 토큰은 Bearer 자리에서 무효여야 한다.
	refresh, err := issuer.IssueRefresh("user-1", 0, "sess-1")
	if err != nil {
		t.Fatalf("발급 실패: %v", err)
	}

	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"헤더 없음", "", ""},
		{"Bearer 접두사 없음", valid, ""},
		{"깨진 토큰", "Bearer not-a-jwt", ""},
		{"리프레시 토큰", "Bearer " + refresh, ""},
		{"유효한 액세스 토큰", "Bearer " + valid, "user-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			var got string
			h := issuer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				got = UserID(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			// 인증에 실패해도 다음 핸들러는 반드시 불려야 한다.
			// 401 판정은 미들웨어가 아니라 핸들러의 몫이다.
			if !called {
				t.Fatal("다음 핸들러가 호출되지 않았다")
			}
			if got != tc.want {
				t.Fatalf("UserID = %q, want %q", got, tc.want)
			}
		})
	}
}
