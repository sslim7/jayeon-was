package admin

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/sslim7/jayeon-was/internal/httpx"
)

// AuthedFunc 는 인증·권한 확인을 통과한 뒤 불리는 핸들러다.
//
// 클레임을 컨텍스트에 숨기지 않고 **인자로 넘긴다.** internal/auth 는 컨텍스트를 쓰지만
// 그쪽은 미인증 통과가 정상 경로라 핸들러가 값의 유무를 판단해야 한다. 어드민 라우트는
// 차단형이라 여기 도달한 시점에 클레임은 반드시 있고, 인자로 받으면 "혹시 nil 인가" 를
// 매 핸들러가 다시 확인하는 코드가 아예 생기지 않는다.
type AuthedFunc func(w http.ResponseWriter, r *http.Request, c *Claims)

// Authenticate 는 Authorization: Bearer 어드민 토큰을 검증한다.
//
// 토큰이 없거나·형식이 틀렸거나·만료됐거나·서명이 다르면 전부 ErrInvalidToken 이다.
// 어느 쪽인지 응답으로 알려 주지 않는다 — 만료와 위조를 구분해 주면 공격자에게
// "서명은 맞았다" 는 정보를 준다.
func (h *Handler) Authenticate(r *http.Request) (*Claims, error) {
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return nil, ErrInvalidToken
	}
	return h.tokens.Parse(strings.TrimSpace(strings.TrimPrefix(raw, "Bearer ")))
}

// PermResolver 는 요청마다 필요한 권한 키를 고르는 함수다. GuardedBy 가 쓴다.
//
// 빈 문자열을 돌려주면 "로그인만 되어 있으면 된다" 는 뜻이다(Subject.Allows 참고).
// **여러 키 중 하나라도 있으면 되는 경우**는 Claims 를 보고 가진 쪽을 골라 돌려준다 —
// 시그니처를 `[]string` 으로 넓히지 않는 이유는, 그러면 "전부 필요" 인지 "하나면 충분" 인지가
// 호출부마다 달라져 판정 규칙이 가드 밖으로 새어 나가기 때문이다.
type PermResolver func(r *http.Request, c *Claims) string

// guard 는 아래 네 헬퍼의 공통 몸통이다. 세 가지를 한자리에서 한다.
//
//  1. 인증 — 토큰이 없거나 유효하지 않으면 401 UNAUTHORIZED.
//  2. 권한 — allow 가 false 면 403 FORBIDDEN 이고 **핸들러는 아예 불리지 않는다.**
//  3. 감사 — **GET 이 아닌 요청만** 활동 로그에 남긴다.
//
// **셋을 한 함수에 묶은 것이 핵심이다.** 감사 기록을 각 핸들러가 알아서 부르게 두면
// 새 엔드포인트가 생길 때마다 빠뜨릴 수 있고, 빠진 줄은 화면에 아무것도 남기지 않아
// 누구도 눈치채지 못한다. 라우트를 거는 것과 기록되는 것이 같은 한 줄이어야 한다.
//
// 네 헬퍼가 이 몸통을 공유하는 것도 같은 이유다 — 권한 판정 방식만 다르고 감사 동작은
// 글자 하나까지 같아야, 어떤 헬퍼로 걸었느냐에 따라 로그가 달라지는 일이 없다.
func (h *Handler) guard(target string, allow func(*http.Request, *Claims) bool, fn AuthedFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.Authenticate(r)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, codeUnauthorized, msgUnauthorized)
			return
		}
		if !allow(r, claims) {
			WriteForbidden(w)
			return
		}
		h.runAudited(w, r, claims, target, actionOf(r), fn)
	}
}

// Guarded 는 권한 키가 **라우트마다 고정된** 엔드포인트를 감싼다. 가장 흔한 경우다.
//
// perm 이 빈 문자열이면 "로그인만 되어 있으면 된다" 는 뜻이다(Subject.Allows 참고).
// target 은 활동 로그의 도메인 문자열이다(audit.go 의 Target* 상수를 쓴다).
func (h *Handler) Guarded(target, perm string, fn AuthedFunc) http.HandlerFunc {
	return h.guard(target, func(_ *http.Request, c *Claims) bool {
		return c.Sub.Allows(perm)
	}, fn)
}

// GuardedBy 는 권한 키가 **요청마다 갈리는** 엔드포인트를 감싼다.
//
// 한 라우트가 `?kind=` 같은 값으로 서로 다른 메뉴를 오가는 경우다. 등록 시점에 키를
// 하나로 못박으면 **한쪽 메뉴 권한만 가진 사람이 다른 쪽까지 쓰게 된다.**
//
// 판정이 Guarded 와 **같은 자리(가드 안)에서** 끝나는 것이 요점이다. 핸들러가 직접
// Authenticate 를 부르고 권한을 따지는 방식으로 풀면 감사 기록을 빠뜨리기 쉽고,
// 빠진 줄은 아무 흔적도 남기지 않는다.
//
// ⚠ 상세·수정·삭제처럼 **저장된 문서를 읽어야** 키가 정해지는 경우는 이 헬퍼로 풀 수 없다.
// resolver 는 요청만 보므로 요청이 주장하는 kind 를 믿게 되고, 그러면 kind 만 바꿔 보내서
// 남의 메뉴 글을 지울 수 있다. 그때는 Authenticated 를 쓴다.
func (h *Handler) GuardedBy(target string, perm PermResolver, fn AuthedFunc) http.HandlerFunc {
	return h.guard(target, func(r *http.Request, c *Claims) bool {
		return c.Sub.Allows(perm(r, c))
	}, fn)
}

// Authenticated 는 인증과 감사만 하고 **권한 판정을 핸들러에 맡긴다.**
//
// 🔴 **가장 위험한 헬퍼다. 다른 셋으로 풀 수 없을 때만 쓴다.** 이것으로 건 핸들러는
// 권한을 스스로 판정할 의무가 있고, 잊으면 그 엔드포인트가 로그인한 아무에게나 열린다.
//
// 존재 이유는 하나다 — **저장된 문서를 읽어야 권한 키가 정해지는 경우.**
// 한 컬렉션이 kind 로 여러 메뉴에 걸쳐 있을 때 상세·수정·삭제가 그렇다. 판정 기준은
// 요청이 주장하는 kind 가 아니라 **문서에 저장된 kind** 여야 하는데, 그 값은 Firestore 를
// 읽기 전에는 알 수 없다. 문서를 읽는 일을 가드가 대신할 수는 없으므로 핸들러가 판정한다.
//
// 그래도 **감사 기록은 반드시 남는다** — Guarded 와 같은 몸통을 지나므로 GET 이 아닌
// 요청은 예외 없이 audit_logs 에 쌓인다. 권한을 핸들러로 내리는 대가로 감사까지
// 내리지는 않는다는 것이 이 헬퍼의 요점이다.
//
// 핸들러는 거절할 때 WriteForbidden 을 쓴다 — 코드와 문구를 직접 지어내면 SPA 의
// 403 처리가 엔드포인트마다 달라진다.
//
//	func (h *adminHandler) delete(w http.ResponseWriter, r *http.Request, c *admin.Claims) {
//	    notice, err := h.store.Get(r.Context(), r.PathValue("noticeId"))
//	    // ... NotFound 처리 ...
//	    if !c.Sub.Allows(permForKind(notice.Kind)) { // 저장된 kind 로 판정한다
//	        admin.WriteForbidden(w)
//	        return
//	    }
//	    // ...
//	}
func (h *Handler) Authenticated(target string, fn AuthedFunc) http.HandlerFunc {
	return h.guard(target, func(*http.Request, *Claims) bool { return true }, fn)
}

// AdminOnly 는 isAdmin 계정만 지나갈 수 있는 엔드포인트를 감싼다.
//
// 어드민 계정 관리와 활동 로그가 여기 해당한다 — 권한 맵으로는 절대 열리지 않아야 하는
// 자리라, 존재하지 않는 메뉴 키를 넘기는 식의 우회가 가능한 Guarded 대신 별도 함수로 둔다.
func (h *Handler) AdminOnly(target string, fn AuthedFunc) http.HandlerFunc {
	return h.guard(target, func(_ *http.Request, c *Claims) bool {
		return c.Sub.IsAdmin
	}, fn)
}

// WriteForbidden 은 권한 부족 응답이다. Authenticated 로 건 핸들러가 직접 거절할 때 쓴다.
//
// 가드와 **같은 코드·같은 문구**를 내보내기 위해 함수로 노출한다. 핸들러마다 문구를
// 지어내면 SPA 의 403 처리가 엔드포인트마다 달라진다.
func WriteForbidden(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusForbidden, codeForbidden, msgForbidden)
}

// runAudited 는 핸들러를 돌리고 그 결과를 활동 로그에 남긴다.
//
// 기록은 **응답을 다 내보낸 뒤에** 한다. 먼저 쓰면 status 를 알 수 없고, 기록이 느릴 때
// 사용자가 그만큼 더 기다린다. 실패는 로그로만 흘린다 — 감사 기록 실패가 이미 성공한
// 쓰기를 되돌릴 수는 없다(auditStore.write 주석).
func (h *Handler) runAudited(w http.ResponseWriter, r *http.Request, c *Claims, target, action string, fn AuthedFunc) {
	// **GET 은 기록하지 않는다.** 조회까지 남기면 로그가 조회로 뒤덮여 "무엇이 바뀌었나"
	// 를 찾을 수 없게 된다. 로그인만 예외인데 그건 login 핸들러가 직접 남긴다.
	if r.Method == http.MethodGet {
		fn(w, r, c)
		return
	}

	// 바디는 핸들러가 읽어 버리기 전에 떠 둔다. 읽고 나면 되감을 수 없으므로 사본을
	// 만들어 다시 끼워 넣는다 — 이 한 줄이 빠지면 모든 쓰기 요청이 빈 바디를 받는다.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAuditBodyBytes+1))
	if err != nil {
		body = nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	rec := newStatusRecorder(w)
	fn(rec, r, c)

	h.recordAudit(auditEntry{
		AdminID:    c.Sub.AdminID,
		AdminName:  c.Sub.Name,
		AdminEmail: c.Sub.Email,
		Actions:    action,
		Targets:    target,
		Status:     rec.status,
		Body:       bodyForAudit(body),
		IPAddress:  clientIP(r),
	})
}

// recordAudit 은 활동 로그 한 줄을 남기고 실패는 삼킨다.
//
// r.Context() 를 쓰지 않는다 — 핸들러가 반환된 뒤 net/http 가 요청 컨텍스트를 취소하는데,
// 여기서 그것을 쓰면 기록이 canceled 로 실패하는 경우가 생긴다. 대신 기동 컨텍스트를 쓴다.
func (h *Handler) recordAudit(e auditEntry) {
	if err := h.audit.write(h.baseCtx, e, h.now()); err != nil {
		log.Printf("[WARN] admin: 활동 로그 기록 실패 actions=%q adminId=%s: %v", e.Actions, e.AdminID, err)
	}
}

// actionOf 는 활동 로그의 actions 값을 만든다. "METHOD 경로" 다.
//
// **쿼리 문자열은 싣지 않는다.** 검색어나 커서가 붙으면 같은 동작이 매번 다른 문자열이
// 되어 화면의 검색·집계가 쓸모없어진다. 무엇을 보냈는지는 details.body 에 남는다.
func actionOf(r *http.Request) string {
	return r.Method + " " + r.URL.Path
}
