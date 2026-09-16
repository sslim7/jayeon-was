// Package admin 은 어드민 SPA 를 받치는 인증·계정·활동 로그 기반이다.
//
// # 계약 정본 문서가 아직 없다
//
// 어드민 SPA 를 만들 때 **이 패키지가 내보내는 응답 모양을 정본으로 삼아 문서를 먼저
// 쓴다.** 문서 없이 서버와 화면을 각자 고치면 **조용히 어긋난다** — 필드 이름 하나가
// 달라져도 컴파일은 되고 로그인도 되며, 어드민 화면의 한 칸만 비어서 아무도 눈치채지
// 못한다. 문서가 생긴 뒤에는 바꿀 일이 있을 때 그 문서를 먼저 고친다.
//
//	admins/{adminId}                 // 자동 ID
//	  email               string     소문자 정규화. 로그인 조회 키
//	  passwordHash        string     bcrypt
//	  name                string
//	  isActive            bool
//	  isAdmin             bool       true 면 전 메뉴
//	  permissions         map[string]bool   메뉴 키 → 허용 여부
//	  mustChangePassword  bool
//	  createdAt/updatedAt timestamp
//
//	admins_by_email/{email}          // 이메일 유일성 락 (store.go 참고)
//	audit_logs/{auditLogId}          // 활동 로그 (audit.go 참고)
//
// # 사용자 인증과 완전히 분리되어 있다
//
// 어드민 토큰은 ADMIN_JWT_SECRET 으로 서명하며 그 값은 JWT_SECRET 과 **달라야 한다**.
// 같으면 기동을 거부한다 — 이유는 token.go 의 ErrSecretReused 주석에 적었다.
// ADMIN_JWT_SECRET 이 비어 있으면 라우트를 하나도 걸지 않는다(경고만 남기고 기동은 계속).
//
// # 도메인 어드민 핸들러를 붙이는 법 (다음 작업자용)
//
// Register 가 돌려주는 *Handler 가 인증·권한·감사를 한자리에서 처리한다. 도메인 패키지는
// 자기 라우트를 이렇게 감싸 걸면 된다. 아래 notices 패키지와 PermNotices·TargetNotices 는
// **아직 없는 가상의 예시**다 — 메뉴를 만들 때 권한 키는 이 파일의 상수 블록에,
// targets 값은 audit.go 의 Target 상수에 더한다.
//
//	adminH, err := admin.Register(mux, client)
//	if err != nil {
//	    log.Fatalf("어드민 초기화 실패: %v", err)
//	}
//	if adminH != nil { // nil 이면 ADMIN_JWT_SECRET 이 없어 어드민이 꺼진 상태다
//	    notices.RegisterAdmin(mux, client, adminH)
//	}
//
//	// internal/notices/admin.go 쪽
//	func RegisterAdmin(mux *http.ServeMux, fs *firestore.Client, g *admin.Handler) {
//	    h := &adminHandler{store: NewStore(fs)}
//	    // 권한 키가 라우트마다 고정이면 Guarded.
//	    mux.HandleFunc("POST /admin/notices", g.Guarded(admin.TargetNotices, admin.PermNotices, h.create))
//	}
//
//	func (h *adminHandler) create(w http.ResponseWriter, r *http.Request, c *admin.Claims) {
//	    // 여기 도달했다면 인증·권한은 끝났다. c.Sub.AdminID 등이 이미 채워져 있다.
//	}
//
// # 권한 키가 요청마다 갈릴 때 — GuardedBy
//
// 한 라우트가 쿼리 값에 따라 서로 다른 메뉴를 오가는 경우가 있다. 예컨대 `/admin/notices`
// 하나가 `?kind=` 로 공지와 릴리스 노트를 함께 다루는 식이다. 등록 시점에 키를 하나로
// 못박으면 **공지 권한만 가진 사람이 릴리스 노트까지 쓰게 된다.** 권한 키를 요청에서
// 뽑되 판정은 가드 안에서 끝낸다.
//
//	// kind 를 생략한 목록 조회는 **둘 중 하나라도** 있으면 통과시킨다.
//	// (생략을 "전부 허용" 으로 읽으면 그것도 같은 구멍이다.
//	//  응답을 가진 권한의 kind 로 좁히는 것은 핸들러 몫이다)
//	byKind := func(r *http.Request, c *admin.Claims) string {
//	    switch r.URL.Query().Get("kind") {
//	    case notices.KindNotice:
//	        return admin.PermNotices
//	    case notices.KindRelease:
//	        return admin.PermReleases
//	    default:
//	        if c.Sub.Allows(admin.PermNotices) {
//	            return admin.PermNotices
//	        }
//	        return admin.PermReleases
//	    }
//	}
//	mux.HandleFunc("GET /admin/notices", g.GuardedBy(admin.TargetNotices, byKind, h.list))
//	mux.HandleFunc("POST /admin/notices", g.GuardedBy(admin.TargetNotices, byKind, h.create))
//
// # 저장된 문서를 읽어야 키가 정해질 때 — Authenticated
//
// 상세·수정·삭제는 **저장된 문서의 kind** 로 판정해야 한다. 요청이 주장하는 kind 를
// 믿으면 kind 만 바꿔 보내서 남의 메뉴 글을 지울 수 있다. 문서를 읽는 일은 가드가 대신할
// 수 없으므로 권한 판정만 핸들러로 내리되, **감사 기록은 그대로 남는다.**
//
//	mux.HandleFunc("DELETE /admin/notices/{noticeId}",
//	    g.Authenticated(admin.TargetNotices, h.delete))
//
//	func (h *adminHandler) delete(w http.ResponseWriter, r *http.Request, c *admin.Claims) {
//	    notice, err := h.store.Get(r.Context(), r.PathValue("noticeId"))
//	    // ... NotFound 처리 ...
//	    if !c.Sub.Allows(permForKind(notice.Kind)) { // 저장된 kind 로 판정한다
//	        admin.WriteForbidden(w)               // 코드·문구를 직접 지어내지 마라
//	        return
//	    }
//	    // ...
//	}
//
// 🔴 Authenticated 는 **권한 판정을 잊으면 로그인한 아무에게나 열린다.** 위 셋으로
// 풀리지 않을 때만 쓴다.
//
// # 감사 기록
//
// **핸들러가 직접 부르지 마라.** 네 헬퍼(Guarded·GuardedBy·Authenticated·AdminOnly)가
// 같은 몸통을 지나며 GET 이 아닌 요청을 전부 남긴다 — 핸들러가 또 남기면 한 동작에
// 두 줄이 쌓인다. 어떤 헬퍼로 걸었느냐에 따라 로그가 달라지지도 않는다.
package admin

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/firestore"

	"github.com/sslim7/jayeon-was/internal/httpx"
)

// 메뉴 권한 키 상수가 들어올 자리다. **아직 어드민 메뉴가 하나도 없어서 비어 있다** —
// 도메인 어드민 메뉴를 만드는 사람이 여기에 상수를 더한다.
//
//	const PermNotices = "notices"
//
// 🔴 키 문자열은 어드민 SPA 의 네비게이션 정의와 **글자 그대로** 같아야 한다.
// 서버는 이 문자열을 permissions 맵에서 대조만 하므로, 오타가 나면 그 메뉴는 isAdmin 이
// 아닌 누구에게도 열리지 않는데 **화면에는 메뉴가 보인다.** 눌러야 403 이 돌아오고,
// 어느 쪽 철자가 틀렸는지는 양쪽을 나란히 놓고 비교하기 전에는 알 수 없다.
//
// 메뉴마다 키를 새로 만들 것 — 하나의 키로 여러 메뉴를 묶으면 한 메뉴를 주려다
// 다른 메뉴까지 함께 열리고, 나중에 가르려면 이미 나간 권한을 회수해야 한다.

// 에러 코드. 어드민 SPA 가 이 값으로 분기하므로 문자열을 바꾸면 화면의 분기가 깨진다.
// 계약 문서를 쓸 때 이 값들이 정본이 된다.
const (
	codeValidation   = "VALIDATION_FAILED"
	codeUnauthorized = "UNAUTHORIZED"
	codeForbidden    = "FORBIDDEN"
	codeNotFound     = "NOT_FOUND"
	codeEmailTaken   = "EMAIL_ALREADY_EXISTS"
	// codeSelfDemotion 은 본인 계정의 isAdmin·isActive 를 스스로 내리려 할 때다.
	// 조용히 무시하면 눌렀는데 안 바뀌는 화면이 되므로 명시적으로 거절한다.
	codeSelfDemotion = "SELF_DEMOTION_FORBIDDEN"
	// codeInternal 은 httpx.CodeInternal 과 **같은 값이어야 한다.** 여기만 "INTERNAL" 로
	// 두면 같은 서버가 500 을 두 가지 코드로 내보내게 된다 — 공통 층(httpx)이 내는 500 은
	// "INTERNAL_ERROR", 어드민 핸들러가 내는 500 은 "INTERNAL" 이 되어, 코드로 분기하는
	// 클라이언트가 어드민 경로의 500 만 못 알아본다. 화면에는 「알 수 없는 오류」로 뭉개져
	// 나타나고 서버 로그는 정상이라 원인을 찾기 어렵다. openapi.yaml 의 공통 응답도
	// "INTERNAL_ERROR" 로 적혀 있어 그쪽이 정본이다.
	// (형제 프로젝트 birdieup-was 는 두 값이 어긋나 있다. 그대로 옮기지 않았다.)
	codeInternal = httpx.CodeInternal
)

// 화면에 그대로 띄우는 문구다. 전부 한국어 문장이어야 한다.
const (
	msgUnauthorized = "로그인이 필요해요"
	msgForbidden    = "권한이 없어요"
	msgInternal     = "서버 오류가 생겼어요"
	msgNotFound     = "찾을 수 없는 계정이에요"
	// msgLoginFailed 는 로그인 실패 **전부**에 쓰는 하나의 문구다. 이메일이 없을 때와
	// 비밀번호가 틀릴 때를 갈라 적으면 어드민 이메일 목록을 밖에서 훑을 수 있게 된다
	// (handler.go 의 로그인 실패 분기 주석 참고). 상수를 하나만 두어, 나중에 한쪽만
	// 친절하게 고치는 일이 생기지 않게 한다.
	msgLoginFailed = "이메일 또는 비밀번호가 맞지 않아요"
)

// Handler 는 어드민 라우트의 인증·권한·감사와 계정 관리 엔드포인트를 함께 들고 있다.
//
// 도메인 패키지는 이 타입을 **가드로만** 쓴다(Guarded / AdminOnly). 내부 필드를 꺼내
// 쓸 일은 없고, 그래야 감사 기록을 우회하는 경로가 생기지 않는다.
type Handler struct {
	store  *Store
	audit  *auditStore
	tokens *TokenIssuer
	now    func() time.Time
	// baseCtx 는 활동 로그를 쓸 때 쓰는 컨텍스트다. 요청 컨텍스트는 핸들러가 반환되는
	// 순간 취소되어 기록이 canceled 로 실패한다(guard.go recordAudit 주석).
	baseCtx context.Context
}

// Register 는 어드민 인증·계정·활동 로그 라우트를 mux 에 붙이고 가드를 돌려준다.
//
// **ADMIN_JWT_SECRET 이 비어 있으면 아무 라우트도 걸지 않고 (nil, nil) 을 돌려준다.**
// 키 하나 때문에 서버 전체가 죽으면 어드민 화면 하나보다 훨씬 넓은 사고가 된다 —
// 사용자 앱은 어드민과 무관하게 계속 떠 있어야 한다. 대신 기동 로그에 경고를 남겨
// "왜 어드민이 404 인가" 를 첫 화면에서 알 수 있게 한다.
//
// 에러를 돌려주는 경우는 **ADMIN_JWT_SECRET 이 JWT_SECRET 과 같을 때뿐이다.** 그때는
// 위험한 설정이라 기동을 멈춰야 한다(token.go ErrSecretReused).
func Register(mux *http.ServeMux, fs *firestore.Client) (*Handler, error) {
	// TrimSpace 를 거친다. `.env` 에 `ADMIN_JWT_SECRET= ` 처럼 공백만 남으면 그 공백이
	// 그대로 서명 키가 되는데, 사실상 아무나 맞힐 수 있는 키로 어드민 토큰이 서명된다.
	// 비어 있는 것으로 보고 라우트를 걸지 않는 편이 안전하다.
	secret := strings.TrimSpace(os.Getenv("ADMIN_JWT_SECRET"))
	if secret == "" {
		log.Println("[WARN] ADMIN_JWT_SECRET 이 없다 — 어드민 API 를 등록하지 않는다 " +
			"(/auth/admin.login 과 /admin/* 이 전부 404 가 된다)")
		return nil, nil
	}
	tokens, err := NewTokenIssuer(secret, os.Getenv("JWT_SECRET"))
	if err != nil {
		return nil, err
	}

	h := &Handler{
		store:   NewStore(fs),
		audit:   newAuditStore(fs),
		tokens:  tokens,
		now:     time.Now,
		baseCtx: context.Background(),
	}

	// 로그인만 인증이 없다. 나머지는 전부 가드를 지난다.
	mux.HandleFunc("POST /auth/admin.login", h.login)
	// 비밀번호 변경은 메뉴가 아니라 계정 자신에 대한 동작이라 권한 키가 없다 —
	// 로그인만 되어 있으면 지나간다(Subject.Allows 의 빈 문자열 규칙).
	mux.HandleFunc("POST /auth/admin.change-password", h.Guarded(TargetAuth, "", h.changePassword))

	mux.HandleFunc("GET /admin/admins", h.AdminOnly(TargetAdmin, h.listAdmins))
	mux.HandleFunc("POST /admin/admins", h.AdminOnly(TargetAdmin, h.createAdmin))
	mux.HandleFunc("PATCH /admin/admins/{adminId}", h.AdminOnly(TargetAdmin, h.patchAdmin))
	mux.HandleFunc("GET /admin/audit-logs", h.AdminOnly(TargetAdmin, h.listAuditLogs))

	return h, nil
}
