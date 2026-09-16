package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// TestRedact 는 비밀번호 계열 값이 활동 로그에 평문으로 남지 않는지 못 박는다.
//
// 이 검사가 무너지면 어드민 비밀번호가 Firestore 에 영구히 쌓이고, 로그를 지우기 전에는
// 되돌릴 수 없다. **키 이름에 password 가 들어가면 전부 가린다** 는 규칙이므로 목록에
// 없는 새 필드(oldPassword 등)도 자동으로 가려져야 한다.
func TestRedact(t *testing.T) {
	in := map[string]any{
		"email":        "admin@jayeon.kr",
		"password":     "hunter2",
		"newPassword":  "hunter3",
		"passwordHash": "$2a$10$abc",
		"oldPassword":  "hunter1", // 목록에 없는 새 필드도 가려져야 한다
		"PASSWORD":     "대문자도",
		"rememberMe":   true,
		"nested": map[string]any{
			"password": "깊은 곳도",
			"name":     "홍길동",
		},
		"list": []any{
			map[string]any{"password": "배열 안도"},
			"평범한 값",
		},
	}
	want := map[string]any{
		"email":        "admin@jayeon.kr",
		"password":     redactedValue,
		"newPassword":  redactedValue,
		"passwordHash": redactedValue,
		"oldPassword":  redactedValue,
		"PASSWORD":     redactedValue,
		"rememberMe":   true,
		"nested": map[string]any{
			"password": redactedValue,
			"name":     "홍길동",
		},
		"list": []any{
			map[string]any{"password": redactedValue},
			"평범한 값",
		},
	}

	got := redact(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redact 결과가 다르다:\n got = %#v\nwant = %#v", got, want)
	}
	// 원본을 건드리지 않아야 한다 — 핸들러가 같은 맵을 다시 쓸 수 있다.
	if in["password"] != "hunter2" {
		t.Fatalf("원본이 훼손됐다: %v", in["password"])
	}
}

// TestIsSecretKey 는 가림 규칙의 경계를 못 박는다.
func TestIsSecretKey(t *testing.T) {
	secret := []string{"password", "newPassword", "passwordHash", "PASSWORD", "adminSecret", "accessToken"}
	plain := []string{"email", "name", "isAdmin", "permissions", "pass", "word"}
	// 규칙이 넓어서 이 무해한 키까지 걸린다. 감수한 대가라는 것을 못 박아 둔다 —
	// 예외를 만들려는 다음 사람이 여기서 의도를 읽게 한다(audit.go isSecretKey 주석).
	if !isSecretKey("mustChangePassword") {
		t.Error("isSecretKey(\"mustChangePassword\") = false — 넓은 규칙이 좁아졌다")
	}

	for _, k := range secret {
		if !isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = false, 가려야 한다", k)
		}
	}
	for _, k := range plain {
		if isSecretKey(k) {
			t.Errorf("isSecretKey(%q) = true, 가리면 안 된다", k)
		}
	}
}

// TestBodyForAudit 는 바디를 로그용 map 으로 바꾸는 규칙을 못 박는다.
func TestBodyForAudit(t *testing.T) {
	t.Run("비밀번호를 가린 객체", func(t *testing.T) {
		got := bodyForAudit([]byte(`{"email":"a@b.kr","password":"hunter2"}`))
		if got["email"] != "a@b.kr" {
			t.Errorf("email = %v", got["email"])
		}
		if got["password"] != redactedValue {
			t.Errorf("password = %v, want %s", got["password"], redactedValue)
		}
	})
	t.Run("빈 바디", func(t *testing.T) {
		if got := bodyForAudit(nil); len(got) != 0 {
			t.Errorf("빈 바디 = %v, want 빈 map", got)
		}
	})
	t.Run("JSON 이 아니면 빈 map", func(t *testing.T) {
		if got := bodyForAudit([]byte("not json")); len(got) != 0 {
			t.Errorf("깨진 JSON = %v, want 빈 map", got)
		}
	})
	t.Run("객체가 아닌 JSON 도 빈 map", func(t *testing.T) {
		// details.body 는 언제나 객체여야 한다. 배열이 들어가면 SPA 상세 보기가 깨진다.
		if got := bodyForAudit([]byte(`[1,2,3]`)); len(got) != 0 {
			t.Errorf("배열 = %v, want 빈 map", got)
		}
	})
	t.Run("너무 크면 표시만 남긴다", func(t *testing.T) {
		big := make([]byte, maxAuditBodyBytes+1)
		got := bodyForAudit(big)
		if got["_truncated"] != true {
			t.Errorf("_truncated = %v, want true", got["_truncated"])
		}
	})
}

// TestKSTDateBoundary 는 활동 로그 날짜 필터의 KST 경계를 못 박는다.
//
// 여기가 틀어지면 오전에 한 일이 전날 조회에 잡히고 저녁에 한 일이 사라진다.
// 조회는 정상적으로 200 을 돌려주므로 어디에도 흔적이 남지 않는다.
func TestKSTDateBoundary(t *testing.T) {
	from, err := parseDateFrom("2026-09-03")
	if err != nil {
		t.Fatalf("parseDateFrom: %v", err)
	}
	// 2026-09-03 00:00 KST = 2026-09-02 15:00 UTC
	if want := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC); !from.Equal(want) {
		t.Fatalf("dateFrom = %s, want %s", from.UTC(), want)
	}

	to, err := parseDateTo("2026-09-03")
	if err != nil {
		t.Fatalf("parseDateTo: %v", err)
	}
	// dateTo 는 그날을 **포함**하므로 경계는 다음 날 00:00 KST = 2026-09-03 15:00 UTC 다.
	if want := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC); !to.Equal(want) {
		t.Fatalf("dateTo = %s, want %s", to.UTC(), want)
	}

	// 그날 23:59:59 KST 의 기록이 [from, to) 안에 들어와야 한다.
	lastMoment := time.Date(2026, 9, 3, 23, 59, 59, 0, kst)
	if lastMoment.Before(from) || !lastMoment.Before(to) {
		t.Fatalf("그날 마지막 순간(%s)이 범위 [%s, %s) 밖이다", lastMoment, from.UTC(), to.UTC())
	}
	// 그날 00:00:00 KST 도 포함이어야 한다.
	firstMoment := time.Date(2026, 9, 3, 0, 0, 0, 0, kst)
	if firstMoment.Before(from) {
		t.Fatalf("그날 첫 순간(%s)이 from(%s) 앞이다", firstMoment, from.UTC())
	}
	// 다음 날 00:00:00 KST 는 **빠져야** 한다.
	nextDay := time.Date(2026, 9, 4, 0, 0, 0, 0, kst)
	if nextDay.Before(to) {
		t.Fatalf("다음 날 첫 순간(%s)이 to(%s) 안에 들어왔다", nextDay, to.UTC())
	}

	if _, err := parseDateFrom("2026-13-01"); err == nil {
		t.Fatal("없는 달을 받아들였다")
	}
	if _, err := parseDateTo("20260903"); err == nil {
		t.Fatal("형식이 다른 날짜를 받아들였다")
	}
}

// TestAuditLogMatches 는 부분검색 대상 필드를 못 박는다.
func TestAuditLogMatches(t *testing.T) {
	l := auditLog{
		Actions:    "POST /admin/admins",
		AdminName:  "홍길동",
		AdminEmail: "admin@jayeon.kr",
		IPAddress:  "203.0.113.7",
		Details:    map[string]any{"body": map[string]any{"name": "김철수"}},
	}
	hits := []string{"admins", "POST", "홍길", "jayeon.kr", "203.0", "ADMIN/ADMINS"}
	for _, needle := range hits {
		if !l.matches(needle) {
			t.Errorf("matches(%q) = false, want true", needle)
		}
	}
	misses := []string{"audit-logs", "이순신", "198.51"}
	for _, needle := range misses {
		if l.matches(needle) {
			t.Errorf("matches(%q) = true, want false", needle)
		}
	}
	// details.body 는 검색 대상이 아니다 — 긴 본문에 우연히 걸린 줄이 쏟아지면
	// 무엇 때문에 걸렸는지 화면에서 보이지 않는다.
	if l.matches("김철수") {
		t.Error("details.body 가 검색됐다")
	}
}

// TestParseAuditFilter 는 쿼리 해석 규칙과 기본값을 못 박는다.
func TestParseAuditFilter(t *testing.T) {
	t.Run("기본값", func(t *testing.T) {
		f, msg := parseAuditFilter(url.Values{})
		if msg != "" {
			t.Fatalf("빈 쿼리가 거절됐다: %s", msg)
		}
		if f.Page != 1 || f.PageSize != defaultAuditPageSize || !f.Desc {
			t.Fatalf("기본값이 다르다: %+v", f)
		}
		if f.From != nil || f.To != nil {
			t.Fatalf("날짜 기본값이 있다: %+v", f)
		}
	})

	t.Run("정상 값", func(t *testing.T) {
		f, msg := parseAuditFilter(url.Values{
			"page":      {"3"},
			"pageSize":  {"100"},
			"dateFrom":  {"2026-09-01"},
			"dateTo":    {"2026-09-03"},
			"adminId":   {"admin-1"},
			"target":    {TargetAdmin},
			"search":    {"로그인"},
			"sortOrder": {"asc"},
		})
		if msg != "" {
			t.Fatalf("정상 쿼리가 거절됐다: %s", msg)
		}
		if f.Page != 3 || f.PageSize != 100 || f.Desc {
			t.Fatalf("해석 결과가 다르다: %+v", f)
		}
		if f.AdminID != "admin-1" || f.Target != TargetAdmin || f.Search != "로그인" {
			t.Fatalf("필터 값이 다르다: %+v", f)
		}
	})

	bad := []struct {
		name string
		q    url.Values
	}{
		{"page 0", url.Values{"page": {"0"}}},
		{"page 가 숫자가 아님", url.Values{"page": {"first"}}},
		{"pageSize 상한 초과", url.Values{"pageSize": {"101"}}},
		{"pageSize 0", url.Values{"pageSize": {"0"}}},
		{"dateFrom 형식 오류", url.Values{"dateFrom": {"2026/09/01"}}},
		{"dateTo 형식 오류", url.Values{"dateTo": {"어제"}}},
		{"sortOrder 오타", url.Values{"sortOrder": {"descending"}}},
		{"뒤집힌 범위", url.Values{"dateFrom": {"2026-09-05"}, "dateTo": {"2026-09-01"}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, msg := parseAuditFilter(tc.q); msg == "" {
				t.Fatal("잘못된 값이 통과했다")
			}
		})
	}

	// 같은 날 하루만 보는 조회는 통과해야 한다(from < to 이므로).
	t.Run("dateFrom 과 dateTo 가 같은 날", func(t *testing.T) {
		if _, msg := parseAuditFilter(url.Values{
			"dateFrom": {"2026-09-03"}, "dateTo": {"2026-09-03"},
		}); msg != "" {
			t.Fatalf("하루 조회가 거절됐다: %s", msg)
		}
	})
}

// TestClientIP 는 Cloud Run 뒤에서 원 요청자를 집어내는 규칙을 못 박는다.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		xff        string
		remoteAddr string
		want       string
	}{
		{"XFF 첫 항목", "203.0.113.7, 10.0.0.1, 10.0.0.2", "10.0.0.9:5000", "203.0.113.7"},
		{"XFF 한 개", "203.0.113.7", "10.0.0.9:5000", "203.0.113.7"},
		{"XFF 없으면 RemoteAddr(포트 제거)", "", "127.0.0.1:54321", "127.0.0.1"},
		{"XFF 가 공백뿐이면 RemoteAddr", " ", "127.0.0.1:54321", "127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/admin/admins", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestActionOf 는 actions 문자열 모양을 못 박는다. 쿼리가 섞이면 감사 화면의
// 검색·집계가 같은 동작을 서로 다른 줄로 센다.
func TestActionOf(t *testing.T) {
	r := httptest.NewRequest(http.MethodPatch, "/admin/admins/abc?foo=bar", nil)
	if got, want := actionOf(r), "PATCH /admin/admins/abc"; got != want {
		t.Fatalf("actionOf = %q, want %q", got, want)
	}
}

// TestStatusRecorder 는 응답 상태 가로채기를 못 박는다.
// WriteHeader 를 부르지 않고 Write 만 하는 핸들러는 200 이어야 한다.
func TestStatusRecorder(t *testing.T) {
	t.Run("명시적 상태", func(t *testing.T) {
		rec := newStatusRecorder(httptest.NewRecorder())
		rec.WriteHeader(http.StatusConflict)
		if rec.status != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.status)
		}
	})
	t.Run("WriteHeader 없이 Write 만", func(t *testing.T) {
		rec := newStatusRecorder(httptest.NewRecorder())
		if _, err := rec.Write([]byte("ok")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if rec.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.status)
		}
	})
}
