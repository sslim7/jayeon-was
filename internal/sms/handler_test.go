package sms

import (
	"context"
	"errors"
	"github.com/sslim7/nature-was/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore struct {
	err error
	uid string
	req CreateRequest
}

func (f *fakeStore) Create(_ context.Context, u string, r CreateRequest) (Campaign, bool, error) {
	f.uid = u
	f.req = r
	return Campaign{ID: "created"}, true, f.err
}
func (f *fakeStore) Get(_ context.Context, u, id string) (document, error) {
	f.uid = u
	return document{}, f.err
}
func (f *fakeStore) List(_ context.Context, u string, l int, c string) ([]Campaign, *string, error) {
	f.uid = u
	return []Campaign{}, nil, f.err
}
func (f *fakeStore) Apply(_ context.Context, u, id, rid, a string, p PatchRequest) (Result, error) {
	f.uid = u
	return Result{}, f.err
}
func TestHandlerGuardAndErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{ErrValidation, 400, "VALIDATION_FAILED"}, {ErrConflict, 409, "STATE_CONFLICT"}, {ErrIdempotency, 409, "IDEMPOTENCY_CONFLICT"}, {ErrNotFound, 404, "NOT_FOUND"}, {errors.New("private phone message"), 500, "INTERNAL_ERROR"}, {nil, 201, "created"}} {
		f := &fakeStore{err: tc.err}
		mux := http.NewServeMux()
		register(mux, &Handler{f}, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), "owner")))
			})
		})
		r := httptest.NewRequest("POST", "/sms/campaigns", strings.NewReader(`{"requestId":"request_123","title":"모임","message":"hello","recipientIds":["one"]}`))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || w.Header().Get("Cache-Control") != "no-store" || f.uid != "owner" {
			t.Fatal(w.Code, w.Body.String(), f.uid)
		}
		if strings.Contains(w.Body.String(), "private phone") {
			t.Fatal("leaked internal error")
		}
	}
}
func TestRoutesRequireUser(t *testing.T) {
	mux := http.NewServeMux()
	register(mux, &Handler{&fakeStore{}}, func(h http.Handler) http.Handler { return h })
	for _, endpoint := range []struct{ method, path string }{{"POST", "/sms/campaigns"}, {"GET", "/sms/campaigns"}, {"GET", "/sms/campaigns/id"}, {"GET", "/sms/campaigns/id/recipients"}, {"POST", "/sms/campaigns/id/start"}, {"POST", "/sms/campaigns/id/cancel"}, {"PATCH", "/sms/campaigns/id/recipients/rid"}, {"POST", "/sms/campaigns/id/recipients/rid/retry"}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(endpoint.method, endpoint.path, nil))
		if w.Code != 401 {
			t.Fatal(endpoint, w.Code)
		}
	}
}
