package recipients

import "testing"

// 앱(`src/lib/recipient-search.ts`)과 **같은 결과**여야 한다. 한쪽만 고치면 같은 검색어에
// 화면마다 다른 결과가 나오는데, 앱에서는 "결과가 없네" 로만 보여 아무도 눈치채지 못한다.
func TestMatchesQuery(t *testing.T) {
	const name, phone = "홍길동", "01012347649"
	for _, c := range []struct {
		why   string
		q     string
		name  string
		phone string
		want  bool
	}{
		{"빈 검색어는 전체", "", name, phone, true},
		{"공백뿐인 검색어도 전체", "   ", name, phone, true},
		{"이름 부분 일치", "길동", name, phone, true},
		{"이름 대소문자 무시", "KIM", "kim철수", phone, true},
		{"전화번호 뒷4자리", "7649", name, phone, true},
		{"전화번호 앞자리", "0101234", name, phone, true},
		{"하이픈 표기 흡수", "1234-7649", name, phone, true},
		{"공백 표기 흡수", "1234 7649", name, phone, true},
		{"검색어의 +82 표기", "+821012347649", name, phone, true},
		{"저장된 번호의 +82 표기", "7649", name, "+821012347649", true},
		{"괄호·점 표기 흡수", "(010).1234", name, phone, true},
		{"이름도 번호도 아니면 불일치", "9999", name, phone, false},
		{"이름만 다른 사람", "김철수", name, phone, false},
		{"이름에 숫자가 들어 있으면 이름으로도 걸린다", "2호", "김2호점", phone, true},
		{"글자+숫자는 이름과 번호가 모두 맞아야 한다", "홍길 7649", name, phone, true},
		{"글자가 다르면 번호가 맞아도 불일치", "김철 7649", name, phone, false},
		{"숫자가 다르면 이름이 맞아도 불일치", "홍길 1111", name, phone, false},
		{"숫자가 없는 검색어는 번호를 보지 않는다", "-", name, phone, false},
		{"번호가 비어 있어도 터지지 않는다", "7649", name, "", false},
	} {
		if got := MatchesQuery(c.q, c.name, c.phone); got != c.want {
			t.Errorf("%s: MatchesQuery(%q, %q, %q) = %v, want %v", c.why, c.q, c.name, c.phone, got, c.want)
		}
	}
}
