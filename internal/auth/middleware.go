package auth

import (
	"net/http"
	"strings"
)

// Middleware 는 Authorization: Bearer 액세스 토큰이 유효하면 사용자 ID 와 세션 ID 를
// 컨텍스트에 주입한다. 토큰이 없거나 유효하지 않아도 요청을 막지 않고
// 미인증 상태로 통과시키며, 401 응답 여부는 각 핸들러가 UserID(ctx) 로 판단한다.
// 공개 엔드포인트가 섞인 API 에서 전역 차단을 하면 그 엔드포인트까지 막히기 때문이다.
// 지금 nature 에는 공개 엔드포인트가 아직 없지만, 전역 차단으로 바꾸면 그런
// 엔드포인트가 생기는 날 이 미들웨어를 되돌려야 한다.
//
// 🔴 **세션이 살아 있는지는 여기서 보지 않는다.** 보려면 매 요청마다 Firestore 를 한 번
// 더 읽어야 하고, 그것을 하지 않기로 한 대가가 액세스 토큰 15분이다(token.go 의
// AccessTokenTTL). 여기에 조회를 붙이려는 사람은 그 주석을 먼저 읽어라.
func (t *TokenIssuer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if userID, sessionID, err := t.ParseAccess(strings.TrimPrefix(h, "Bearer ")); err == nil {
				ctx := WithUserID(r.Context(), userID)
				// 세션 장치 이전에 발급된 토큰이면 빈 문자열이다. 넣어도 SessionID 가
				// 빈 값을 돌려줄 뿐이라 분기를 두지 않는다.
				r = r.WithContext(WithSessionID(ctx, sessionID))
			}
		}
		next.ServeHTTP(w, r)
	})
}
