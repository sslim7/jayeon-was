package sms

import (
	"errors"
	"net/http"
	"strconv"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/messaging"
)

type Handler struct{ store Store }

func Register(mux *http.ServeMux, fs *firestore.Client, guard func(http.Handler) http.Handler) {
	register(mux, &Handler{store: &FirestoreStore{Client: fs}}, guard)
	registerHistory(mux, fs, guard)
}
func register(mux *http.ServeMux, h *Handler, guard func(http.Handler) http.Handler) {
	for pattern, fn := range map[string]http.HandlerFunc{
		"POST /sms/campaigns": h.create, "GET /sms/campaigns": h.list, "GET /sms/campaigns/{id}": h.get, "GET /sms/campaigns/{id}/recipients": h.getRecipients,
		"POST /sms/campaigns/{id}/start": h.start, "POST /sms/campaigns/{id}/cancel": h.cancel, "PATCH /sms/campaigns/{id}/recipients/{recipientId}": h.patch, "POST /sms/campaigns/{id}/recipients/{recipientId}/retry": h.retry,
	} {
		mux.Handle(pattern, guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if auth.UserID(r.Context()) == "" {
				httpx.WriteError(w, 401, httpx.CodeUnauthorized, "로그인이 필요해요")
				return
			}
			fn(w, r)
		})))
	}
}
func fail(w http.ResponseWriter, err error) {
	// 🔴 messaging.ValidationError 는 사용자에게 그대로 보여 줄 문장을 들고 온다(첨부 합계 초과 등).
	// 이걸 먼저 잡지 않으면 아래 어느 case 에도 안 걸려 default 로 떨어지고, 이유가 분명한 거절이
	// 「서버 오류가 생겼어요」 500 으로 둔갑한다.
	var invalid messaging.ValidationError
	if errors.As(err, &invalid) {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, invalid.Message)
		return
	}
	switch {
	case errors.Is(err, ErrValidation):
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "요청 값이 올바르지 않아요")
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, 404, "NOT_FOUND", "발송 정보 또는 수신자를 찾을 수 없어요")
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, 409, "STATE_CONFLICT", "현재 발송 상태에서 처리할 수 없어요")
	case errors.Is(err, ErrIdempotency):
		httpx.WriteError(w, 409, "IDEMPOTENCY_CONFLICT", "같은 요청 ID의 내용이 달라요")
	default:
		httpx.WriteError(w, 500, httpx.CodeInternal, "서버 오류가 생겼어요")
	}
}
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	c, created, err := h.store.Create(r.Context(), auth.UserID(r.Context()), req)
	if err != nil {
		fail(w, err)
		return
	}
	code := 200
	if created {
		code = 201
	}
	httpx.WriteJSON(w, code, c)
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fail(w, ErrValidation)
			return
		}
		limit = n
	}
	items, next, err := h.store.List(r.Context(), auth.UserID(r.Context()), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		fail(w, err)
		return
	}
	httpx.WriteJSON(w, 200, struct {
		Items      []Campaign `json:"items"`
		NextCursor *string    `json:"nextCursor"`
	}{items, next})
}
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.Get(r.Context(), auth.UserID(r.Context()), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	httpx.WriteJSON(w, 200, d.Campaign)
}
func (h *Handler) getRecipients(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.Get(r.Context(), auth.UserID(r.Context()), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	httpx.WriteJSON(w, 200, struct {
		Items []CampaignRecipient `json:"items"`
	}{d.Recipients})
}
func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, action string) {
	var req PatchRequest
	if action == "patch" && !httpx.DecodeJSON(w, r, &req) {
		return
	}
	out, err := h.store.Apply(r.Context(), auth.UserID(r.Context()), r.PathValue("id"), r.PathValue("recipientId"), action, req)
	if err != nil {
		fail(w, err)
		return
	}
	if action == "start" || action == "cancel" {
		httpx.WriteJSON(w, 200, out.Campaign)
	} else {
		httpx.WriteJSON(w, 200, out)
	}
}
func (h *Handler) start(w http.ResponseWriter, r *http.Request)  { h.mutate(w, r, "start") }
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) { h.mutate(w, r, "cancel") }
func (h *Handler) patch(w http.ResponseWriter, r *http.Request)  { h.mutate(w, r, "patch") }
func (h *Handler) retry(w http.ResponseWriter, r *http.Request)  { h.mutate(w, r, "retry") }
