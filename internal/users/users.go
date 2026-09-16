// Package users 는 사용자 계정 문서(`users/{userId}`)의 주인이다. 문서의 필드 이름과
// 읽기·쓰기가 전부 이 패키지에 모여 있고, 인증 흐름은 auth 가 선언한 인터페이스를 통해서만
// 이 문서를 만진다(internal/auth/account.go).
//
// **초대 전용 서비스라 가입 API 가 없다.** 계정이 생기는 경로는 `cmd/create-user` 하나뿐이고,
// 그 도구도 여기의 Store.Create 를 거친다 — 임시 비밀번호를 발급하고 mustChangePassword 를
// 켜 둔 채로 만든다. 공개 회원가입 엔드포인트를 여기에 더하려는 사람은 이메일 유일성 락과
// 초대 정책을 함께 다시 봐야 한다(store.go 의 EmailLockCollection).
//
// HTTP 로 열려 있는 것은 GET /users/me 하나다.
package users

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
)

// auth 의 같은 이름 코드와 값이 같아야 한다. 앱은 코드 문자열 하나로 분기하므로,
// 한쪽만 고치면 같은 상황에서 화면이 갈린다.
const CodeAccountDisabled = "ACCOUNT_DISABLED"

// profileStore 는 이 핸들러가 Store 에서 쓰는 전부다. 인터페이스로 좁혀 두면 테스트가
// 에뮬레이터 없이 돌고, 핸들러가 계정 쓰기 메서드에 손댈 수 없게 된다.
type profileStore interface {
	GetProfile(context.Context, string) (User, error)
}
type Handler struct{ store profileStore }

type profileDTO struct {
	UserID             string    `json:"userId"`
	Email              string    `json:"email"`
	UserName           string    `json:"userName"`
	MustChangePassword bool      `json:"mustChangePassword"`
	CreatedAt          time.Time `json:"createdAt"`
}

// toDTO 는 사용자 계정을 응답 모양으로 바꾼다.
//
// 🔴 **사용자 문서가 응답으로 나가는 길은 이 함수 하나뿐이다.** User 에는 PasswordHash 와
// TokenVersion 이 들어 있고, 그 구조체를 그대로 WriteJSON 에 넘기면 bcrypt 해시가 앱까지
// 나간다. 나가고 나면 회수할 수 없다 — 함수를 하나로 좁혀 두면 엔드포인트가 늘어도 그
// 사고가 구조적으로 일어나지 않는다. 여기에 PasswordHash 를 더하지 마라.
//
// 🔴 필드명은 앱과 합의된 계약이다(docs/openapi.yaml). 이름을 바꾸면 이미 깔린 앱의
// 프로필 화면이 빈칸이 된다.
func toDTO(u User) profileDTO {
	return profileDTO{u.UserID, u.Email, u.UserName, u.MustChangePassword, u.CreatedAt}
}

func Register(mux *http.ServeMux, fs *firestore.Client) {
	h := &Handler{NewStore(fs)}
	mux.HandleFunc("GET /users/me", h.me)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	id := auth.UserID(r.Context())
	if id == "" {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "로그인이 필요해요")
		return
	}
	u, err := h.store.GetProfile(r.Context(), id)
	// 🔴 토큰은 멀쩡한데 계정 문서가 없는 경우다(지워졌거나 잘못 만들어졌다). **404 가
	// 아니라 401 을 준다** — 앱은 401 만 세션 만료로 처리해서 토큰을 버리고 로그인 화면으로
	// 보낸다. 404 를 주면 "내 프로필이 없다" 는 상태로 계속 돌면서, 사용자는 지워진 계정의
	// 토큰을 쥔 채 빈 화면과 재시도를 반복한다.
	if errors.Is(err, auth.ErrAccountNotFound) {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "로그인이 필요해요")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "서버 오류가 생겼어요")
		return
	}
	if !u.IsActive {
		httpx.WriteError(w, http.StatusForbidden, CodeAccountDisabled, "사용할 수 없는 계정이에요")
		return
	}
	// 프로필은 사용자마다 다른 응답이라 앞단 캐시에 남으면 남의 요청에 실려 나갈 수 있다
	// (internal/httpx 의 noStore 와 같은 결).
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, toDTO(u))
}
