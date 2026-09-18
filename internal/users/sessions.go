package users

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sslim7/nature-was/internal/auth"
)

// SessionCollection 은 로그인 세션(=기기 하나)이 사는 **하위 컬렉션** 이름이다.
// 전체 경로는 `users/{userId}/sessions/{sessionId}` 다.
//
// 최상위 `sessions` 컬렉션에 userId 필드를 두는 대신 하위 컬렉션을 고른 이유:
//
//  1. 🔴 **소유자 확인을 빠뜨릴 자리가 없다.** 경로에 userId 가 들어가므로 남의 세션은
//     애초에 찾아지지 않는다. 최상위 컬렉션이면 매 조회마다 `doc.userId == 그 사용자`
//     비교를 손으로 붙여야 하고, 한 군데라도 빠지면 남의 기기를 끊을 수 있게 되는데
//     **그 실수는 에러를 내지 않는다.**
//  2. 이 저장소가 사용자별 데이터를 이미 그렇게 둔다(`users/{uid}/calls/{id}`).
//  3. 목록 조회가 컬렉션 하나를 통째로 읽는 일이 되어 복합 인덱스가 필요 없다.
//
// ⚠️ 하위 컬렉션 문서는 **상위 문서를 지워도 남는다.** 계정을 지우는 경로가 생기면
// 여기를 함께 지워야 한다 — 남아 있으면 같은 id 로 계정을 다시 만들었을 때 남의 기기
// 목록이 딸려 온다. 지금은 계정을 지우는 경로 자체가 없다.
const SessionCollection = "sessions"

// sessionDoc 은 Firestore 문서다. 필드 이름의 정본이 여기다.
//
// revokedAt 은 **살아 있는 세션의 문서에는 아예 없다.** 읽으면 Go 의 제로값이 되고
// auth 는 그것을 "살아 있음" 으로 읽는다(auth.Session.RevokedAt). 여기에 "없으면 현재
// 시각" 같은 보정을 넣으면 모든 세션이 끊긴 것으로 보인다.
type sessionDoc struct {
	DeviceLabel  string      `firestore:"deviceLabel"`
	UserAgent    string      `firestore:"userAgent"`
	CreatedAt    time.Time   `firestore:"createdAt"`
	LastSeenAt   time.Time   `firestore:"lastSeenAt"`
	RevokedAt    time.Time   `firestore:"revokedAt"`
	TokenVersion int         `firestore:"tokenVersion"`
	Accesses     []accessDoc `firestore:"accesses"`
}

type accessDoc struct {
	At          time.Time `firestore:"at"`
	DeviceLabel string    `firestore:"deviceLabel"`
}

// 🔴 이 한 줄이 auth 와의 세션 계약(internal/auth/session.go)을 **컴파일 타임에** 붙잡는다.
// 배선은 main.go 가 하므로, 이 선언이 없으면 어긋남이 런타임까지 미뤄진다 —
// 그리고 그 런타임은 "로그인" 이다.
var _ auth.SessionStore = (*Store)(nil)

func (s *Store) sessions(userID string) *firestore.CollectionRef {
	return s.fs.Collection(Collection).Doc(userID).Collection(SessionCollection)
}

// CreateSession 은 세션 문서를 만들고 그 id 를 돌려준다. id 는 Firestore 가 만드는
// 20자 난수다 — 순번이면 남의 세션 id 를 추측할 수 있고, 추측할 수 있으면 404 로 감춘
// 것이 의미를 잃는다.
//
// 🔴 map 으로 쓴다(이 패키지의 Create 와 같은 이유). 구조체를 그대로 Set 하면 제로값인
// revokedAt 이 **서기 1년 타임스탬프**로 기록되고, 그것은 IsZero() 가 참이라 동작은
// 하지만 콘솔에서 보면 "끊긴 시각이 있는 살아 있는 세션" 이라는 읽을 수 없는 문서가 된다.
func (s *Store) CreateSession(ctx context.Context, userID string, sess auth.Session) (string, error) {
	if userID == "" {
		return "", auth.ErrAccountNotFound
	}
	accesses := make([]map[string]any, 0, len(sess.Accesses))
	for _, a := range sess.Accesses {
		accesses = append(accesses, map[string]any{"at": a.At, "deviceLabel": a.DeviceLabel})
	}
	ref := s.sessions(userID).NewDoc()
	if _, err := ref.Create(ctx, map[string]any{
		"deviceLabel":  sess.DeviceLabel,
		"userAgent":    sess.UserAgent,
		"createdAt":    sess.CreatedAt,
		"lastSeenAt":   sess.LastSeenAt,
		"tokenVersion": sess.TokenVersion,
		"accesses":     accesses,
	}); err != nil {
		return "", err
	}
	return ref.ID, nil
}

// GetSession 은 세션 하나를 읽는다. 없으면 auth.ErrSessionNotFound.
//
// 🔴 **다른 사용자의 세션 id 를 넘겨도 여기서는 "없음" 이다.** 경로가
// `users/{userID}/sessions/{sessionID}` 라 남의 문서에 닿지 않기 때문이다 — 읽은 다음
// 소유자를 비교하는 구조가 아니라 애초에 못 읽는 구조다.
func (s *Store) GetSession(ctx context.Context, userID, sessionID string) (auth.Session, error) {
	// 빈 값을 Doc() 에 넘기면 Firestore 가 경로 에러를 내는데, 그 에러는 호출부에서
	// 500 으로 나간다. "없음" 이 맞는 답이다.
	if userID == "" || sessionID == "" {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	snap, err := s.sessions(userID).Doc(sessionID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return auth.Session{}, auth.ErrSessionNotFound
		}
		return auth.Session{}, err
	}
	return toSession(snap)
}

// ListSessions 는 최근 접속 순으로 최대 limit 건을 돌려준다.
//
// 인덱스: users/{uid}/sessions(lastSeenAt desc) — 단일 필드라 Firestore 가 자동으로 만든다.
//
// ⚠️ **OrderBy 필드가 없는 문서는 결과에서 빠진다.** Firestore 의 정렬 질의는 그 필드가
// 있는 문서만 본다. 지금 세션 문서를 만드는 경로는 CreateSession 하나뿐이고 거기서 항상
// lastSeenAt 을 쓰므로 빠지는 문서는 없다. 콘솔에서 손으로 문서를 만들면 목록에서
// **조용히 사라진다**는 뜻이기도 하다.
func (s *Store) ListSessions(ctx context.Context, userID string, limit int) ([]auth.Session, error) {
	if userID == "" {
		return nil, auth.ErrAccountNotFound
	}
	if limit <= 0 {
		return nil, nil
	}
	snaps, err := s.sessions(userID).
		OrderBy("lastSeenAt", firestore.Desc).
		Limit(limit).
		Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]auth.Session, 0, len(snaps))
	for _, snap := range snaps {
		sess, err := toSession(snap)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

// TouchSession 은 마지막 접속·기기 표시·접속 기록만 갱신한다.
//
// 🔴 **Set 이 아니라 Update 다.** 문서를 통째로 다시 쓰면 그 사이에 들어온 revokedAt 이
// 지워진다 — 사용자는 끊었다고 믿는데 그 기기는 계속 도는, 이 기능에서 가장 나쁜 상태가
// 된다. 갱신과 끊기는 실제로 같은 순간에 겹칠 수 있다(15분마다 갱신하는 기기를 끊는다).
//
// 없는 문서면 Update 가 NotFound 를 낸다. 끊긴 뒤 문서가 지워진 경우라 호출부는 이
// 에러를 무시해도 된다 — 갱신 자체는 이미 세션을 확인한 뒤이기 때문이다.
func (s *Store) TouchSession(ctx context.Context, userID, sessionID string, lastSeenAt time.Time, deviceLabel string, accesses []auth.Access) error {
	if userID == "" || sessionID == "" {
		return auth.ErrSessionNotFound
	}
	rows := make([]map[string]any, 0, len(accesses))
	for _, a := range accesses {
		rows = append(rows, map[string]any{"at": a.At, "deviceLabel": a.DeviceLabel})
	}
	_, err := s.sessions(userID).Doc(sessionID).Update(ctx, []firestore.Update{
		{Path: "lastSeenAt", Value: lastSeenAt},
		{Path: "deviceLabel", Value: deviceLabel},
		{Path: "accesses", Value: rows},
	})
	if status.Code(err) == codes.NotFound {
		return auth.ErrSessionNotFound
	}
	return err
}

// RevokeSession 은 세션에 끊은 시각을 남긴다. 없으면 auth.ErrSessionNotFound.
//
// 🔴 **문서를 지우지 않는다.** 지우면 "없는 세션" 과 "끊긴 세션" 이 같은 상태가 되는데,
// 그 순간 없는 세션을 한 번이라도 너그럽게 다루면 끊기가 통째로 풀린다. 그리고 지우면
// 사용자가 "끊었는데 진짜 끊겼는가" 를 확인할 화면이 사라진다.
//
// 🔴 **끊은 시각 하나만 쓴다.** 다른 필드를 함께 쓰면 동시에 들어온 TouchSession 과
// 서로를 덮는다.
func (s *Store) RevokeSession(ctx context.Context, userID, sessionID string, at time.Time) error {
	if userID == "" || sessionID == "" {
		return auth.ErrSessionNotFound
	}
	_, err := s.sessions(userID).Doc(sessionID).Update(ctx, []firestore.Update{
		{Path: "revokedAt", Value: at},
	})
	if status.Code(err) == codes.NotFound {
		return auth.ErrSessionNotFound
	}
	return err
}

func toSession(snap *firestore.DocumentSnapshot) (auth.Session, error) {
	var doc sessionDoc
	if err := snap.DataTo(&doc); err != nil {
		return auth.Session{}, err
	}
	accesses := make([]auth.Access, 0, len(doc.Accesses))
	for _, a := range doc.Accesses {
		accesses = append(accesses, auth.Access{At: a.At, DeviceLabel: a.DeviceLabel})
	}
	return auth.Session{
		ID:           snap.Ref.ID,
		DeviceLabel:  doc.DeviceLabel,
		UserAgent:    doc.UserAgent,
		CreatedAt:    doc.CreatedAt,
		LastSeenAt:   doc.LastSeenAt,
		RevokedAt:    doc.RevokedAt,
		TokenVersion: doc.TokenVersion,
		Accesses:     accesses,
	}, nil
}
