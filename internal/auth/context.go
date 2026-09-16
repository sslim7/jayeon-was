package auth

import "context"

// ctxKey 는 비공개 타입이다. 문자열 키를 쓰면 다른 패키지가 같은 문자열로
// 값을 덮어쓸 수 있으므로 이 패키지 밖에서 만들 수 없는 타입을 키로 쓴다.
type ctxKey struct{}

// WithUserID 는 인증된 사용자 ID 를 컨텍스트에 넣는다. 미들웨어가 호출한다.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, userID)
}

// UserID 는 미들웨어가 주입한 인증 사용자 ID 를 돌려준다. 미인증이면 빈 문자열.
func UserID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}
