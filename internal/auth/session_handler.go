package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/sslim7/nature-was/internal/httpx"
)

// CodeSessionNotFound 는 「그런 기기가 없다」다.
//
// 🔴 **남의 세션도 이 코드로 404 다.** 403 을 주면 「그 id 는 실제로 있다」를 알려 주는
// 것과 같다 — 세션 id 는 목록 응답에 그대로 나가는 값이라 남의 것을 찍어 볼 수 있다.
// 이 저장소의 같은 판단이 internal/calls/audio.go 의 load 에 있다.
const CodeSessionNotFound = "SESSION_NOT_FOUND"

// maxListedSessions 는 한 번에 훑는 세션 문서 수 상한이다.
//
// 로그인 한 번에 세션 하나가 생기므로 이 값은 「최근 로그인 100번」이다. 사용자가 둘뿐인
// 서비스에서 넘칠 일은 없지만, 상한이 없으면 문서가 늘어나는 만큼 이 엔드포인트의 읽기
// 비용이 같이 늘어난다. 넘치는 것은 어차피 오래된 것부터라 화면에서 잃을 것이 없다.
const maxListedSessions = 100

func sessionNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, CodeSessionNotFound, "기기를 찾을 수 없어요")
}

// accessDTO 는 접속 기록 한 건이다.
type accessDTO struct {
	At          time.Time `json:"at"`
	DeviceLabel string    `json:"deviceLabel"`
}

// sessionDTO 는 세션 한 건의 응답 모양이다.
//
// 🔴 필드명은 앱과 합의된 계약이다(docs/openapi.yaml). nullable 인 두 필드를 포인터로
// 둔 것은 **키를 빼지 않고 명시적으로 null 을 내보내기 위해서다** — omitempty 로 키를
// 지우면 앱이 "아직 안 내려온 값" 과 "없는 값" 을 구별할 수 없다.
type sessionDTO struct {
	SessionID string `json:"sessionId"`
	// DeviceLabel 은 ⚠️ **추정값**이다. 앱에서 단정적인 문구로 감싸지 마라
	// ("이 기기는 확실히 …") — 근거는 session.go 의 DeviceLabel 주석에 있다.
	DeviceLabel string `json:"deviceLabel"`
	// UserAgent 는 추정의 근거가 된 원문이다. 추정이 빗나갔을 때 사람이 직접 읽으라고 둔다.
	UserAgent string    `json:"userAgent"`
	CreatedAt time.Time `json:"createdAt"`
	// LastSeenAt 은 ⚠️ **날짜 단위로만 정확하다**(Session.LastSeenAt 주석).
	LastSeenAt time.Time `json:"lastSeenAt"`
	// Current 는 지금 이 요청을 보낸 기기인지다. 액세스 토큰의 sid 로 가린다.
	Current   bool       `json:"current"`
	RevokedAt *time.Time `json:"revokedAt"`
	// AccessibleUntilAtMost 는 끊긴 기기가 **늦어도 언제까지** 앱을 쓸 수 있는지다.
	// 살아 있는 세션이면 null 이다 — 끊기 전에는 한계가 없기 때문이다(계속 갱신한다).
	// ⚠️ 정확한 시각이 아니라 상한이다. Session.AccessibleUntilAtMost 주석을 보라.
	AccessibleUntilAtMost *time.Time  `json:"accessibleUntilAtMost"`
	RecentAccesses        []accessDTO `json:"recentAccesses"`
}

func toSessionDTO(s Session, currentSessionID string) sessionDTO {
	out := sessionDTO{
		SessionID:   s.ID,
		DeviceLabel: s.DeviceLabel,
		UserAgent:   s.UserAgent,
		CreatedAt:   s.CreatedAt.UTC(),
		LastSeenAt:  s.LastSeenAt.UTC(),
		// 🔴 빈 sid 끼리 같다고 판정하지 않는다. 세션 장치 이전에 발급된 액세스 토큰은
		// sid 가 비어 있는데, 그것을 sid 없는 세션과 맞다고 보면 **엉뚱한 줄이 「지금 이
		// 기기」로 표시된다.** 사용자는 그 줄만 남기고 나머지를 끊는다.
		Current: currentSessionID != "" && s.ID == currentSessionID,
		// 비어 있어도 null 이 아니라 [] 로 나가야 앱이 length 를 그냥 읽는다.
		RecentAccesses: make([]accessDTO, 0, len(s.Accesses)),
	}
	if !s.RevokedAt.IsZero() {
		revoked := s.RevokedAt.UTC()
		out.RevokedAt = &revoked
	}
	if until, ok := s.AccessibleUntilAtMost(); ok {
		u := until.UTC()
		out.AccessibleUntilAtMost = &u
	}
	for _, a := range s.Accesses {
		out.RecentAccesses = append(out.RecentAccesses, accessDTO{At: a.At.UTC(), DeviceLabel: a.DeviceLabel})
	}
	return out
}

// listSessions 는 GET /auth/sessions — 로그인된 기기 목록이다.
//
// 이 화면이 답해야 하는 질문은 둘뿐이다. 「목록의 어느 줄이 잃어버린 기기인가」와
// 「끊었는데 진짜 끊긴 건가」. deviceLabel·lastSeenAt·recentAccesses 가 앞을 맡고,
// revokedAt·accessibleUntilAtMost 가 뒤를 맡는다.
func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	id := UserID(r.Context())
	if id == "" {
		unauthorized(w)
		return
	}
	// 계정을 읽는 이유는 **TokenVersion 때문**이다. 비밀번호 변경으로 이미 죽은 세션을
	// 「살아 있음」으로 보여 주지 않으려면 현재 버전을 알아야 한다(visible).
	//
	// ⚠️ 여기서 IsActive·MustChangePassword 로 막지 않는다. 임시 비밀번호를 아직 안
	// 바꿨거나 방금 비활성된 계정이라도 **잃어버린 기기를 끊는 것만은 할 수 있어야 한다.**
	// 이 기능이 존재하는 이유가 그 상황이다(Register 주석).
	a, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		// 토큰은 멀쩡한데 계정 문서가 없다. 404 가 아니라 401 이다 — 앱은 401 만 세션
		// 만료로 처리한다(users.go 의 me 와 같은 판단).
		unauthorized(w)
		return
	}
	if err != nil {
		internalError(w)
		return
	}
	items, err := h.sessions.ListSessions(r.Context(), id, maxListedSessions)
	if err != nil {
		internalError(w)
		return
	}
	now := h.now()
	current := SessionID(r.Context())
	out := make([]sessionDTO, 0, len(items))
	for _, s := range items {
		if !visible(s, a.TokenVersion, now) {
			continue
		}
		out = append(out, toSessionDTO(s, current))
	}
	// 세션 목록은 사용자마다 다른 응답이라 앞단 캐시에 남으면 남의 요청에 실려 나갈 수 있다.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, struct {
		Sessions []sessionDTO `json:"sessions"`
	}{out})
}

// revokeSessionResponse 는 DELETE /auth/sessions/{sessionId} 의 응답이다.
//
// 🔴 **204 가 아니라 200 + 본문인 이유가 accessibleUntilAtMost 다.** 이 기능은 "끊었다"
// 라고만 말하면 절반만 한 것이다 — 액세스 토큰을 검증하지 않으므로 끊은 기기가 최대
// AccessTokenTTL 동안 계속 쓸 수 있고, 사용자는 그 사실을 알아야 한다. 앱이 「이 기기는
// 00:49까지 쓸 수 있어요」라고 말할 근거가 이 필드다.
type revokeSessionResponse struct {
	SessionID string    `json:"sessionId"`
	RevokedAt time.Time `json:"revokedAt"`
	// ⚠️ 상한이지 정확한 시각이 아니다(Session.AccessibleUntilAtMost 주석).
	AccessibleUntilAtMost time.Time `json:"accessibleUntilAtMost"`
	// Current 가 참이면 **지금 이 기기를 끊은 것**이다. 앱은 저장된 토큰을 버리고
	// 로그인 화면으로 가야 한다 — 안 그러면 남은 액세스 토큰으로 15분 더 돌아다닌다.
	Current bool `json:"current"`
}

// revokeSession 은 DELETE /auth/sessions/{sessionId} — 기기 하나 끊기다.
//
// 문서를 지우지 않고 끊은 시각을 남긴다. 이유는 Session.RevokedAt 주석에 있다.
func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := UserID(r.Context())
	if id == "" {
		unauthorized(w)
		return
	}
	sid := r.PathValue("sessionId")
	// 🔴 모양이 이상한 id 도 400 이 아니라 404 다. 400 과 404 를 가르면 「이 id 는 형식은
	// 맞다」는 정보가 새고, 무엇보다 사용자가 고칠 수 있는 것이 없다.
	if !validSessionID(sid) {
		sessionNotFound(w)
		return
	}
	a, err := h.store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		unauthorized(w)
		return
	}
	if err != nil {
		internalError(w)
		return
	}
	// 🔴 **남의 세션은 여기서 걸린다.** 저장소가 userID 아래에서만 찾으므로 다른 사용자의
	// id 는 ErrSessionNotFound 가 되고, 그대로 404 가 된다. "찾아서 소유자를 비교하는"
	// 구조가 아니라 **애초에 남의 것을 찾을 수 없는** 구조라는 점이 중요하다 — 비교를
	// 빠뜨릴 자리가 없다.
	s, err := h.sessions.GetSession(r.Context(), id, sid)
	if errors.Is(err, ErrSessionNotFound) {
		sessionNotFound(w)
		return
	}
	if err != nil {
		internalError(w)
		return
	}
	now := h.now()
	// 목록에 없는 세션은 끊기에서도 없다. 같은 판정을 쓰지 않으면 목록에 안 보이는 것을
	// 끊었다는 200 이 나가고, 사용자는 무엇을 끊었는지 알 수 없게 된다.
	if !visible(s, a.TokenVersion, now) {
		sessionNotFound(w)
		return
	}
	revokedAt := s.RevokedAt
	if revokedAt.IsZero() {
		if err := h.sessions.RevokeSession(r.Context(), id, sid, now); err != nil {
			internalError(w)
			return
		}
		revokedAt = now
	}
	// 🔴 이미 끊긴 세션을 다시 끊어도 **처음 끊은 시각을 그대로 돌려준다.** 지금 시각으로
	// 덮어쓰면 accessibleUntilAtMost 가 누를 때마다 뒤로 밀려서, 사용자는 이미 지난
	// 시각을 다시 기다리게 된다 — 「끊었는데 계속 쓸 수 있다」로 읽히는 화면이 된다.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, revokeSessionResponse{
		SessionID:             sid,
		RevokedAt:             revokedAt.UTC(),
		AccessibleUntilAtMost: revokedAt.Add(AccessTokenTTL).UTC(),
		Current:               SessionID(r.Context()) != "" && SessionID(r.Context()) == sid,
	})
}
