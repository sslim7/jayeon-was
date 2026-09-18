package auth

import "context"

// ctxKey 는 비공개 타입이다. 문자열 키를 쓰면 다른 패키지가 같은 문자열로
// 값을 덮어쓸 수 있으므로 이 패키지 밖에서 만들 수 없는 타입을 키로 쓴다.
type ctxKey struct{}

// ctxSessionKey 는 세션 id 용 별도 키다. ctxKey 와 **다른 타입**이어야 한다 —
// 같은 타입에 필드만 달리 두면 한쪽 WithValue 가 다른 쪽을 지운다.
type ctxSessionKey struct{}

// WithUserID 는 인증된 사용자 ID 를 컨텍스트에 넣는다. 미들웨어가 호출한다.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, userID)
}

// UserID 는 미들웨어가 주입한 인증 사용자 ID 를 돌려준다. 미인증이면 빈 문자열.
func UserID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

// WithSessionID 는 액세스 토큰에 실려 온 세션 id 를 컨텍스트에 넣는다.
//
// ⚠️ **살아 있는 세션이라는 보증이 아니다.** 토큰 클레임을 그대로 옮긴 값일 뿐이고,
// 끊긴 세션의 액세스 토큰도 만료 전까지는 이 값을 달고 들어온다(token.go 의 Claims.Sid).
// 권한 판단에 쓰지 마라 — 「지금 이 기기」 표시 하나가 용도의 전부다.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, ctxSessionKey{}, sessionID)
}

// SessionID 는 미들웨어가 주입한 세션 id 를 돌려준다. 없으면 빈 문자열.
// 세션 장치 이전에 발급된 액세스 토큰이면 빈 문자열이 되고, 그때는 세션 목록에서
// 「지금 이 기기」 표시만 빠진다.
func SessionID(ctx context.Context) string {
	v, _ := ctx.Value(ctxSessionKey{}).(string)
	return v
}
