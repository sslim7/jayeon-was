package users

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/auth"
)

// sessionUser 는 이 테스트 전용 사용자 id 를 만든다. 하위 컬렉션이라 정리도 사용자
// 단위로 하면 되고, 같은 에뮬레이터에서 여러 테스트가 겹치지 않는다.
func sessionUser(t *testing.T, s *Store) string {
	t.Helper()
	uid := fmt.Sprintf("session-user-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		snaps, err := s.sessions(uid).Documents(ctx).GetAll()
		if err != nil {
			return
		}
		for _, snap := range snaps {
			snap.Ref.Delete(ctx)
		}
	})
	return uid
}

// TestSessionStoreLifecycle 은 Firestore 위에서의 세션 저장 규칙을 통째로 본다.
// 페이크로는 확인할 수 없는 것들이라 에뮬레이터가 있을 때만 돈다.
//
// 🔴 이 테스트가 지키는 가장 중요한 두 가지:
//
//   - **TouchSession 이 revokedAt 을 지우지 않는다.** 갱신과 끊기는 실제로 같은 순간에
//     겹친다(15분마다 갱신하는 기기를 끊는다). 구현이 Update 대신 Set 으로 바뀌면 끊은
//     세션이 되살아나는데, 사용자는 끊었다고 믿고 그 기기는 계속 돈다. 아무 에러도 없다.
//   - **남의 세션 id 는 "없음" 이다.** 경로에 userId 가 들어가는 하위 컬렉션이라 애초에
//     닿지 않는다. 최상위 컬렉션으로 옮기면 여기서 먼저 깨진다.
func TestSessionStoreLifecycle(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	uid := sessionUser(t, s)
	other := sessionUser(t, s)

	base := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	sid, err := s.CreateSession(ctx, uid, auth.Session{
		DeviceLabel:  "네이처 앱 1.4.0 · 안드로이드",
		UserAgent:    "Mozilla/5.0 (Linux; Android 14) NatureApp/1.4.0",
		CreatedAt:    base,
		LastSeenAt:   base,
		TokenVersion: 3,
		Accesses:     []auth.Access{{At: base, DeviceLabel: "네이처 앱 1.4.0 · 안드로이드"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Fatal("세션 id 가 비었다")
	}

	got, err := s.GetSession(ctx, uid, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sid || got.DeviceLabel != "네이처 앱 1.4.0 · 안드로이드" || got.TokenVersion != 3 {
		t.Fatal(got)
	}
	// 🔴 새 세션 문서에는 revokedAt 필드가 아예 없고, 읽으면 제로값이어야 한다.
	// 여기가 깨지면 모든 세션이 「끊긴 것」으로 보여 갱신이 전부 401 이 된다.
	if !got.RevokedAt.IsZero() {
		t.Fatal("새 세션이 끊긴 상태로 읽혔다", got.RevokedAt)
	}
	if !got.CreatedAt.Equal(base) || !got.LastSeenAt.Equal(base) || len(got.Accesses) != 1 || !got.Accesses[0].At.Equal(base) {
		t.Fatal("왕복에서 값이 어긋났다", got)
	}

	// 🔴 남의 세션 id 는 "없음" 이다. 경로에 userId 가 들어가 애초에 닿지 않는다.
	if _, err := s.GetSession(ctx, other, sid); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal("남의 세션이 읽혔다", err)
	}
	if _, err := s.GetSession(ctx, uid, "없는세션"); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, uid, ""); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal("빈 세션 id 가 경로 에러로 새어 나갔다", err)
	}

	// 끊는다.
	revokedAt := base.Add(time.Hour)
	if err := s.RevokeSession(ctx, uid, sid, revokedAt); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSession(ctx, uid, sid)
	if err != nil || !got.RevokedAt.Equal(revokedAt) {
		t.Fatal(got, err)
	}

	// 🔴 **끊은 뒤의 Touch 가 revokedAt 을 지우면 안 된다.**
	later := revokedAt.Add(time.Minute)
	if err := s.TouchSession(ctx, uid, sid, later, "Chrome · macOS", []auth.Access{
		{At: later, DeviceLabel: "Chrome · macOS"},
		{At: base, DeviceLabel: "네이처 앱 1.4.0 · 안드로이드"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetSession(ctx, uid, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RevokedAt.Equal(revokedAt) {
		t.Fatal("갱신이 끊은 시각을 지웠다 — 끊은 기기가 되살아난다", got.RevokedAt)
	}
	if !got.LastSeenAt.Equal(later) || got.DeviceLabel != "Chrome · macOS" || len(got.Accesses) != 2 {
		t.Fatal("갱신이 반영되지 않았다", got)
	}
	// 기록 순서(최신이 앞)가 왕복에서 보존되어야 한다.
	if !got.Accesses[0].At.Equal(later) || !got.Accesses[1].At.Equal(base) {
		t.Fatal("접속 기록 순서가 뒤집혔다", got.Accesses)
	}

	// 없는 문서에 대한 쓰기는 "없음" 이다. 500 으로 새면 갱신이 서버 오류가 된다.
	if err := s.TouchSession(ctx, uid, "없는세션", later, "x", nil); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, uid, "없는세션", later); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatal(err)
	}
}

// TestListSessionsOrderAndLimit 은 목록이 **최근 접속 순**이고 상한을 지키는지 본다.
// 사용자가 잃어버린 기기를 찾을 때 가장 먼저 보는 것이 이 순서다.
//
// 남의 세션이 섞이지 않는지도 함께 본다 — 하위 컬렉션이라 섞일 수 없지만, 최상위
// 컬렉션으로 옮기려는 사람이 여기서 먼저 걸린다.
func TestListSessionsOrderAndLimit(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	uid := sessionUser(t, s)
	other := sessionUser(t, s)

	base := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 3 {
		at := base.Add(time.Duration(i) * time.Hour)
		id, err := s.CreateSession(ctx, uid, auth.Session{
			DeviceLabel: fmt.Sprintf("기기 %d", i), CreatedAt: at, LastSeenAt: at, TokenVersion: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.CreateSession(ctx, other, auth.Session{DeviceLabel: "남의 기기", CreatedAt: base, LastSeenAt: base}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListSessions(ctx, uid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("세션 %d건 — 남의 세션이 섞였거나 빠졌다", len(list))
	}
	// 최근 접속이 앞이다.
	if list[0].ID != ids[2] || list[2].ID != ids[0] {
		t.Fatal("최근 접속 순이 아니다", list[0].ID, list[1].ID, list[2].ID)
	}
	limited, err := s.ListSessions(ctx, uid, 2)
	if err != nil || len(limited) != 2 || limited[0].ID != ids[2] {
		t.Fatal(limited, err)
	}
	// 상한이 0 이하면 읽지 않는다. 호출부 실수로 컬렉션 전체를 훑지 않게 한다.
	if got, err := s.ListSessions(ctx, uid, 0); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
