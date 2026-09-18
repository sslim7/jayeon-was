package auth

import (
	"context"
	"errors"
	"time"
)

// 이 파일은 **auth 와 users 사이의 계약**이다.
//
// 사용자 문서(`users/{userId}`)의 소유자는 internal/users 다. 그런데 로그인·리프레시·
// 비밀번호 변경은 auth 의 일이라 그 문서를 읽고 써야 한다. auth 가 users 를 직접
// import 하면, users 가 `auth.UserID(ctx)` 를 쓰는 순간 import 순환이 된다.
//
// 그래서 **필요한 것의 모양만 auth 가 선언하고, 구현은 users 가 한다.** 방향이 한쪽
// (users → auth)으로만 흐르므로 순환이 생기지 않고, auth 는 Firestore 문서 구조를
// 몰라도 된다. 배선은 main.go 가 한다.

// ErrAccountNotFound 는 그런 계정이 없다는 뜻이다.
//
// 🔴 **호출부는 이 에러를 받아도 "계정이 없다" 를 응답으로 드러내면 안 된다.**
// 비밀번호가 틀린 경우와 글자 하나까지 같은 응답을 내보내야 한다(handler 쪽 주석 참고).
// 여기서 구분된 에러를 주는 것은 감사 로그와 분기를 위한 것이지 응답을 가르기 위한 것이 아니다.
var ErrAccountNotFound = errors.New("auth: 계정을 찾을 수 없다")

// Account 는 인증에 필요한 만큼의 사용자 계정이다.
//
// ⚠ PasswordHash 가 들어 있다. **이 타입을 그대로 응답에 싣지 마라.**
type Account struct {
	UserID       string
	Email        string
	PasswordHash string
	UserName     string
	// IsActive 가 거짓이면 로그인도 리프레시도 막는다.
	IsActive bool
	// MustChangePassword 는 초대 시 발급한 임시 비밀번호를 아직 안 바꿨다는 뜻이다.
	// 로그인 응답과 GET /users/me 가 이 값을 그대로 내보내고, 앱은 참이면 비밀번호
	// 변경 화면으로 보낸다.
	MustChangePassword bool
	// TokenVersion 은 **리프레시 토큰을 무효화하는 유일한 수단**이다.
	//
	// 리프레시 토큰은 서버에 저장되지 않는 순수 JWT 라, 이 값이 없으면 비밀번호를 바꿔도
	// 이미 나간 토큰이 TTL(90일) 동안 그대로 살아 있다. 발급한 토큰에 이 값을 실어 두고
	// 리프레시할 때 문서의 값과 대조하면, 비밀번호를 바꾸는 순간(=이 값이 오르는 순간)
	// 옛 토큰이 전부 어긋나 401 이 된다.
	//
	// 🔴 **액세스 토큰에는 싣지 않는다.** 매 요청마다 문서를 한 번 더 읽어야 해서 비용이
	// 맞지 않는다. 그 대가로 비밀번호를 바꿔도 **이미 발급된 액세스 토큰은 최대 15분
	// (AccessTokenTTL) 더 유효하다.** 즉 이 장치는 "즉시 로그아웃" 이 아니라 "최대 15분
	// 안에 로그아웃" 이다. 비밀번호 변경 화면의 안내 문구가 그렇게 적혀 있어야 한다.
	//
	// 이 값은 **계정 전체**를 끊는다. 기기 하나만 끊는 길은 세션이 따로 맡는다
	// (session.go). 🔴 세션이 생겼다고 이 장치를 걷어내지 마라 — 비밀번호를 털렸을 때
	// 「전부 끊기」는 목록을 열어 하나씩 누르는 것보다 확실해야 한다.
	TokenVersion int
}

// AccountStore 는 auth 가 사용자 문서에 대해 필요로 하는 전부다.
//
// 여기에 메서드를 더할 때는 **정말 인증 흐름에 필요한지** 먼저 보라. 프로필 수정처럼
// 인증과 무관한 것은 users 가 자기 Store 로 직접 하면 되고, 그것까지 이 인터페이스에
// 넣으면 auth 가 사용자 도메인 전체를 알게 된다.
type AccountStore interface {
	// FindByEmail 은 정규화된 이메일로 계정을 찾는다.
	// 없으면 ErrAccountNotFound 다.
	//
	// 🔴 호출부는 없을 때에도 비밀번호 비교를 한 번 돌려야 한다
	// (credentials.VerifyPassword 가 빈 해시를 미끼로 받는 이유).
	FindByEmail(ctx context.Context, email string) (Account, error)

	// Get 은 userId 로 계정을 읽는다. 없으면 ErrAccountNotFound 다.
	// 리프레시가 TokenVersion 과 IsActive 를 확인하는 데 쓴다.
	Get(ctx context.Context, userID string) (Account, error)

	// SetPassword 는 비밀번호 해시를 바꾸고 **TokenVersion 을 하나 올린다.**
	// MustChangePassword 는 거짓이 된다.
	//
	// 🔴 이 셋은 **한 번의 쓰기**여야 한다. 나눠 쓰면 해시만 바뀌고 버전이 안 오른 상태가
	// 생길 수 있는데, 그러면 비밀번호를 바꿨는데도 옛 리프레시 토큰이 계속 도는 상태가
	// 되고 아무 에러도 남지 않는다.
	SetPassword(ctx context.Context, userID, passwordHash string, now time.Time) error
}
