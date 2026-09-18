// 이 파일은 **「이 기기만 끊기」의 도메인 규칙**이다. Firestore 는 한 줄도 나오지 않는다 —
// 저장은 users 가 한다(account.go 에 적힌 것과 같은 이유로 방향은 users → auth 한쪽이다).
//
// 왜 만들었는가: 2026-09-19 에 상담 통화 녹음과 고객 개인정보가 든 폰을 택시에 두고
// 내렸다. 되찾았지만 그 순간 할 수 있는 일은 **비밀번호 변경(= 모든 기기 로그아웃)**
// 하나뿐이었다. 계정 단위 무효화(Account.TokenVersion)밖에 없었기 때문이다.
// 세션은 그 위에 「기기 단위」를 얹는다. 🔴 **대체하는 것이 아니다** — 비밀번호를 털렸을
// 때 전부 끊는 길은 그대로 있어야 한다.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrSessionNotFound 는 그런 세션이 없다는 뜻이다.
//
// 🔴 **호출부는 「남의 세션」도 이 에러로 받아 404 를 내야 한다.** 403 은 「그 id 가
// 존재한다」를 알려 주는 것과 같다(internal/calls/audio.go 의 load 와 같은 판단).
// 세션 id 는 목록에 그대로 나가는 값이라 남의 것을 찍어 볼 수 있다.
var ErrSessionNotFound = errors.New("auth: 세션을 찾을 수 없다")

// maxAccesses 는 세션 문서가 들고 있는 접속 기록의 최대 건수다.
//
// 🔴 **별도 컬렉션에 한 건씩 쌓지 않는다.** 토큰을 15분마다 갱신하면 기기 하나가 한 달에
// 2,880건이고, 쌓이기만 할 뿐 볼 일은 거의 없는데 쓰기 비용은 계속 나간다. 세션 문서 안에
// 배열로 최근 것만 두고 오래된 것을 밀어낸다 — 문서 하나의 크기가 이 상수로 묶인다.
//
// 쓰기 자체도 매 갱신마다 하지 않는다. shouldTouch 를 보라.
const maxAccesses = 10

// maxUserAgentBytes 는 저장하는 User-Agent 원문의 길이 상한이다.
// UA 는 클라이언트가 마음대로 정하는 값이라 상한이 없으면 문서 크기도 상한이 없다.
const maxUserAgentBytes = 512

// unknownDevice 는 User-Agent 로 아무것도 알아낼 수 없을 때의 표시다.
//
// 🔴 **모르면 모른다고 적는다.** 여기에 그럴듯한 기본값(「휴대폰」, 「Chrome」)을 넣으면
// 사용자는 목록에서 엉뚱한 줄을 끊고 정작 잃어버린 기기는 살려 둔 채 안심한다.
const unknownDevice = "알 수 없는 기기"

// kst 는 「날짜가 바뀌었는가」를 판정하는 기준 시간대다.
//
// time.LoadLocation("Asia/Seoul") 을 쓰지 않는 이유: 그것은 컨테이너 안의 tzdata 파일에
// 의존하고, 없으면 **에러가 아니라 UTC 로 조용히 떨어진다.** 그러면 날짜 경계가 아홉 시간
// 밀려서 접속 기록이 엉뚱한 날짜에 찍히는데 아무 로그도 남지 않는다.
// 한국은 서머타임이 없어 고정 오프셋으로 충분하다.
var kst = time.FixedZone("KST", 9*60*60)

// Access 는 접속 기록 한 건이다. 「언제, 어떤 기기 표시로 들어왔는가」가 전부다.
//
// ⚠️ IP 나 위치는 담지 않는다. 지금 이 기능이 답해야 하는 질문은 「이 줄이 잃어버린
// 폰인가」와 「끊었는데 진짜 끊겼는가」 둘뿐이고, 둘 다 IP 없이 답한다. 필요해지면
// 그때 더하되 무엇을 위해 더하는지 여기에 적어라.
type Access struct {
	At          time.Time
	DeviceLabel string
}

// Session 은 로그인 한 번으로 생기는 기기 하나다. `users/{uid}/sessions/{sid}` 에 산다.
type Session struct {
	ID string
	// DeviceLabel 은 User-Agent 로 **추정한** 기기 이름이다. DeviceLabel() 을 보라.
	DeviceLabel string
	// UserAgent 는 추정의 근거가 된 원문이다(잘려 있을 수 있다).
	// 추정이 빗나갔을 때 사람이 직접 읽어 보라고 남긴다.
	UserAgent string
	CreatedAt time.Time
	// LastSeenAt 은 마지막으로 **기록한** 접속 시각이다.
	//
	// ⚠️ **마지막으로 접속한 시각이 아니다.** 쓰기를 줄이려고 하루에 한두 번만
	// 갱신하므로(shouldTouch), 같은 날 안에서는 그날 처음 갱신한 시각에 머문다.
	// 날짜는 항상 맞고 시:분은 뒤처질 수 있다.
	//
	// 🔴 **이 값으로 「언제까지 쓸 수 있는가」를 계산하지 마라.** 뒤처진 값이라
	// 실제보다 **이른** 시각이 나오고, 사용자는 이미 끊겼다고 믿는데 그 기기는 아직
	// 쓰고 있는 상태가 된다. 그 계산은 AccessibleUntilAtMost 가 RevokedAt 으로 한다.
	LastSeenAt time.Time
	// RevokedAt 은 끊은 시각이다. 제로값이면 살아 있다.
	//
	// 🔴 **문서를 지우지 않고 시각을 남긴다.** 지우면 목록에서 사라져 「끊었는데 진짜
	// 끊긴 건가」에 답할 수 없고, 무엇보다 「없는 세션」과 「끊긴 세션」이 같은 상태가
	// 되어 버린다 — 그러면 없는 세션을 너그럽게 다루는 순간 끊기가 통째로 풀린다.
	RevokedAt time.Time
	// TokenVersion 은 이 세션이 만들어진 시점의 Account.TokenVersion 이다.
	//
	// 비밀번호를 바꾸면 계정의 버전이 올라 옛 세션의 리프레시가 전부 막힌다. 그런데
	// 세션 문서에는 아무 일도 일어나지 않으므로, 이 값이 없으면 **이미 죽은 세션이
	// 목록에 「살아 있음」으로 남는다.** 보안 화면이 거짓말을 하는 셈이라 버전을 함께
	// 들고 다니며 visible() 이 걸러 낸다.
	TokenVersion int
	// Accesses 는 최근 접속 기록이다. **최신이 앞**이고 최대 maxAccesses 건이다.
	Accesses []Access
}

// SessionStore 는 auth 가 세션 문서에 대해 필요로 하는 전부다. 구현은 users 다.
//
// 메서드 이름에 Session 을 붙인 것은 users.Store 가 AccountStore 와 이 인터페이스를
// **함께** 구현하기 때문이다. Get/Create 같은 이름은 이미 계정 쪽이 쓰고 있다.
type SessionStore interface {
	// CreateSession 은 세션 하나를 만들고 그 id 를 돌려준다.
	//
	// 🔴 실패하면 **로그인을 실패시켜야 한다.** 세션 없이 발급한 토큰은 첫 갱신에서
	// 거절되므로, 통과시키면 로그인은 성공한 것처럼 보이고 15분 뒤에 로그인 화면으로
	// 돌아간다 — 사용자에게는 원인 없는 고장으로 보인다(handler.go 의 login).
	CreateSession(ctx context.Context, userID string, s Session) (string, error)

	// GetSession 은 세션 하나를 읽는다. 없으면 ErrSessionNotFound.
	// 🔴 다른 사용자의 세션도 ErrSessionNotFound 여야 한다(그래서 userID 를 받는다).
	GetSession(ctx context.Context, userID, sessionID string) (Session, error)

	// ListSessions 는 최근 접속 순으로 최대 limit 건을 돌려준다.
	ListSessions(ctx context.Context, userID string, limit int) ([]Session, error)

	// TouchSession 은 마지막 접속과 접속 기록을 갱신한다.
	//
	// 🔴 **RevokedAt 을 건드리면 안 된다.** 갱신과 끊기가 같은 순간에 겹쳤을 때 이
	// 쓰기가 끊기를 되돌려 버리면, 사용자는 끊었다고 믿는데 그 기기는 계속 돈다.
	// 구현은 문서 전체를 Set 하지 말고 해당 필드만 Update 해야 한다.
	TouchSession(ctx context.Context, userID, sessionID string, lastSeenAt time.Time, deviceLabel string, accesses []Access) error

	// RevokeSession 은 세션에 끊은 시각을 남긴다. 없으면 ErrSessionNotFound.
	// 이미 끊긴 세션에 대해서는 호출부가 부르지 않는다 — 첫 끊은 시각을 덮어쓰면
	// 「언제까지 쓸 수 있는가」가 뒤로 밀려 사용자가 이미 지난 시각을 다시 기다린다.
	RevokeSession(ctx context.Context, userID, sessionID string, at time.Time) error
}

// AccessibleUntilAtMost 는 이 세션의 기기가 **늦어도 언제까지** 앱을 쓸 수 있는지다.
// 살아 있는 세션이면 (제로값, false) 다.
//
// 이 값이 이 기능의 핵심이다. 사용자가 「끊었는데 진짜 끊긴 건가」에 답하려면 숫자가
// 필요하다 — 앱은 이것으로 「이 기기는 00:49까지 쓸 수 있어요」라고 말한다.
//
// 계산은 **RevokedAt + AccessTokenTTL** 이다. LastSeenAt 이 아니다.
//   - 끊은 뒤로는 갱신이 막히므로, 그 기기가 쥘 수 있는 가장 새 액세스 토큰은 늦어도
//     RevokedAt 에 발급된 것이다. 거기에 액세스 토큰 수명을 더하면 **상한**이 된다.
//   - LastSeenAt 을 쓰면 안 되는 이유는 그 필드 주석에 있다(쓰기를 줄이느라 뒤처진다).
//     뒤처진 값으로 계산하면 실제보다 이른 시각이 나오고, 그것이 이 기능에서 가장
//     나쁜 실패다 — 아직 살아 있는 기기를 죽었다고 알려 주는 것이기 때문이다.
//
// ⚠️ **정확한 시각이 아니라 상한이다.** 그 기기가 마지막으로 갱신한 것이 한참 전이면
// 실제로는 훨씬 일찍 끊긴다. 또 끊기 요청과 갱신 요청이 같은 순간에 겹치면 갱신 쪽이
// 요청 왕복 시간만큼 늦게 토큰을 받을 수 있어, 그만큼은 이 상한도 어긋난다.
// 필드 이름에 AtMost 를 넣어 둔 것이 그 성격이다 — 「까지만 쓸 수 있다」가 아니라
// 「아무리 길어도 여기까지」다.
func (s Session) AccessibleUntilAtMost() (time.Time, bool) {
	if s.RevokedAt.IsZero() {
		return time.Time{}, false
	}
	return s.RevokedAt.Add(AccessTokenTTL), true
}

// visible 은 이 세션이 사용자에게 보여야 하는지다. 목록과 끊기가 **같은 판정을 쓴다** —
// 갈라 두면 목록에 없는 세션이 끊기에서는 200 을 받는 식으로 어긋난다.
//
//   - 계정의 TokenVersion 과 다르면 비밀번호 변경으로 이미 죽은 세션이다. 끊을 것이
//     남아 있지 않으므로 보여 주지 않는다(Session.TokenVersion 주석).
//   - 끊긴 세션은 상한 시각이 지날 때까지만 보여 준다. 그때까지는 「아직 쓸 수 있는
//     기기」라서 사용자가 알아야 하고, 지나면 아무것도 할 수 없는 줄이 목록만 채운다.
func visible(s Session, tokenVersion int, now time.Time) bool {
	if s.TokenVersion != tokenVersion {
		return false
	}
	until, revoked := s.AccessibleUntilAtMost()
	if !revoked {
		return true
	}
	return now.Before(until)
}

// shouldTouch 는 이번 갱신에서 세션 문서를 **다시 쓸지** 정한다.
//
// 🔴 **매 갱신마다 쓰지 않는다.** 액세스 토큰이 15분이라 기기 하나가 하루 최대 96번
// 갱신하는데, 그때마다 접속 기록 배열을 통째로 다시 쓰면 쌓이는 정보에 비해 쓰기만
// 나간다. 아래 조건이면 하루 한 번(자정을 넘겨 쓰면 두 번)으로 줄어들고, 그 결과
// **접속 기록 10건이 「최근 10일 중 쓴 날」이 되어 오히려 읽을 만해진다** — 15분 간격
// 기록 10건은 「방금 전」 열 줄일 뿐이라 아무것도 알려 주지 않는다.
//
// 쓰는 조건:
//   - 기록이 없다(제로값). 옛 문서이거나 만들다 만 문서다.
//   - 한국 날짜가 바뀌었다. 하루 한 번을 보장한다.
//   - 기기 표시가 달라졌다. 앱 업데이트나 브라우저 변경이라 사용자가 목록에서 알아볼
//     이름이 바뀐 것이고, 이걸 안 쓰면 목록이 옛 이름으로 굳는다.
//   - 시각이 거꾸로 갔다. 시계가 되돌려졌거나 저장된 값이 미래다 — 그대로 두면 날짜
//     비교가 영원히 참이 되어 **두 번 다시 쓰지 않는다.** 한 번 바로잡고 만다.
//
// ⚠️ 그 대가로 LastSeenAt 은 **날짜 단위로만 정확하다.** 그 성격은 필드 주석에 적혀
// 있고, 「언제까지 쓸 수 있는가」는 이 값에 기대지 않는다.
func shouldTouch(prev Session, now time.Time, deviceLabel string) bool {
	if prev.LastSeenAt.IsZero() {
		return true
	}
	if prev.DeviceLabel != deviceLabel {
		return true
	}
	if now.Before(prev.LastSeenAt) {
		return true
	}
	last := prev.LastSeenAt.In(kst)
	cur := now.In(kst)
	ly, lm, ld := last.Date()
	cy, cm, cd := cur.Date()
	return ly != cy || lm != cm || ld != cd
}

// pushAccess 는 기록을 앞에 붙이고 maxAccesses 를 넘는 꼬리를 잘라낸다.
// 최신이 앞이라 응답 순서와 저장 순서가 같다 — 한쪽만 뒤집으면 언젠가 반대로 나간다.
func pushAccess(prev []Access, a Access) []Access {
	next := make([]Access, 0, maxAccesses)
	next = append(next, a)
	for _, old := range prev {
		if len(next) == maxAccesses {
			break
		}
		next = append(next, old)
	}
	return next
}

// validSessionID 는 경로에서 받은 세션 id 가 Firestore 문서 id 로 쓸 만한지 본다.
// 저장소가 만드는 값은 20자 영숫자다. 여기서 막지 않으면 빈 문자열이나 `..` 같은 값이
// 그대로 문서 경로에 들어간다.
func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// truncateUserAgent 는 UA 원문을 상한까지 자른다. UTF-8 경계를 지켜 자른다 —
// 바이트로만 자르면 깨진 글자가 Firestore 에 들어가고 JSON 직렬화가 그걸 물고 넘어진다.
func truncateUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) <= maxUserAgentBytes {
		return ua
	}
	cut := ua[:maxUserAgentBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// ── 기기 이름 추정 ───────────────────────────────────────────────────────────

// nativeMarker 는 네이티브 껍데기가 UA 끝에 붙이는 표식이다.
// 앱이 `applicationNameForUserAgent={'NatureApp/' + APP_VERSION}` 으로 심는다
// (nature-app 의 src/components/web-shell.tsx).
//
// 🔴 앱에서 이 문자열을 바꾸면 여기도 같이 바꿔야 한다. 어긋나면 네이티브 앱이 그냥
// 브라우저로 보이고 — **아무 에러 없이** — 목록에 「Chrome · 안드로이드」로 뜬다.
const nativeMarker = "NatureApp/"

// DeviceLabel 은 User-Agent 로 기기 이름을 **추정한다.**
//
// ⚠️ **정확하지 않다.** UA 로 알 수 있는 것은 브라우저 계열과 OS 계열 정도이고,
// 기기 모델은 대개 알 수 없다(iOS 는 아예 감추고, 최신 크롬은 안드로이드 모델명을
// 지운다). 그래서 이 함수는 「Chrome · macOS」, 「네이처 앱 1.4.0 · 안드로이드」 정도에서
// 멈춘다.
//
// 🔴 **모르면 unknownDevice 다.** 여기에 그럴듯한 추측을 더하면 사용자가 목록에서
// 엉뚱한 줄을 끊는다 — 잃어버린 기기는 살아 있는데 끊었다고 믿는 상태가 가장 나쁘다.
// 원문이 필요하면 Session.UserAgent 를 보라, 그래서 함께 저장한다.
func DeviceLabel(userAgent string) string {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return unknownDevice
	}
	os := detectOS(ua)
	if v, ok := nativeVersion(ua); ok {
		name := "네이처 앱"
		if v != "" {
			name += " " + v
		}
		if os == "" {
			return name
		}
		return name + " · " + os
	}
	browser := detectBrowser(ua)
	switch {
	case browser != "" && os != "":
		return browser + " · " + os
	case browser != "":
		return browser
	case os != "":
		return os
	default:
		return unknownDevice
	}
}

// nativeVersion 은 UA 에서 네이티브 껍데기 표식을 찾아 버전을 떼어 낸다.
// 표식이 없으면 (,"" false) 다. 표식은 있는데 버전이 이상하면 버전만 비운다 —
// 껍데기라는 사실이 버전보다 중요하다.
func nativeVersion(ua string) (string, bool) {
	i := strings.Index(ua, nativeMarker)
	if i < 0 {
		return "", false
	}
	rest := ua[i+len(nativeMarker):]
	if j := strings.IndexAny(rest, " \t;)"); j >= 0 {
		rest = rest[:j]
	}
	for _, c := range rest {
		if !(c >= '0' && c <= '9') && c != '.' {
			return "", true
		}
	}
	return rest, true
}

// detectBrowser 는 UA 의 토큰으로 브라우저 계열을 고른다.
//
// 🔴 **순서가 곧 정확도다.** 크로뮴 계열은 전부 UA 에 `Chrome/` 을 달고 다니고 사파리를
// 흉내 내려고 `Safari/` 까지 붙인다. 좁은 것부터 보지 않으면 엣지도 웨일도 삼성인터넷도
// 전부 「Chrome」이 되고, iOS 의 크롬(CriOS)은 「Safari」가 된다.
func detectBrowser(ua string) string {
	for _, c := range []struct{ token, name string }{
		{"Edg/", "Edge"},
		{"EdgiOS/", "Edge"},
		{"OPR/", "Opera"},
		{"Whale/", "Whale"},
		{"SamsungBrowser/", "삼성 인터넷"},
		{"FxiOS/", "Firefox"},
		{"Firefox/", "Firefox"},
		{"CriOS/", "Chrome"},
		{"Chrome/", "Chrome"},
		{"Safari/", "Safari"},
	} {
		if strings.Contains(ua, c.token) {
			return c.name
		}
	}
	return ""
}

// detectOS 는 UA 에서 OS 계열을 고른다.
//
// ⚠️ 여기서도 순서가 있다. 안드로이드 UA 에는 `Linux` 가 함께 들어 있고, 아이패드는
// 데스크톱 모드에서 자기를 `Macintosh` 라고 소개한다 — 그래서 아이패드가 「macOS」로
// 보일 수 있다. 그 오인은 UA 만으로는 막을 수 없으므로 단정하지 말고, 사용자가 원문을
// 볼 수 있게 두는 것으로 대신한다(Session.UserAgent).
func detectOS(ua string) string {
	for _, c := range []struct{ token, name string }{
		{"Android", "안드로이드"},
		{"iPhone", "아이폰"},
		{"iPad", "아이패드"},
		{"Windows NT", "Windows"},
		{"Mac OS X", "macOS"},
		{"Macintosh", "macOS"},
		{"CrOS", "ChromeOS"},
		{"Linux", "Linux"},
	} {
		if strings.Contains(ua, c.token) {
			return c.name
		}
	}
	return ""
}
