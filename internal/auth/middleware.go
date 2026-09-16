package auth

import (
	"net/http"
	"strings"
)

// Middleware 는 Authorization: Bearer 액세스 토큰이 유효하면 사용자 ID 를
// 컨텍스트에 주입한다. 토큰이 없거나 유효하지 않아도 요청을 막지 않고
// 미인증 상태로 통과시키며, 401 응답 여부는 각 핸들러가 UserID(ctx) 로 판단한다.
// 공개 엔드포인트가 섞인 API 에서 전역 차단을 하면 그 엔드포인트까지 막히기 때문이다.
// 지금 nature 에는 공개 엔드포인트가 아직 없지만, 전역 차단으로 바꾸면 그런
// 엔드포인트가 생기는 날 이 미들웨어를 되돌려야 한다.
func (t *TokenIssuer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if userID, err := t.Parse(strings.TrimPrefix(h, "Bearer "), TokenUseAccess); err == nil {
				r = r.WithContext(WithUserID(r.Context(), userID))
			}
		}
		next.ServeHTTP(w, r)
	})
}
