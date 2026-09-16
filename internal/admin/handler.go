package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sslim7/nature-was/internal/credentials"
	"github.com/sslim7/nature-was/internal/httpx"
)

const (
	// MinPasswordRunes 는 비밀번호 최소 길이다. 계정 생성과 변경이 같은 값을 쓴다 —
	// 한쪽만 느슨하면 8자 미만 비밀번호가 변경 경로로 들어온다.
	MinPasswordRunes = 8

	// MaxPasswordBytes 는 비밀번호 상한이다. **바이트다.**
	//
	// bcrypt 는 72바이트를 넘는 입력에 에러를 낸다(x/crypto). 한글은 한 자가 3바이트라
	// **한글 25자면 이미 넘는다** — 그 자리를 막지 않으면 사용자는 "사용할 수 없는
	// 비밀번호예요" 라는 이유 모를 거절만 받는다. 길이 문제라고 말해 주는 편이 낫다.
	MaxPasswordBytes = 72

	// maxNameRunes 는 관리자 이름 상한이다. 감사 로그의 한 칸에 그대로 실리는 값이라
	// 길면 목록이 무너진다.
	maxNameRunes = 50

	// maxEmailBytes 는 이메일 상한이다. 이 값이 락 문서의 ID 가 되는데 Firestore 문서 ID
	// 는 1500바이트까지라 그보다 훨씬 앞에서 막는다.
	maxEmailBytes = 254

	defaultAuditPageSize = 20
	maxAuditPageSize     = 100
)

// emailPattern 은 이메일 형식이다. RFC 5322 를 다 받지 않는다 — 목적은 두 가지뿐이다.
//
//  1. 오타로 로그인 못 하는 계정이 만들어지는 것을 막는다.
//  2. `/` 나 공백처럼 **Firestore 문서 ID 로 쓸 수 없는 문자**를 걸러 낸다.
//     이 값이 admins_by_email 의 문서 ID 가 되므로(store.go) 여기가 마지막 문지기다.
var emailPattern = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)

// ──────────────────────────────────────────────────────────────
// 응답 DTO — 어드민 SPA 가 이 필드명을 그대로 읽는다.
// ──────────────────────────────────────────────────────────────

type adminDTO struct {
	AdminID            string          `json:"adminId"`
	Email              string          `json:"email"`
	Name               string          `json:"name"`
	IsActive           bool            `json:"isActive"`
	IsAdmin            bool            `json:"isAdmin"`
	Permissions        map[string]bool `json:"permissions"`
	MustChangePassword bool            `json:"mustChangePassword"`
	CreatedAt          string          `json:"createdAt"`
	UpdatedAt          string          `json:"updatedAt"`
}

// toDTO 는 어드민 계정을 응답 모양으로 바꾼다.
//
// 🔴 **어드민 계정이 응답으로 나가는 길은 이 함수 하나뿐이다.** Store 의 Admin 에는
// passwordHash 가 들어 있고, 그 구조체를 그대로 WriteJSON 에 넘기면 bcrypt 해시가
// 브라우저까지 나간다. 나가고 나면 회수할 수 없다 — 함수를 하나로 좁혀 두면 새 엔드포인트가
// 생겨도 그 사고가 구조적으로 일어나지 않는다. 여기에 PasswordHash 를 더하지 마라.
func toDTO(a Admin) adminDTO {
	perms := a.Permissions
	if perms == nil {
		// nil 이면 JSON 에서 null 이 되고 SPA 의 Object.keys 가 터진다.
		perms = map[string]bool{}
	}
	return adminDTO{
		AdminID:            a.ID,
		Email:              a.Email,
		Name:               a.Name,
		IsActive:           a.IsActive,
		IsAdmin:            a.IsAdmin,
		Permissions:        perms,
		MustChangePassword: a.MustChangePassword,
		CreatedAt:          a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:          a.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

type tokenResponse struct {
	AccessToken string `json:"accessToken"`
}

// ──────────────────────────────────────────────────────────────
// POST /auth/admin.login  (인증 없음)
// ──────────────────────────────────────────────────────────────

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	RememberMe bool   `json:"rememberMe"`
}

// login 은 어드민 토큰을 발급한다.
//
// **성공도 실패도 활동 로그에 남는다.** GET 이 아닌 요청만 남긴다는 규칙의 예외가
// 아니라 — 로그인은 POST 다 — 가드를 지나지 않는 유일한 라우트라 직접 남긴다.
// 실패 기록이 없으면 무차별 대입 시도가 어디에도 흔적을 남기지 않는다.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	rec := newStatusRecorder(w)

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAuditBodyBytes+1))
	if err != nil {
		raw = nil
	}
	var req loginRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		httpx.WriteError(rec, http.StatusBadRequest, codeValidation, "요청 값이 올바르지 않아요")
		h.recordLogin(r, rec, Admin{}, req.Email, raw, false)
		return
	}

	email := credentials.NormalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		httpx.WriteError(rec, http.StatusBadRequest, codeValidation, "이메일과 비밀번호를 입력해 주세요")
		h.recordLogin(r, rec, Admin{}, email, raw, false)
		return
	}

	found, err := h.store.FindByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("admin: 로그인 조회 실패 email=%s: %v", email, err)
		httpx.WriteError(rec, http.StatusInternalServerError, codeInternal, msgInternal)
		h.recordLogin(r, rec, Admin{}, email, raw, false)
		return
	}
	missing := errors.Is(err, ErrNotFound)

	// 🔴 **계정이 없어도 비밀번호 비교를 반드시 한 번 돌린다.** 없을 때 곧장 401 을
	// 돌려주면 bcrypt 한 번(수십 ms)이 통째로 빠져 응답 시간이 눈에 띄게 빨라지고,
	// 그 차이만으로 "이 이메일이 어드민인가" 를 밖에서 훑어낼 수 있다.
	// VerifyPassword 는 해시가 비면 미끼 해시로 비교한다(internal/credentials).
	ok := credentials.VerifyPassword(found.PasswordHash, req.Password)

	// 🔴 **계정이 없을 때와 비밀번호가 틀릴 때의 응답이 글자 하나까지 같아야 한다.**
	// 바로 위의 미끼 해시와 아래의 비활성 판정 순서는 둘 다 "이 이메일이 어드민인가" 를
	// 감추려고 둔 장치인데, 문구를 갈라 놓으면 그 둘이 통째로 무의미해진다 — 시간을 재지
	// 않아도 응답 본문이 그대로 알려 주기 때문이다. 화면에 친절한 안내를 띄우고 싶어지는
	// 자리지만, 그 친절의 대가가 어드민 이메일 목록을 밖에서 훑을 수 있게 되는 것이다.
	//
	// (형제 프로젝트 birdieup-was 는 여기서 "등록된 어드민 계정이 아닙니다" 와
	//  "비밀번호를 확인하세요" 로 갈라 놓았다. 같은 파일이 공들여 만든 두 방어를 스스로
	//  되돌리고 있는 자리라 그대로 옮기지 않았다.)
	if missing || !ok {
		httpx.WriteError(rec, http.StatusUnauthorized, codeUnauthorized, msgLoginFailed)
		if missing {
			h.recordLogin(r, rec, Admin{}, email, raw, false)
		} else {
			h.recordLogin(r, rec, found, email, raw, false)
		}
		return
	}
	// 비활성 판정을 비밀번호 확인 **뒤에** 둔다. 앞에 두면 비밀번호를 모르는 사람도
	// "그 계정은 있고 잠겨 있다" 를 알게 된다.
	if !found.IsActive {
		httpx.WriteError(rec, http.StatusForbidden, codeForbidden, "비활성 계정입니다")
		h.recordLogin(r, rec, found, email, raw, false)
		return
	}

	token, err := h.tokens.Issue(found.Subject(), req.RememberMe)
	if err != nil {
		log.Printf("admin: 토큰 발급 실패 adminId=%s: %v", found.ID, err)
		httpx.WriteError(rec, http.StatusInternalServerError, codeInternal, msgInternal)
		h.recordLogin(r, rec, found, email, raw, false)
		return
	}

	httpx.WriteJSON(rec, http.StatusOK, tokenResponse{AccessToken: token})
	h.recordLogin(r, rec, found, email, raw, true)
}

// recordLogin 은 로그인 시도 한 건을 활동 로그에 남긴다.
//
// 실패 기록에는 계정이 없을 수 있다. 그때 adminId 는 비고 **adminEmail 에는 시도된
// 주소가 들어간다** — 누가 어떤 주소로 두드리고 있는지가 그 칸에 남아야 의미가 있다.
func (h *Handler) recordLogin(r *http.Request, rec *statusRecorder, a Admin, attempted string, body []byte, success bool) {
	action := ActionLoginFailed
	if success {
		action = ActionLogin
	}
	email := a.Email
	if email == "" {
		email = attempted
	}
	h.recordAudit(auditEntry{
		AdminID:    a.ID,
		AdminName:  a.Name,
		AdminEmail: email,
		Actions:    action,
		Targets:    TargetAuth,
		Status:     rec.status,
		// bodyForAudit 이 password 키를 [REDACTED] 로 바꾼다. 이 한 줄이 빠지면
		// 어드민 비밀번호가 평문으로 감사 로그에 영구히 쌓인다.
		Body:      bodyForAudit(body),
		IPAddress: clientIP(r),
	})
}

// ──────────────────────────────────────────────────────────────
// POST /auth/admin.change-password  (어드민 토큰)
// ──────────────────────────────────────────────────────────────

type changePasswordRequest struct {
	NewPassword string `json:"newPassword"`
}

// changePassword 는 본인 비밀번호를 바꾸고 **새 토큰을 함께 돌려준다.**
//
// 현재 비밀번호를 묻지 않는다 — 이 화면은 로그인 직후에만 도달하고 본인 확인은 토큰이
// 한다. 새 토큰을 주는 이유는 mustChangePassword 가 토큰 안에 있어서다.
// 갱신하지 않으면 SPA 가 바꾸자마자 다시 비밀번호 변경 화면으로 튕긴다.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request, c *Claims) {
	var req changePasswordRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
		return
	}

	// 토큰이 아니라 **문서를 다시 읽는다.** 토큰의 sub 는 로그인 시점의 사본이라
	// 그 사이 비밀번호가 바뀌었으면 "기존과 다른가" 판정이 옛 해시로 이뤄진다.
	current, err := h.store.Get(r.Context(), c.Sub.AdminID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, codeNotFound, msgNotFound)
			return
		}
		log.Printf("admin: 비밀번호 변경 대상 조회 실패 adminId=%s: %v", c.Sub.AdminID, err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}
	if credentials.VerifyPassword(current.PasswordHash, req.NewPassword) {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, "기존 비밀번호와 다른 비밀번호를 입력해 주세요")
		return
	}

	hash, err := credentials.HashPassword(req.NewPassword)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, "사용할 수 없는 비밀번호예요")
		return
	}
	must := false
	now := h.now()
	if err := h.store.Update(r.Context(), current.ID, Changes{
		PasswordHash:       &hash,
		MustChangePassword: &must,
	}, now); err != nil {
		log.Printf("admin: 비밀번호 변경 실패 adminId=%s: %v", current.ID, err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}

	current.PasswordHash = hash
	current.MustChangePassword = false
	current.UpdatedAt = now
	token, err := h.tokens.Issue(current.Subject(), false)
	if err != nil {
		log.Printf("admin: 비밀번호 변경 후 토큰 발급 실패 adminId=%s: %v", current.ID, err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, tokenResponse{AccessToken: token})
}

// ──────────────────────────────────────────────────────────────
// GET /admin/admins  (isAdmin)
// ──────────────────────────────────────────────────────────────

type listAdminsResponse struct {
	Items []adminDTO `json:"items"`
}

func (h *Handler) listAdmins(w http.ResponseWriter, r *http.Request, _ *Claims) {
	found, err := h.store.List(r.Context())
	if err != nil {
		log.Printf("admin: 계정 목록 조회 실패: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}
	items := make([]adminDTO, 0, len(found))
	for _, a := range found {
		items = append(items, toDTO(a))
	}
	httpx.WriteJSON(w, http.StatusOK, listAdminsResponse{Items: items})
}

// ──────────────────────────────────────────────────────────────
// POST /admin/admins  (isAdmin)
// ──────────────────────────────────────────────────────────────

type createAdminRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
	// 아래 셋은 포인터다. **기본값이 제로값과 다르기** 때문이다 —
	// mustChangePassword 의 기본은 true 라 값 타입으로 받으면 "안 보냈다" 와
	// "false 로 보냈다" 를 구분할 수 없어 초기 비밀번호가 그대로 눌러앉는다.
	IsAdmin            *bool            `json:"isAdmin"`
	Permissions        *map[string]bool `json:"permissions"`
	MustChangePassword *bool            `json:"mustChangePassword"`
}

type createAdminResponse struct {
	AdminID string `json:"adminId"`
	Email   string `json:"email"`
	Name    string `json:"name"`
}

func (h *Handler) createAdmin(w http.ResponseWriter, r *http.Request, _ *Claims) {
	var req createAdminRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	email := credentials.NormalizeEmail(req.Email)
	if msg := validateEmail(email); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
		return
	}
	name := strings.TrimSpace(req.Name)
	if msg := validateName(name); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
		return
	}

	hash, err := credentials.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, "사용할 수 없는 비밀번호예요")
		return
	}

	now := h.now()
	a := Admin{
		Email:        email,
		PasswordHash: hash,
		Name:         name,
		// 새 계정은 켜진 상태로 만든다. 꺼진 채로 만들 이유가 없고, 꺼야 한다면 PATCH 가 있다.
		IsActive: true,
		// 권한은 주는 것이지 기본으로 갖는 것이 아니고, 초기 비밀번호는 만든 사람도
		// 알고 있으므로 반드시 한 번 바꾸게 한다.
		IsAdmin:            false,
		Permissions:        map[string]bool{},
		MustChangePassword: true,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if req.IsAdmin != nil {
		a.IsAdmin = *req.IsAdmin
	}
	if req.Permissions != nil && *req.Permissions != nil {
		a.Permissions = *req.Permissions
	}
	if req.MustChangePassword != nil {
		a.MustChangePassword = *req.MustChangePassword
	}

	id, err := h.store.Create(r.Context(), a)
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			httpx.WriteError(w, http.StatusConflict, codeEmailTaken, "이미 등록된 이메일이에요")
			return
		}
		log.Printf("admin: 계정 생성 실패 email=%s: %v", email, err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}

	// 201 의 바디는 세 칸뿐이다. 목록은 SPA 가 다시 불러 갱신한다.
	httpx.WriteJSON(w, http.StatusCreated, createAdminResponse{AdminID: id, Email: email, Name: name})
}

// ──────────────────────────────────────────────────────────────
// PATCH /admin/admins/{adminId}  (isAdmin)
// ──────────────────────────────────────────────────────────────

// patchAdminRequest 는 부분 수정이라 **모든 필드가 포인터다.**
// nil 은 "그 항목을 건드리지 않는다" 는 뜻이고, 값 타입으로 받으면 이름만 고치려던
// 요청이 권한 맵을 빈 값으로 덮어 버린다.
type patchAdminRequest struct {
	// Email 은 받기만 하고 거절하기 위해 있다. 필드를 아예 두지 않으면 encoding/json 이
	// 모르는 키를 조용히 버려서, 이메일을 고쳤다고 믿는 화면이 만들어진다.
	Email              *string          `json:"email"`
	Name               *string          `json:"name"`
	IsActive           *bool            `json:"isActive"`
	IsAdmin            *bool            `json:"isAdmin"`
	Permissions        *map[string]bool `json:"permissions"`
	MustChangePassword *bool            `json:"mustChangePassword"`
	Password           *string          `json:"password"`
}

func (h *Handler) patchAdmin(w http.ResponseWriter, r *http.Request, c *Claims) {
	adminID := r.PathValue("adminId")

	var req patchAdminRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Email != nil {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, "이메일은 바꿀 수 없어요")
		return
	}

	// **본인 계정의 isAdmin·isActive 는 스스로 바꿀 수 없다.**
	// 마지막 관리자가 자기 권한을 내리면 아무도 계정 관리 화면에 들어갈 수 없게 되고,
	// 되돌리려면 cmd/create-admin 을 다시 돌려야 한다. 조용히 무시하지 않고 거절하는
	// 이유는 눌렀는데 아무 일도 일어나지 않는 화면이 더 나쁘기 때문이다.
	//
	// **판정 기준은 "키가 실렸는가" 가 아니라 "값이 실제로 달라지는가" 다.**
	// 이름만 고치는 PATCH 에 폼이 들고 있던 isAdmin 이 현재 값 그대로 딸려 오는 일이
	// 흔한데, 그것까지 400 으로 막으면 화면은 "이름 수정이 왜 실패하지" 가 되고
	// 원인을 알 방법이 없다. 그래서 현재 문서를 읽어 견줘 본다.
	if adminID == c.Sub.AdminID && (req.IsAdmin != nil || req.IsActive != nil) {
		self, err := h.store.Get(r.Context(), adminID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				httpx.WriteError(w, http.StatusNotFound, codeNotFound, msgNotFound)
				return
			}
			log.Printf("admin: 본인 계정 확인 실패 adminId=%s: %v", adminID, err)
			httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
			return
		}
		if (req.IsAdmin != nil && *req.IsAdmin != self.IsAdmin) ||
			(req.IsActive != nil && *req.IsActive != self.IsActive) {
			httpx.WriteError(w, http.StatusBadRequest, codeSelfDemotion,
				"본인 계정의 관리자 권한과 활성 상태는 바꿀 수 없어요")
			return
		}
	}

	changes := Changes{
		IsActive:           req.IsActive,
		IsAdmin:            req.IsAdmin,
		MustChangePassword: req.MustChangePassword,
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if msg := validateName(name); msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
			return
		}
		changes.Name = &name
	}
	if req.Permissions != nil {
		perms := *req.Permissions
		if perms == nil {
			// `"permissions": null` 은 "전부 회수" 로 읽는다. nil 을 그대로 쓰면
			// Firestore 문서의 그 필드가 null 이 되어 다음 로그인에서 맵이 사라진다.
			perms = map[string]bool{}
		}
		changes.Permissions = &perms
	}
	if req.Password != nil {
		if msg := validatePassword(*req.Password); msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
			return
		}
		hash, err := credentials.HashPassword(*req.Password)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, codeValidation, "사용할 수 없는 비밀번호예요")
			return
		}
		changes.PasswordHash = &hash
		// 🔴 **password 를 보내면 mustChangePassword 를 `true` 로 올린다.**
		// 방향에 주의하라 — 이 경로에서 비밀번호를 바꾸는 사람은 **계정 주인이 아니라
		// 관리자**다. 동료 비밀번호를 재설정해 주는 상황이고, 그러면 재설정한 사람이
		// 그 비밀번호를 안다. 여기서 내려 버리면 그 상태가 그대로 굳는다.
		// POST /admin/admins 가 기본을 true 로 두는 것과 같은 이유다.
		//
		// 본인이 스스로 바꾸는 경로는 /auth/admin.change-password 이고 **그쪽만
		// false 로 내린다** — 거기서는 새 비밀번호를 아는 사람이 본인뿐이다.
		//
		// 요청이 mustChangePassword 를 명시했으면 그 값이 이긴다.
		if req.MustChangePassword == nil {
			must := true
			changes.MustChangePassword = &must
		}
	}

	if changes.Empty() {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, "바꿀 항목이 없어요")
		return
	}

	if err := h.store.Update(r.Context(), adminID, changes, h.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, codeNotFound, msgNotFound)
			return
		}
		log.Printf("admin: 계정 수정 실패 adminId=%s: %v", adminID, err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}

	// 수정 뒤 문서를 다시 읽어 돌려준다. 요청 값으로 응답을 조립하면 서버가 함께 내린
	// mustChangePassword 나 updatedAt 이 화면에 반영되지 않는다.
	updated, err := h.store.Get(r.Context(), adminID)
	if err != nil {
		log.Printf("admin: 계정 수정 후 조회 실패 adminId=%s: %v", adminID, err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toDTO(updated))
}

// ──────────────────────────────────────────────────────────────
// GET /admin/audit-logs  (isAdmin)
// ──────────────────────────────────────────────────────────────

type auditLogDTO struct {
	AuditLogID string         `json:"auditLogId"`
	AdminID    string         `json:"adminId"`
	AdminName  string         `json:"adminName"`
	AdminEmail string         `json:"adminEmail"`
	Actions    string         `json:"actions"`
	Targets    string         `json:"targets"`
	Details    map[string]any `json:"details"`
	IPAddress  string         `json:"ipAddress"`
	CreatedAt  string         `json:"createdAt"`
}

// auditLogsResponse 는 **이 API 만 커서가 아니라 page/total 을 쓴다.**
// 감사 화면은 "몇 건 중 몇 페이지" 가 필요하고, 그 수는 집계 카운트로 낼 수 있다.
type auditLogsResponse struct {
	Logs     []auditLogDTO `json:"logs"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"pageSize"`
}

func (h *Handler) listAuditLogs(w http.ResponseWriter, r *http.Request, _ *Claims) {
	f, msg := parseAuditFilter(r.URL.Query())
	if msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, codeValidation, msg)
		return
	}

	logs, total, err := h.audit.list(r.Context(), f)
	if err != nil {
		log.Printf("admin: 활동 로그 조회 실패: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal)
		return
	}

	items := make([]auditLogDTO, 0, len(logs))
	for _, l := range logs {
		items = append(items, auditLogDTO{
			AuditLogID: l.ID,
			AdminID:    l.AdminID,
			AdminName:  l.AdminName,
			AdminEmail: l.AdminEmail,
			Actions:    l.Actions,
			Targets:    l.Targets,
			Details:    l.Details,
			IPAddress:  l.IPAddress,
			CreatedAt:  l.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, auditLogsResponse{
		Logs:     items,
		Total:    total,
		Page:     f.Page,
		PageSize: f.PageSize,
	})
}

// parseAuditFilter 는 쿼리 문자열을 조회 조건으로 바꾼다.
// 두 번째 반환값이 비어 있지 않으면 그것이 400 으로 나갈 한국어 문구다.
//
// 잘못된 값을 기본값으로 눕히지 않고 거절한다 — page=0 을 조용히 1로 고치면 화면의
// 페이지 번호와 응답의 page 가 어긋난 채로 계속 돌아간다.
func parseAuditFilter(q map[string][]string) (auditFilter, string) {
	get := func(key string) string {
		if v, ok := q[key]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}

	f := auditFilter{Page: 1, PageSize: defaultAuditPageSize, Desc: true}

	if raw := get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return f, "page 는 1 이상의 정수여야 해요"
		}
		f.Page = n
	}
	if raw := get("pageSize"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAuditPageSize {
			return f, "pageSize 는 1~" + strconv.Itoa(maxAuditPageSize) + " 사이의 정수여야 해요"
		}
		f.PageSize = n
	}
	if raw := get("dateFrom"); raw != "" {
		from, err := parseDateFrom(raw)
		if err != nil {
			return f, "dateFrom 은 YYYY-MM-DD 형식이어야 해요"
		}
		f.From = &from
	}
	if raw := get("dateTo"); raw != "" {
		to, err := parseDateTo(raw)
		if err != nil {
			return f, "dateTo 는 YYYY-MM-DD 형식이어야 해요"
		}
		f.To = &to
	}
	// 뒤집힌 범위는 Firestore 에서 조용히 빈 결과가 된다. 화면에는 "기록이 없다" 로
	// 보여 사용자가 자기 입력을 의심하지 못하므로 여기서 이유를 알려 준다.
	if f.From != nil && f.To != nil && !f.To.After(*f.From) {
		return f, "dateFrom 이 dateTo 보다 뒤일 수 없어요"
	}

	f.AdminID = get("adminId")
	f.Target = get("target")
	f.Search = get("search")

	switch order := get("sortOrder"); order {
	case "", "desc":
		f.Desc = true
	case "asc":
		f.Desc = false
	default:
		return f, "sortOrder 는 asc 또는 desc 여야 해요"
	}
	return f, ""
}

// ──────────────────────────────────────────────────────────────
// 검증
// ──────────────────────────────────────────────────────────────

// validateEmail 은 문제가 있으면 화면에 띄울 문구를, 없으면 빈 문자열을 돌려준다.
// email 은 NormalizeEmail 을 이미 거친 값이어야 한다(패턴이 소문자만 받는다).
func validateEmail(email string) string {
	if email == "" {
		return "이메일을 입력해 주세요"
	}
	if len(email) > maxEmailBytes {
		return "이메일이 너무 길어요"
	}
	if !emailPattern.MatchString(email) {
		return "이메일 형식이 올바르지 않아요"
	}
	return ""
}

// validatePassword 는 비밀번호 길이를 확인한다.
//
// 하한은 **룬**, 상한은 **바이트**다. 단위가 다른 것이 실수가 아니다 — 하한은 사람이 세는
// 글자 수여야 하고(바이트로 재면 한글 3자가 8자로 통과한다), 상한은 bcrypt 가 실제로
// 받아 주는 크기여야 한다(MaxPasswordBytes 주석).
func validatePassword(plain string) string {
	if utf8.RuneCountInString(plain) < MinPasswordRunes {
		return "비밀번호는 " + strconv.Itoa(MinPasswordRunes) + "자 이상이어야 해요"
	}
	if len(plain) > MaxPasswordBytes {
		return "비밀번호가 너무 길어요 (한글은 " + strconv.Itoa(MaxPasswordBytes/3) + "자까지예요)"
	}
	return ""
}

// validateName 은 관리자 이름을 확인한다. 룬으로 센다 —
// 바이트로 재면 한글 이름이 실제보다 세 배 길게 계산된다.
func validateName(name string) string {
	if name == "" {
		return "이름을 입력해 주세요"
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "이름은 " + strconv.Itoa(maxNameRunes) + "자까지 쓸 수 있어요"
	}
	return ""
}
