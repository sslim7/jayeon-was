package userguard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
)

type fakeAccounts struct {
	account auth.Account
	err     error
	calls   int
}

func (f *fakeAccounts) Get(_ context.Context, _ string) (auth.Account, error) {
	f.calls++
	return f.account, f.err
}

func TestGuardAccessConditions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      string
		account auth.Account
		err     error
		status  int
		code    string
	}{
		{name: "no bearer", status: 401, code: "UNAUTHORIZED"},
		{name: "deleted user", id: "owner", err: auth.ErrAccountNotFound, status: 401, code: "UNAUTHORIZED"},
		{name: "storage failure", id: "owner", err: errors.New("private backend detail"), status: 500, code: "INTERNAL_ERROR"},
		{name: "disabled", id: "owner", account: auth.Account{}, status: 403, code: "ACCOUNT_DISABLED"},
		{name: "temporary password", id: "owner", account: auth.Account{IsActive: true, MustChangePassword: true}, status: 403, code: "PASSWORD_CHANGE_REQUIRED"},
		{name: "active", id: "owner", account: auth.Account{IsActive: true}, status: 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAccounts{account: tc.account, err: tc.err}
			called := false
			h := New(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if auth.UserID(r.Context()) != tc.id {
					t.Fatal("owner context lost")
				}
				w.WriteHeader(204)
			}))
			r := httptest.NewRequest("GET", "/recipients", nil)
			if tc.id != "" {
				r = r.WithContext(auth.WithUserID(r.Context(), tc.id))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if called != (tc.status == 204) {
				t.Fatalf("next handler called=%v", called)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private response cacheable")
			}
			if tc.id == "" && store.calls != 0 {
				t.Fatal("unauthenticated request queried storage")
			}
			if tc.code != "" {
				var e httpx.Error
				if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
					t.Fatal(err)
				}
				if e.Code != tc.code {
					t.Fatalf("code=%q", e.Code)
				}
				if e.Message == "private backend detail" {
					t.Fatal("internal details leaked")
				}
			}
		})
	}
}
