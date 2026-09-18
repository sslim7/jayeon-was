package recipients

import (
	"strings"
	"unicode"
)

// 「이름 또는 전화번호 뒷자리」 한 줄 검색 규칙 — 서버 쪽 한 곳.
//
// # 왜 여기 한 곳인가
//
// 앱의 검색칸은 전부 `src/lib/recipient-search.ts` 한 곳을 거친다. 서버에서 찾아 주는
// 곳(발송 이력 `GET /sms/history`, 통화 목록 `GET /calls`)이 각자 `strings.Contains(name, q)`
// 를 적어 두면 규칙이 바뀔 때 한쪽만 빠뜨리기 쉽고, **빠뜨려도 아무것도 깨지지 않아서**
// 아무도 모른다. 같은 검색어에 화면마다 다른 결과가 나오는 것이 가장 나쁜 실패 방식이다.
//
// # 규칙 (jayeon-app `src/lib/recipient-search.ts` 와 같은 결과여야 한다)
//
//   - 빈 검색어 → 전체.
//   - 이름에 검색어가 그대로 들어 있으면 → 통과(부분 일치, 대소문자 무시).
//   - 숫자만 친 검색어 → 이름과 전화번호 **둘 다** 본다(이름에 숫자가 든 사람이 있다).
//   - 글자와 숫자가 섞인 검색어 → 글자는 이름, 숫자는 번호를 가리킨다고 본다
//     (「김영 7649」 → 이름에 "김영" 이 있고 번호에 "7649" 가 있는 사람).
//     숫자만 떼어 번호를 맞히면 이름과 상관없는 사람이 올라오므로 그렇게 하지 않는다.
//   - 하이픈·공백·괄호·`+82` 같은 표기 차이는 **양쪽 모두** 걷어 내고 숫자만 비교한다.
//     그래서 "7649" 는 뒷자리로, "010-1234" 는 앞자리로 똑같이 걸린다(부분 일치).

// searchDigits 는 표기를 걷고 숫자만 남긴다.
//
// NormalizePhone 은 **완성된** 번호(010 + 8자리)만 받으므로 검색어처럼 잘린 입력에는 쓸 수
// 없다. 그래서 같은 규칙(국가번호 `+82` → `0`)만 느슨하게 옮겨 적는다. 저장된 번호는
// StoredPhone 이 다루는 `+8210…` 형태일 수 있는데, 그 경우도 여기서 함께 흡수된다.
func searchDigits(value string) string {
	var compact strings.Builder
	for _, r := range value {
		if unicode.IsSpace(r) || r == '-' || r == '(' || r == ')' || r == '.' {
			continue
		}
		compact.WriteRune(r)
	}
	local := compact.String()
	if strings.HasPrefix(local, "+82") {
		local = "0" + local[3:]
	}
	var digits strings.Builder
	for _, r := range local {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	return digits.String()
}

// searchLetters 는 검색어에서 번호 표기에 쓰이는 문자를 모두 뺀 나머지 — 이름 쪽 단서다.
func searchLetters(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' || unicode.IsSpace(r) {
			continue
		}
		if r == '-' || r == '+' || r == '(' || r == ')' || r == '.' {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// MatchesQuery 는 이름 또는 전화번호에 검색어가 걸리는지 본다. 빈 검색어는 전부 통과다.
func MatchesQuery(query, name, phone string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return true
	}
	lowerName := strings.ToLower(name)
	if strings.Contains(lowerName, strings.ToLower(trimmed)) {
		return true
	}
	digits := searchDigits(trimmed)
	if digits == "" {
		return false
	}
	// 숫자를 뺀 나머지가 이름 쪽 단서다. 남아 있으면 이름이 먼저 맞아야 번호를 본다.
	if letters := strings.ToLower(searchLetters(trimmed)); letters != "" && !strings.Contains(lowerName, letters) {
		return false
	}
	return strings.Contains(searchDigits(phone), digits)
}
