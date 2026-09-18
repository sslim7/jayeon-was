package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 토큰 종류. 클레임의 `token_use` 에 들어가며, 파싱 시 반드시 대조한다.
// (액세스 토큰을 리프레시로 재사용하는 등의 혼용을 막는다)
const (
	TokenUseAccess  = "access"
	TokenUseRefresh = "refresh"
)

// 기본 유효 기간.
//
// 🔴 **AccessTokenTTL 은 「기기 끊기」가 실제로 듣기까지의 시간이다.**
// 액세스 토큰은 서명과 만료만 보고 통과시킨다 — 서버가 저장하지도, 세션을 대조하지도
// 않는다(Claims.Sid 주석). 그래서 세션을 끊어도 **그 기기가 이미 쥐고 있는 액세스 토큰은
// 만료될 때까지 그대로 통한다.** 이 상수가 곧 그 구멍의 크기다.
//
// 1시간이던 것을 15분으로 줄인 것이 그 판단이다. 택시에 두고 내린 폰을 끊었는데 한 시간
// 동안 녹음과 고객 정보를 그대로 볼 수 있다면 「끊기」라고 부를 수 없다. 대가는 갱신
// 요청이 잦아지는 것인데, 앱은 401 을 받았을 때만 갱신하므로(nature-app 의 lib/api.ts)
// 늘어나는 것은 **실제로 앱을 쓰는 동안**뿐이고 동시에 터진 401 은 한 번으로 합쳐진다.
//
// 🔴 이 값을 다시 늘리려는 사람은 GET /auth/sessions 응답의 accessibleUntilAtMost 가
// 그만큼 뒤로 밀린다는 것을 먼저 보라. 그 숫자가 곧 사용자에게 보여 주는 「언제까지
// 쓸 수 있는가」다.
//
// RefreshTokenTTL 은 모바일 앱이 재로그인 없이 버티는 기간이다.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 90 * 24 * time.Hour
)

// ErrInvalidToken 은 서명 불일치·만료·종류 불일치를 모두 덮는다.
// 호출자에게 어느 쪽인지 알려 주지 않는 편이 안전하다.
var ErrInvalidToken = errors.New("auth: 유효하지 않은 토큰")

// Claims 는 WAS 가 발급하는 JWT 의 페이로드다.
// Subject 는 사용자 ID 다.
type Claims struct {
	jwt.RegisteredClaims
	TokenUse string `json:"token_use"`
	// Ver 는 발급 시점의 Account.TokenVersion 이다. **리프레시 토큰에만 싣는다.**
	//
	// 리프레시할 때 문서의 TokenVersion 과 대조해서, 비밀번호 변경으로 버전이 오른
	// 뒤에 남아 있던 옛 토큰을 전부 어긋나게 만드는 것이 이 값의 전부다. 즉 **계정
	// 단위로 전부 끊는 길**이고, 기기 하나만 끊는 길은 Sid 가 따로 맡는다.
	//
	// 🔴 **Sid 가 생겼다고 이 대조를 걷어내지 마라.** 비밀번호를 털렸을 때 「전부 끊기」는
	// 세션 목록을 열어 하나씩 누르는 것보다 확실해야 한다. 세션 검사는 이것을 대신하는
	// 것이 아니라 그 위에 얹는 것이다.
	//
	// 🔴 **액세스 토큰에는 싣지 않는다.** 실으면 매 요청마다 사용자 문서를 한 번 더
	// 읽어 대조해야 해서 Firestore 읽기 비용이 요청 수만큼 늘어난다. 그 대가로
	// **비밀번호를 바꿔도 이미 발급된 액세스 토큰은 최대 AccessTokenTTL(15분) 동안
	// 그대로 통한다** — 즉 이 장치는 "즉시 로그아웃" 이 아니라 "최대 15분 안에
	// 로그아웃" 이다. 비밀번호 변경 화면의 안내 문구가 그 사실과 맞아야 한다.
	// (Account.TokenVersion 주석에 같은 내용이 적혀 있다)
	//
	// omitempty 라 버전 0(=초기 계정)이면 클레임 자체가 빠지고, 파싱하면 다시 0 이
	// 된다. 대조는 그대로 성립한다.
	Ver int `json:"ver,omitempty"`
	// Sid 는 이 토큰이 속한 **세션(=로그인한 기기 하나)의 id** 다.
	// `users/{uid}/sessions/{sid}` 문서를 가리킨다.
	//
	// **두 종류 모두에 싣는다.** 다만 쓰임이 전혀 다르다.
	//
	//   - 리프레시: 🔴 갱신할 때 **세션 문서를 읽어 살아 있는지 확인한다.** 이것이
	//     「이 기기만 끊기」가 실제로 동작하는 유일한 지점이다(handler.go 의 refresh).
	//     세션이 없거나 끊겼으면 401 이고, 그 기기는 다시 로그인해야 한다.
	//
	//   - 액세스: ⚠️ **검증하지 않는다. 표시용이다.** GET /auth/sessions 가 "지금 이
	//     요청을 보낸 기기가 목록의 어느 줄인가" 를 가리기 위해서만 쓴다. 클레임을
	//     읽는 것은 공짜지만(서명 안에 이미 들어 있다) **여기에 세션 조회를 붙이면
	//     매 요청마다 Firestore 읽기가 한 번씩 늘어난다.** 그 유혹이 이 자리에 늘
	//     있으므로 못박아 둔다 — 액세스 토큰의 수명을 15분으로 줄인 것이 그 조회를
	//     하지 않기 위해 치르는 대가다(AccessTokenTTL 주석).
	//
	// 🔴 **이 값이 빈 리프레시 토큰은 거절한다.** 세션 장치가 생기기 전에 발급된
	// 토큰이라는 뜻이고, 받아 주면 「세션 검사를 통째로 건너뛰는 갈래」가 영영 남는다 —
	// 끊긴 기기가 옛 토큰을 들고 그 길로 들어온다. 거절 처리는 handler.go 의 refresh 다.
	Sid string `json:"sid,omitempty"`
}

// TokenIssuer 는 HS256 JWT 를 발급·검증한다. 시크릿은 env JWT_SECRET.
type TokenIssuer struct {
	secret []byte
	// now 는 테스트가 시간을 주입할 수 있도록 필드로 둔다. 발급과 검증이 같은
	// 시계를 보아야 만료 경계를 테스트할 수 있다.
	now func() time.Time
}

// NewTokenIssuer 는 시크릿으로 발급기를 만든다.
func NewTokenIssuer(secret string) *TokenIssuer {
	return &TokenIssuer{secret: []byte(secret), now: time.Now}
}

// IssueAccess 는 이 API 호출용 Bearer 토큰을 발급한다.
// 🔴 버전을 싣지 않는다 — 이유는 Claims.Ver 주석에 있다.
// sessionID 는 표시용으로만 실린다 — 이유는 Claims.Sid 주석에 있다.
func (t *TokenIssuer) IssueAccess(userID, sessionID string) (string, error) {
	return t.issue(userID, TokenUseAccess, AccessTokenTTL, 0, sessionID)
}

// IssueRefresh 는 토큰 세트 재발급용 토큰을 발급한다.
//
// ver 에는 발급 시점의 Account.TokenVersion 을 넣는다. 이 값을 빠뜨리면(0 으로 두면)
// 버전이 오른 계정의 리프레시가 전부 막히거나, 반대로 무효화가 아예 동작하지 않는다.
//
// 🔴 sessionID 를 빈 값으로 발급하면 **그 토큰은 첫 갱신에서 401 로 거절된다**
// (Claims.Sid 주석). 즉 로그인은 성공한 것처럼 보이는데 15분 뒤에 로그인 화면으로
// 되돌아간다. 부르는 쪽은 세션을 먼저 만들고 그 id 를 넘겨야 한다.
func (t *TokenIssuer) IssueRefresh(userID string, ver int, sessionID string) (string, error) {
	return t.issue(userID, TokenUseRefresh, RefreshTokenTTL, ver, sessionID)
}

func (t *TokenIssuer) issue(subject, use string, ttl time.Duration, ver int, sid string) (string, error) {
	now := t.now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		TokenUse: use,
		Ver:      ver,
		Sid:      sid,
	}
	// 같은 초에 재발급해도 리프레시 문자열이 바뀌도록 고유 ID를 싣는다.
	// 서버에 사용 이력을 저장하지 않으므로 단일 사용을 보장하는 장치는 아니다.
	if use == TokenUseRefresh {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", err
		}
		claims.ID = hex.EncodeToString(id[:])
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// parse 는 토큰을 검증하고 클레임을 통째로 돌려준다.
// want 와 종류가 다르거나 subject 가 비면 ErrInvalidToken 이다.
func (t *TokenIssuer) parse(token, want string) (Claims, error) {
	var claims Claims
	parser := jwt.NewParser(
		// 🔴 허용 알고리즘을 HS256 하나로 못박는다. 지정하지 않으면 `alg: none`
		// 토큰이나 공개키를 HMAC 키로 쓰는 알고리즘 혼동 공격이 열린다.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(t.now),
		jwt.WithExpirationRequired(),
	)
	if _, err := parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return t.secret, nil
	}); err != nil {
		return Claims{}, ErrInvalidToken
	}
	// 🔴 종류를 대조하지 않으면 액세스 토큰이 리프레시 자리에서 그대로 통한다.
	if claims.TokenUse != want || claims.Subject == "" {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

// Parse 는 토큰을 검증하고 subject 를 돌려준다.
// want 와 종류가 다르면 ErrInvalidToken 이다.
func (t *TokenIssuer) Parse(token, want string) (string, error) {
	claims, err := t.parse(token, want)
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

// ParseAccess 는 액세스 토큰을 검증하고 subject 와 **세션 id 를 함께** 돌려준다.
// 미들웨어가 쓴다.
//
// ⚠️ 여기서 돌려주는 세션 id 는 **검증된 값이 아니다.** 서명 안에 들어 있으니 위조는
// 아니지만, 그 세션이 아직 살아 있는지는 보지 않는다(Claims.Sid 주석). 「지금 이 기기」를
// 표시하는 것 말고 다른 판단에 쓰지 마라 — 끊긴 세션의 액세스 토큰도 만료 전까지는 이
// 값을 그대로 달고 들어온다.
//
// 세션 장치 이전에 발급된 액세스 토큰에는 이 값이 없어 빈 문자열이 된다. 그 토큰은
// 늦어도 AccessTokenTTL 안에 사라지고, 그때까지는 세션 목록에서 「지금 이 기기」 표시만
// 빠진다 — 요청이 막히지는 않는다.
func (t *TokenIssuer) ParseAccess(token string) (string, string, error) {
	claims, err := t.parse(token, TokenUseAccess)
	if err != nil {
		return "", "", err
	}
	return claims.Subject, claims.Sid, nil
}

// ParseRefresh 는 리프레시 토큰을 검증하고 subject 와 **버전·세션 id 를 함께** 돌려준다.
//
// 세 값을 한 번에 주는 전용 함수로 둔 것은, 호출부가 Parse 로 subject 만 받아 가서
// 버전이나 세션 대조를 건너뛰는 일을 막기 위해서다. 그렇게 되면 비밀번호를 바꿔도, 기기를
// 끊어도 옛 리프레시 토큰이 계속 도는데 **아무 에러도 나지 않아** 밖에서는 드러나지 않는다.
//
// 🔴 세션 id 가 빈 문자열이면 세션 장치 이전에 발급된 토큰이다. 호출부가 **반드시 거절**
// 해야 한다 — 그 판단과 로그는 handler.go 의 refresh 에 있다.
func (t *TokenIssuer) ParseRefresh(token string) (string, int, string, error) {
	claims, err := t.parse(token, TokenUseRefresh)
	if err != nil {
		return "", 0, "", err
	}
	return claims.Subject, claims.Ver, claims.Sid, nil
}
