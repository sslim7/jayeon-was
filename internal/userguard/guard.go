// Package userguard는 개인정보를 다루는 사용자 도메인의 접근 조건을 모은다.
// JWT가 남아 있어도 삭제·비활성 계정이나 임시 비밀번호 계정이 SMS 데이터에 접근하지 못하게 한다.
package userguard

import (
	"context"
	"errors"
	"net/http"

	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
)

// AccountReader는 기존 users.Store가 구현한다. 인증 핸들러 자체는 이 가드로 감싸지 않는다.
type AccountReader interface {
	Get(context.Context, string) (auth.Account, error)
}

func New(store AccountReader) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			id := auth.UserID(r.Context())
			if id == "" {
				httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "로그인이 필요해요")
				return
			}
			account, err := store.Get(r.Context(), id)
			if errors.Is(err, auth.ErrAccountNotFound) {
				httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "로그인이 필요해요")
				return
			}
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "서버 오류가 생겼어요")
				return
			}
			if !account.IsActive {
				httpx.WriteError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "사용할 수 없는 계정이에요")
				return
			}
			if account.MustChangePassword {
				httpx.WriteError(w, http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "비밀번호를 먼저 변경해 주세요")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
