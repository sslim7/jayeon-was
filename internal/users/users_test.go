package users

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/auth"
)

type fakeProfiles struct {
	user User
	err  error
}

func (s fakeProfiles) GetProfile(context.Context, string) (User, error) { return s.user, s.err }
func TestProfileResponse(t *testing.T) {
	u := User{UserID: "user-1", Email: "u@example.com", UserName: "사용자", PasswordHash: "secret-hash", TokenVersion: 42, IsActive: true, MustChangePassword: true, CreatedAt: time.Now()}
	b, err := json.Marshal(toDTO(u))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"passwordHash", "PasswordHash", "tokenVersion", "TokenVersion", "secret-hash"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("비밀 유출: %s", b)
		}
	}
	for _, tc := range []struct {
		name, id string
		store    fakeProfiles
		status   int
	}{
		{"미인증", "", fakeProfiles{user: u}, 401},
		{"활성", "user-1", fakeProfiles{user: u}, 200},
		{"비활성", "user-1", fakeProfiles{user: User{}}, 403},
		{"삭제됨", "user-1", fakeProfiles{err: auth.ErrAccountNotFound}, 401},
		{"저장소실패", "user-1", fakeProfiles{err: errors.New("private detail")}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler{store: tc.store}
			r := httptest.NewRequest("GET", "/users/me", nil)
			r = r.WithContext(auth.WithUserID(r.Context(), tc.id))
			w := httptest.NewRecorder()
			h.me(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private detail") {
				t.Fatal("내부 오류 노출")
			}
			if tc.status == 200 {
				var result map[string]any
				json.Unmarshal(w.Body.Bytes(), &result)
				if len(result) != 5 || result["userId"] != "user-1" || result["mustChangePassword"] != true {
					t.Fatal(result)
				}
			}
		})
	}
}
