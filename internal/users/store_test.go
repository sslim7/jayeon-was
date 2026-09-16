package users

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
)

func emulatorStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore 에뮬레이터가 없다")
	}
	fs, err := firestore.NewClient(context.Background(), "demo-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return NewStore(fs)
}
func TestStoreAccountLifecycle(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	email := fmt.Sprintf("User-%d@Example.COM", time.Now().UnixNano())
	now := time.Now()
	u := User{Email: email, UserName: "사용자", PasswordHash: "old-hash", IsActive: true, MustChangePassword: true, CreatedAt: now, UpdatedAt: now}
	id, err := s.Create(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.fs.Collection(Collection).Doc(id).Delete(ctx)
		s.fs.Collection(EmailLockCollection).Doc(strings.ToLower(email)).Delete(ctx)
	})
	if _, err := s.Create(ctx, u); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("중복 이메일=%v", err)
	}
	found, err := s.FindByEmail(ctx, "  "+strings.ToLower(email)+"  ")
	if err != nil || found.UserID != id || found.TokenVersion != 0 {
		t.Fatal(found, err)
	}
	if _, err := s.FindByEmail(ctx, "missing-"+email); !errors.Is(err, auth.ErrAccountNotFound) {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, id, "new-hash", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	found, err = s.Get(ctx, id)
	if err != nil || found.TokenVersion != 1 || found.MustChangePassword || found.PasswordHash != "new-hash" {
		t.Fatal(found, err)
	}
	if err := s.SetPassword(ctx, id, "next-hash", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	found, err = s.Get(ctx, id)
	if err != nil || found.TokenVersion != 2 {
		t.Fatal(found, err)
	}
	mux := http.NewServeMux()
	Register(mux, s.fs)
	r := httptest.NewRequest("GET", "/users/me", nil)
	r = r.WithContext(auth.WithUserID(ctx, id))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Hash") || strings.Contains(w.Body.String(), "tokenVersion") {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.SetPassword(ctx, "missing-user", "hash", now); !errors.Is(err, auth.ErrAccountNotFound) {
		t.Fatal(err)
	}
}
func TestConcurrentEmailUniqueness(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	email := fmt.Sprintf("race-%d@example.com", time.Now().UnixNano())
	u := User{Email: email, IsActive: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { id, err := s.Create(ctx, u); results <- result{id, err} })
	}
	wg.Wait()
	close(results)
	success, duplicate := 0, 0
	for r := range results {
		if r.err == nil {
			success++
			t.Cleanup(func() { s.fs.Collection(Collection).Doc(r.id).Delete(ctx) })
		} else if errors.Is(r.err, ErrEmailTaken) {
			duplicate++
		} else {
			t.Fatal(r.err)
		}
	}
	t.Cleanup(func() { s.fs.Collection(EmailLockCollection).Doc(email).Delete(ctx) })
	if success != 1 || duplicate != 1 {
		t.Fatal(success, duplicate)
	}
}
func TestLegacyTokenVersion(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	ref := s.fs.Collection(Collection).NewDoc()
	_, err := ref.Set(ctx, map[string]any{"email": "legacy@example.com", "isActive": true, "mustChangePassword": true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ref.Delete(ctx) })
	a, err := s.Get(ctx, ref.ID)
	if err != nil || a.TokenVersion != 0 {
		t.Fatal(a, err)
	}
	if err := s.SetPassword(ctx, ref.ID, "hash", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err = s.Get(ctx, ref.ID)
	if err != nil || a.TokenVersion != 1 || a.MustChangePassword {
		t.Fatal(a, err)
	}
}
