package admin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
)

// AuditCollection 은 활동 로그 문서가 사는 곳이다.
const AuditCollection = "audit_logs"

// 활동 로그의 targets 값. 도메인 하나에 문자열 하나다.
//
// 상수로 두는 이유는 화면의 「대상」 필터가 이 값과 정확히 일치해야 하기 때문이다 —
// 한 핸들러가 "admins" 라고 적으면 그 줄만 필터에서 영영 빠지는데, 목록에는 보이므로
// 아무도 이상하다고 느끼지 못한다.
//
// 지금 있는 둘은 **이 패키지 자신이 쓰는 값**이다. 도메인 어드민 메뉴를 만드는 사람이
// 자기 targets 상수를 여기에 더한다(권한 키와 달리 targets 는 SPA 의 네비게이션과
// 같을 필요가 없다 — 감사 화면의 필터 목록과만 맞으면 된다).
const (
	TargetAdmin = "admin"
	TargetAuth  = "auth"
)

// 로그인 기록의 actions 값. 다른 기록은 "METHOD 경로" 지만 로그인만 사람 말이다 —
// 감사 화면에서 가장 자주 훑는 줄이고, `POST /auth/admin.login` 은 성공과 실패가
// 같은 문자열이라 구분되지 않는다.
const (
	ActionLogin       = "어드민 로그인"
	ActionLoginFailed = "어드민 로그인 실패"
)

// redactedValue 는 가려진 값 자리에 넣는 표시다. 키를 통째로 지우지 않는 이유는
// "무엇을 보냈는가" 는 감사에 필요하고 "그 값이 무엇이었나" 만 필요 없기 때문이다.
const redactedValue = "[REDACTED]"

// maxAuditBodyBytes 는 활동 로그에 담을 요청 바디의 상한이다.
//
// 긴 본문을 받는 도메인 엔드포인트가 생기면 바디를 그대로 담는 순간 감사 로그 한 줄이
// 본문 사본이 된다. 넘치면 담지 않고 표시만 남긴다 — 로그가 원본 저장소가 되면
// 지우기도 어렵고 Firestore 문서 1MB 상한에도 걸린다.
const maxAuditBodyBytes = 8 << 10

// kst 는 활동 로그 날짜 필터의 기준 시간대다.
//
// 화면에서 고른 「2026-09-03」 은 **한국 날짜**다. 서버가 UTC 로 해석하면 그 하루가
// 실제로는 09-03 09:00 ~ 09-04 09:00 KST 가 되어, 오전에 한 일이 전날 조회에 잡히고
// 저녁에 한 일이 사라진다. 시간대 하나를 고정으로 박는 이유는 운영 인원이 전부 한국에
// 있고 화면에도 KST 로 그리기 때문이다.
//
// time.LoadLocation("Asia/Seoul") 을 쓰지 않는 것은 배포 이미지(distroless)에 tzdata 가
// 없을 수 있어서다. KST 는 서머타임이 없는 고정 +09:00 이라 FixedZone 으로 충분하다.
var kst = time.FixedZone("KST", 9*60*60)

// auditEntry 는 남길 활동 로그 한 줄이다.
type auditEntry struct {
	AdminID    string
	AdminName  string
	AdminEmail string
	Actions    string
	Targets    string
	Status     int
	Body       map[string]any
	IPAddress  string
}

// auditDoc 은 Firestore 문서다. 활동 로그 응답 필드와 이름이 같아야 한다.
type auditDoc struct {
	AdminID    string         `firestore:"adminId"`
	AdminName  string         `firestore:"adminName"`
	AdminEmail string         `firestore:"adminEmail"`
	Actions    string         `firestore:"actions"`
	Targets    string         `firestore:"targets"`
	Details    map[string]any `firestore:"details"`
	IPAddress  string         `firestore:"ipAddress"`
	CreatedAt  time.Time      `firestore:"createdAt"`
}

// auditStore 는 활동 로그 컬렉션에 대한 읽기·쓰기다.
type auditStore struct {
	fs *firestore.Client
}

func newAuditStore(fs *firestore.Client) *auditStore { return &auditStore{fs: fs} }

// write 는 활동 로그 한 줄을 남긴다.
//
// **여기의 실패는 요청을 실패시키지 않는다** — 호출부가 에러를 로그로만 흘린다.
// 감사 기록을 남기지 못했다고 이미 성공한 쓰기를 500 으로 되돌리면, 되돌릴 수
// 없는 쓰기가 이미 끝난 뒤라 화면과 데이터가 어긋난다.
func (s *auditStore) write(ctx context.Context, e auditEntry, now time.Time) error {
	body := e.Body
	if body == nil {
		body = map[string]any{}
	}
	_, _, err := s.fs.Collection(AuditCollection).Add(ctx, map[string]any{
		"adminId":    e.AdminID,
		"adminName":  e.AdminName,
		"adminEmail": e.AdminEmail,
		"actions":    e.Actions,
		"targets":    e.Targets,
		"details": map[string]any{
			"status": e.Status,
			"body":   body,
		},
		"ipAddress": e.IPAddress,
		"createdAt": now,
	})
	return err
}

// ──────────────────────────────────────────────────────────────
// 비밀번호 가리기
// ──────────────────────────────────────────────────────────────

// redact 는 요청 바디에서 비밀번호 계열 값을 [REDACTED] 로 바꾼다.
//
// **키 이름에 password 가 들어가면 전부 가린다.** `password`·`newPassword`·`passwordHash`
// 를 일일이 나열하면 다음에 `oldPassword` 같은 필드가 생기는 날 그 값이 그대로 감사
// 로그에 평문으로 쌓인다 — 목록을 유지보수하는 쪽에 기대지 않고 규칙으로 막는다.
//
// 중첩 map 과 배열도 따라 들어간다. 지금은 바디가 한 겹뿐이지만, 언젠가 중첩 구조가
// 들어왔을 때 조용히 새어 나가는 쪽이 훨씬 나쁘다.
func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecretKey(k) {
				out[k] = redactedValue
				continue
			}
			out[k] = redact(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redact(val)
		}
		return out
	default:
		return v
	}
}

// isSecretKey 는 값을 가려야 하는 키인지다. 대소문자를 가리지 않는다 —
// JSON 키는 camelCase 지만 언젠가 다른 표기가 섞여도 규칙이 뚫리면 안 된다.
//
// ⚠ 규칙이 넓어서 **`mustChangePassword` 같은 무해한 불리언까지 [REDACTED] 로 덮인다.**
// 알고 감수한 대가다 — 예외 목록을 두기 시작하면 그 목록이 곧 새는 구멍이 되고,
// 감사 로그에서 잃는 것은 "true 였는지 false 였는지" 하나뿐인데 잘못 새면 잃는 것은
// 어드민 비밀번호다. 값이 필요하면 응답 문서(admins/{id})를 보면 된다.
func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "password") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "token")
}

// bodyForAudit 은 요청 바디 바이트를 활동 로그에 담을 map 으로 만든다.
//
// JSON 이 아니거나 객체가 아니면 빈 map 을 돌려준다 — 감사 로그의 details.body 는
// 언제나 객체여야 하고, 여기서 문자열이나 배열이 들어가면 SPA 의 상세 보기가 깨진다.
func bodyForAudit(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	if len(raw) > maxAuditBodyBytes {
		// 원문 대신 왜 없는지를 남긴다. 빈 map 만 남기면 "바디가 없었다" 와 구분되지 않는다.
		return map[string]any{"_truncated": true, "_bytes": len(raw)}
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return map[string]any{}
	}
	redacted, _ := redact(parsed).(map[string]any)
	if redacted == nil {
		return map[string]any{}
	}
	return redacted
}

// ──────────────────────────────────────────────────────────────
// 응답 상태 가로채기 · 클라이언트 IP
// ──────────────────────────────────────────────────────────────

// statusRecorder 는 핸들러가 쓴 응답 상태를 기억한다.
//
// details.status 를 남기려면 응답이 몇으로 나갔는지 알아야 하는데, http.ResponseWriter
// 에는 그것을 되읽는 방법이 없다. 그래서 WriteHeader 를 한 겹 가로챈다.
//
// 핸들러가 WriteHeader 를 부르지 않고 Write 만 하면 net/http 가 200 을 쓴다 —
// 그 경우를 기본값 200 으로 맞춰 둔다.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// clientIP 는 요청을 보낸 쪽의 주소다.
//
// **X-Forwarded-For 의 첫 항목이 정답이다.** 이 서버는 Cloud Run 뒤에 있어 RemoteAddr
// 은 언제나 Google 프론트엔드의 내부 주소이고, 그 값을 남기면 감사 로그의 IP 칸이
// 전부 같은 값으로 채워져 아무 의미가 없다. XFF 는 `클라이언트, 프록시1, 프록시2` 순서라
// 맨 앞이 원래 요청자다.
//
// ⚠ XFF 는 클라이언트가 위조할 수 있는 헤더다. Cloud Run 은 자기가 본 주소를 맨 뒤에
// 덧붙이므로 앞쪽 값을 신뢰할 수는 없다 — 이 값은 **감사의 참고 정보이지 인증 근거가
// 아니다.** 접근 제어에 쓰지 마라.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if first != "" {
			return first
		}
	}
	// 로컬 개발에서는 XFF 가 없다. 포트는 감사에 쓸모가 없어 떼어 낸다.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ──────────────────────────────────────────────────────────────
// 조회 (GET /admin/audit-logs)
// ──────────────────────────────────────────────────────────────

// auditFilter 는 활동 로그 조회 조건이다. 시각은 이미 KST 해석을 마친 UTC 절대 시각이다.
type auditFilter struct {
	Page     int
	PageSize int
	// From 은 포함, To 는 **미포함**이다. dateTo 가 가리키는 날의 다음 날 00:00 KST 가
	// 여기 들어온다(parseDateTo 참고).
	From    *time.Time
	To      *time.Time
	AdminID string
	Target  string
	Search  string
	Desc    bool
}

// auditLog 는 조회 결과 한 줄이다.
type auditLog struct {
	ID         string
	AdminID    string
	AdminName  string
	AdminEmail string
	Actions    string
	Targets    string
	Details    map[string]any
	IPAddress  string
	CreatedAt  time.Time
}

// searchScanLimit 은 부분검색을 위해 메모리로 끌어올 문서 수의 상한이다.
//
// **Firestore 는 부분검색(LIKE)을 못 한다.** 전문 검색을 붙이려면 Algolia 같은 외부
// 색인이 필요한데, 하루 수십~수백 줄 규모의 감사 로그에 그 운영 비용을 얹을 이유가 없다.
// 그래서 날짜·관리자·대상 필터까지는 Firestore 가 좁히고, 남은 것을 이만큼 읽어
// 메모리에서 거른다.
//
// ⚠ **한계가 있고 숨기지 않는다.** 필터로 좁힌 결과가 이 수를 넘으면 **뒤쪽(오래된
// 쪽, sortOrder 에 따라 최신 쪽)이 잘린 채로 검색된다.** 그때 화면의 total 은 "찾은
// 만큼" 이지 "실제 전체" 가 아니다. 검색을 자주 쓰게 되면 날짜 범위를 좁히도록 안내하는
// 것이 먼저이고, 그래도 부족하면 그때 외부 색인을 붙인다.
const searchScanLimit = 3000

// list 는 조건에 맞는 활동 로그 한 페이지와 전체 건수를 돌려준다.
//
// 인덱스: audit_logs(createdAt desc/asc) 는 단일 필드라 자동이고, adminId·targets 필터를
// 함께 쓰면 복합 인덱스가 필요하다 — audit_logs(adminId asc, createdAt desc) ·
// audit_logs(targets asc, createdAt desc). 인덱스 소유자는 이 저장소가 아니라
// 인프라(테라폼) 쪽이다 — 여기서 정의를 바꿔도 배포되지 않는다.
func (s *auditStore) list(ctx context.Context, f auditFilter) ([]auditLog, int, error) {
	dir := firestore.Desc
	if !f.Desc {
		dir = firestore.Asc
	}

	// createdAt 이 범위 조건이자 정렬 키다. Firestore 는 범위 조건을 건 필드로 먼저
	// 정렬할 것을 요구하므로 이 조합이어야 한다.
	q := s.fs.Collection(AuditCollection).Query
	if f.From != nil {
		q = q.Where("createdAt", ">=", *f.From)
	}
	if f.To != nil {
		q = q.Where("createdAt", "<", *f.To)
	}
	if f.AdminID != "" {
		q = q.Where("adminId", "==", f.AdminID)
	}
	if f.Target != "" {
		q = q.Where("targets", "==", f.Target)
	}
	q = q.OrderBy("createdAt", dir)

	if f.Search != "" {
		return s.listWithSearch(ctx, q, f)
	}

	// total 은 **집계 카운트로 낸다.** 문서를 전부 읽어 세면 로그가 쌓일수록 조회 한 번의
	// 비용과 시간이 같이 늘어나고, 결국 감사 화면 첫 페이지가 열리지 않게 된다.
	total, err := s.count(ctx, q)
	if err != nil {
		return nil, 0, err
	}

	snaps, err := q.Offset((f.Page - 1) * f.PageSize).Limit(f.PageSize).Documents(ctx).GetAll()
	if err != nil {
		return nil, 0, err
	}
	logs, err := toAuditLogs(snaps)
	if err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

// listWithSearch 는 부분검색이 걸린 조회다. searchScanLimit 주석의 한계가 그대로 적용된다.
func (s *auditStore) listWithSearch(ctx context.Context, q firestore.Query, f auditFilter) ([]auditLog, int, error) {
	snaps, err := q.Limit(searchScanLimit).Documents(ctx).GetAll()
	if err != nil {
		return nil, 0, err
	}
	scanned, err := toAuditLogs(snaps)
	if err != nil {
		return nil, 0, err
	}

	matched := make([]auditLog, 0, len(scanned))
	for _, l := range scanned {
		if l.matches(f.Search) {
			matched = append(matched, l)
		}
	}

	// total 은 걸러 낸 뒤의 수다. 집계 카운트를 쓸 수 없다 — 그 쿼리는 검색어를 모른다.
	total := len(matched)
	start := (f.Page - 1) * f.PageSize
	if start >= total {
		return []auditLog{}, total, nil
	}
	end := start + f.PageSize
	if end > total {
		end = total
	}
	return matched[start:end], total, nil
}

// count 는 조건에 맞는 문서 수다. 문서를 읽지 않고 서버가 센 값만 받는다.
func (s *auditStore) count(ctx context.Context, q firestore.Query) (int, error) {
	const alias = "all"
	res, err := q.NewAggregationQuery().WithCount(alias).Get(ctx)
	if err != nil {
		return 0, err
	}
	v, ok := res[alias]
	if !ok {
		return 0, nil
	}
	// 집계 결과는 protobuf Value 로 온다. 정수로 오지만 타입 단정이 실패하면
	// 0 을 돌려주고 목록은 그대로 내보낸다 — 건수 하나 때문에 화면 전체를 막지 않는다.
	pv, ok := v.(*firestorepb.Value)
	if !ok {
		return 0, nil
	}
	return int(pv.GetIntegerValue()), nil
}

// matches 는 부분검색 대상 필드 중 하나라도 검색어를 포함하는지다.
//
// 대상은 네 가지다 — actions / 관리자명 / 이메일 / IP.
// details.body 는 넣지 않는다. 거기에는 긴 본문이 들어 있을 수 있어 검색어가 우연히
// 걸리는 줄이 쏟아지고, 무엇 때문에 걸렸는지 화면에서 보이지도 않는다.
func (l auditLog) matches(needle string) bool {
	needle = strings.ToLower(needle)
	for _, hay := range []string{l.Actions, l.AdminName, l.AdminEmail, l.IPAddress} {
		if strings.Contains(strings.ToLower(hay), needle) {
			return true
		}
	}
	return false
}

// parseDateFrom 은 KST 날짜(YYYY-MM-DD)의 그날 00:00 을 절대 시각으로 바꾼다.
func parseDateFrom(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, kst)
}

// parseDateTo 는 KST 날짜의 **다음 날 00:00** 을 돌려준다.
//
// dateTo 는 **그날을 포함**한다. 그래서 비교는 `<= dateTo` 가 아니라 `< dateTo+1일` 이다.
// `<=` 로 쓰면 그날 00:00:00 한 순간만 걸려 하루치가 통째로 사라지고,
// `<= dateTo 23:59:59` 로 쓰면 그 마지막 1초 안의 기록이 빠진다.
//
// AddDate 를 쓰는 것은 KST 가 고정 오프셋이라 24시간 더하기와 결과가 같지만, 시간대를
// 다루는 자리에서 산술 대신 달력 연산을 쓰는 편이 나중에 시간대를 바꿔도 안전해서다.
func parseDateTo(s string) (time.Time, error) {
	day, err := time.ParseInLocation("2006-01-02", s, kst)
	if err != nil {
		return time.Time{}, err
	}
	return day.AddDate(0, 0, 1), nil
}

func toAuditLogs(snaps []*firestore.DocumentSnapshot) ([]auditLog, error) {
	out := make([]auditLog, 0, len(snaps))
	for _, snap := range snaps {
		var doc auditDoc
		if err := snap.DataTo(&doc); err != nil {
			return nil, err
		}
		details := doc.Details
		if details == nil {
			details = map[string]any{}
		}
		out = append(out, auditLog{
			ID:         snap.Ref.ID,
			AdminID:    doc.AdminID,
			AdminName:  doc.AdminName,
			AdminEmail: doc.AdminEmail,
			Actions:    doc.Actions,
			Targets:    doc.Targets,
			Details:    details,
			IPAddress:  doc.IPAddress,
			CreatedAt:  doc.CreatedAt,
		})
	}
	return out, nil
}
