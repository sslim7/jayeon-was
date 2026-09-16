// Package credentials 는 이메일·비밀번호 자격증명을 다루는 **공용** 헬퍼다.
//
// 어드민 계정(internal/admin)과 사용자 계정(internal/users)이 둘 다 이메일+비밀번호로
// 로그인하고, 둘 다 같은 함정을 안고 있다 — 이메일 정규화가 저장과 조회에서 어긋나는 것,
// bcrypt 의 72바이트 한계, 그리고 계정이 없을 때 비교를 건너뛰어 응답 시간으로 가입 여부가
// 새는 것.
//
// 🔴 **이 셋을 각 패키지가 따로 구현하면 반드시 갈라진다.** 특히 미끼 해시는 한쪽에만
// 있어도 그쪽이 아닌 다른 쪽에서 계정 열거가 열리는데, 두 구현이 비슷하게 생겨서 훑어봐서는
// 드러나지 않는다. 그래서 한자리에 둔다.
package credentials

import (
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// dummyHash 는 **없는 계정에 대해서도 bcrypt 비교를 한 번 돌리기 위한** 미끼다.
//
// 계정이 없을 때 즉시 401 을 돌려주면 응답이 수십 밀리초 빨라진다. bcrypt 는 일부러
// 느리게 설계된 함수라 그 차이가 밖에서 또렷하게 보이고, 그러면 로그인 API 하나로
// "이 이메일이 등록돼 있는가" 를 훑어낼 수 있다. 등록된 이메일 목록은 표적 피싱의
// 출발점이므로 그 정보를 시간으로 흘리지 않는다.
//
// 아무 비밀번호와도 맞지 않는 임의 값의 해시다. cost 는 bcrypt.DefaultCost(=10)으로
// **실제 계정과 같아야** 소요 시간이 같다 — 여기만 낮추면 미끼가 미끼 구실을 못 한다.
const dummyHash = "$2a$10$KcFvHV9FpL6YiD8PfrNs9esrwER0RrgDqXbg6fe5lx2ILe28BuNsq"

// NormalizeEmail 은 이메일을 저장·조회에 쓸 표준형으로 바꾼다.
//
// 소문자로 눕히고 앞뒤 공백을 턴다. 저장할 때와 로그인할 때 **반드시 같은 함수를**
// 거쳐야 한다 — 대문자로 만든 계정에 소문자로 로그인하면 "그런 계정 없음" 이 나오는데,
// 콘솔에서 문서를 보면 멀쩡히 있어서 원인을 찾기 어렵다.
//
// 이 값이 이메일 유일성 락 문서의 **문서 ID** 가 되기도 한다. 그래서 정규화가
// 어긋나면 같은 사람으로 계정이 둘 생긴다.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// HashPassword 는 비밀번호를 bcrypt 해시로 바꾼다.
//
// bcrypt 는 **72바이트를 넘는 입력을 조용히 자르지 않고 에러를 낸다**(x/crypto v0.53
// 이후). 그래서 호출부가 길이를 미리 막지 않아도 여기서 걸린다 — 조용히 잘리던 시절에는
// 73바이트 비밀번호를 쓴 사람이 앞 72바이트만 맞으면 로그인되는 문제가 있었다.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword 는 해시와 평문이 맞는지 본다.
//
// 🔴 **hash 가 비어 있으면 미끼 해시로 비교한다.** 계정이 없는 경우에도 호출부가 같은
// 코드를 지나가게 해서 응답 시간으로 가입 여부가 새지 않게 하는 것이 이 분기의 전부다.
// "해시가 없으면 어차피 실패인데" 하고 앞에서 잘라내면 그 방어가 사라진다.
//
// 시간을 맞추는 것만으로는 부족하다는 것도 함께 기억할 것 — 호출부가 "계정 없음" 과
// "비밀번호 틀림" 에 **다른 응답**을 내보내면 시간을 잴 것도 없이 본문이 알려 준다.
func VerifyPassword(hash, plain string) bool {
	if hash == "" {
		hash = dummyHash
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
