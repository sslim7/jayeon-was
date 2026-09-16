package admin

import (
	"context"
	"errors"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/jayeon-was/internal/credentials"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Collection 은 어드민 계정 문서가 사는 곳이다. cmd/create-admin 도 같은 상수를 쓴다 —
// 컬렉션 이름이 두 곳에 문자열로 박히면 한쪽 오타가 "만들었는데 로그인이 안 된다" 가 된다.
const Collection = "admins"

// EmailLockCollection 은 이메일 유일성 락 문서가 사는 곳이다. 문서 ID 가 곧 이메일이다.
//
// **Firestore 에는 unique 제약이 없다.** "Where(email==) 로 없는 걸 확인하고 쓴다" 는
// 조회와 쓰기 사이에 틈이 있어 같은 이메일 두 건이 동시에 들어오면 둘 다 통과한다.
// 그러면 로그인 조회가 어느 문서를 집을지 알 수 없는 상태가 되고, 한쪽 계정의 비밀번호
// 변경이 다른 쪽에 반영되지 않는 유령 계정이 생긴다.
//
// 그래서 이메일을 **문서 ID 로 삼은 락 문서를 트랜잭션 안에서 Create** 한다. 같은 ID 로
// 두 번 Create 할 수는 없으므로 경합하는 쪽이 반드시 실패한다.
//
// ⚠ 이메일이 문서 ID 가 되므로 `/` 가 들어간 값은 쓸 수 없다. validEmail 이 그런 값을
// 애초에 막는다 — 검증을 느슨하게 풀면 여기서 잘못된 경로 문서가 생긴다.
const EmailLockCollection = "admins_by_email"

// ErrNotFound 는 그런 어드민이 없다는 뜻이다.
var ErrNotFound = errors.New("admin: 대상을 찾을 수 없다")

// ErrEmailTaken 은 그 이메일을 이미 다른 어드민이 쓰고 있다는 뜻이다.
var ErrEmailTaken = errors.New("admin: 이미 쓰이는 이메일")

// Admin 은 어드민 계정 한 건이다.
//
// ⚠ PasswordHash 가 들어 있다. **이 타입을 그대로 응답에 싣지 마라** — 응답으로 나가는
// 모양은 handler.go 의 toDTO 하나뿐이고, 그 함수는 해시를 옮기지 않는다.
type Admin struct {
	ID           string
	Email        string
	PasswordHash string
	Name         string
	IsActive     bool
	IsAdmin      bool
	Permissions  map[string]bool
	// MustChangePassword 는 "다음 로그인에서 비밀번호를 바꿔야 한다" 는 표시다.
	// 운영자가 초기 비밀번호를 알려 주는 방식이라, 그 비밀번호가 계속 살아 있으면
	// 알려 준 사람도 계정을 쓸 수 있다.
	MustChangePassword bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Subject 는 토큰에 실을 모양이다. **해시는 옮기지 않는다.**
func (a Admin) Subject() Subject {
	perms := a.Permissions
	if perms == nil {
		perms = map[string]bool{}
	}
	return Subject{
		AdminID:            a.ID,
		Email:              a.Email,
		Name:               a.Name,
		IsAdmin:            a.IsAdmin,
		Permissions:        perms,
		MustChangePassword: a.MustChangePassword,
	}
}

// adminDoc 은 Firestore 문서다.
//
// Permissions 가 map 이라 새 메뉴 키가 생겨도 스키마를 바꾸지 않는다 — 키 목록의
// 정본은 어드민 SPA 의 네비게이션 정의이고(admin.go 권한 키 주석) 서버는 그 문자열을
// 그대로 대조만 한다.
type adminDoc struct {
	Email              string          `firestore:"email"`
	PasswordHash       string          `firestore:"passwordHash"`
	Name               string          `firestore:"name"`
	IsActive           bool            `firestore:"isActive"`
	IsAdmin            bool            `firestore:"isAdmin"`
	Permissions        map[string]bool `firestore:"permissions"`
	MustChangePassword bool            `firestore:"mustChangePassword"`
	CreatedAt          time.Time       `firestore:"createdAt"`
	UpdatedAt          time.Time       `firestore:"updatedAt"`
}

// Store 는 어드민 계정 컬렉션에 대한 읽기·쓰기다.
//
// HTTP 핸들러와 `cmd/create-admin` 이 **같은 Store 를 쓴다.** 도구가 Firestore 를 직접
// 만지면 필드 이름과 이메일 정규화 규칙이 두 벌이 되고, 갈린 쪽은 "터미널에서 만들었는데
// 로그인이 안 된다" 로 나타난다.
type Store struct {
	fs *firestore.Client
}

func NewStore(fs *firestore.Client) *Store { return &Store{fs: fs} }

// 자격증명 처리는 internal/credentials가 맡는다. 미끼 해시를 두 벌로 두면
// 한쪽에서 계정 열거 방어가 사라져도 다른 쪽 테스트로 드러나지 않는다.

// FindByEmail 은 로그인용 조회다. email 은 호출부가 NormalizeEmail 을 거친 값이어야 한다.
//
// 인덱스: admins(email asc) — 단일 필드라 Firestore 가 자동으로 만든다.
func (s *Store) FindByEmail(ctx context.Context, email string) (Admin, error) {
	snaps, err := s.fs.Collection(Collection).
		Where("email", "==", email).
		Limit(1).
		Documents(ctx).GetAll()
	if err != nil {
		return Admin{}, err
	}
	if len(snaps) == 0 {
		return Admin{}, ErrNotFound
	}
	return toAdmin(snaps[0])
}

// Get 은 어드민 한 건을 읽는다.
func (s *Store) Get(ctx context.Context, id string) (Admin, error) {
	snap, err := s.fs.Collection(Collection).Doc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Admin{}, ErrNotFound
		}
		return Admin{}, err
	}
	return toAdmin(snap)
}

// List 는 모든 어드민을 만든 순서의 역순으로 읽는다.
//
// **페이지네이션이 없다.** 어드민 계정은 운영 인원 수만큼이라 수십 건을 넘길 일이 없고,
// 화면도 `{ items }` 만 있으면 된다. 언젠가 이 목록이 무거워진다면 그건 계정이
// 정리되지 않고 있다는 신호이므로 커서를 붙이기 전에 그쪽을 먼저 봐야 한다.
func (s *Store) List(ctx context.Context) ([]Admin, error) {
	snaps, err := s.fs.Collection(Collection).
		OrderBy("createdAt", firestore.Desc).
		Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]Admin, 0, len(snaps))
	for _, snap := range snaps {
		a, err := toAdmin(snap)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Create 는 어드민 계정 하나를 넣고 그 ID 를 돌려준다. 이미 있는 이메일이면 ErrEmailTaken.
//
// 계정 문서와 이메일 락 문서를 **한 트랜잭션에서** 쓴다(EmailLockCollection 주석 참고).
// 락만 남고 계정이 없거나 그 반대인 상태가 생기면 그 이메일은 영영 쓸 수 없게 된다.
func (s *Store) Create(ctx context.Context, a Admin) (string, error) {
	email := credentials.NormalizeEmail(a.Email)
	if email == "" {
		return "", errors.New("admin: 이메일이 비어 있다")
	}
	if a.Permissions == nil {
		a.Permissions = map[string]bool{}
	}

	ref := s.fs.Collection(Collection).NewDoc()
	lock := s.fs.Collection(EmailLockCollection).Doc(email)

	err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		// 락을 먼저 읽어 둔다. 트랜잭션이 이 문서를 읽었다는 사실 자체가 경합 감지의
		// 근거가 되고, 이미 있으면 Create 를 시도하기 전에 깔끔한 에러로 나간다.
		if _, err := tx.Get(lock); err == nil {
			return ErrEmailTaken
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		if err := tx.Create(lock, map[string]any{
			"adminId":   ref.ID,
			"createdAt": a.CreatedAt,
		}); err != nil {
			return err
		}
		return tx.Create(ref, map[string]any{
			"email":              email,
			"passwordHash":       a.PasswordHash,
			"name":               a.Name,
			"isActive":           a.IsActive,
			"isAdmin":            a.IsAdmin,
			"permissions":        a.Permissions,
			"mustChangePassword": a.MustChangePassword,
			"createdAt":          a.CreatedAt,
			"updatedAt":          a.UpdatedAt,
		})
	})
	if err != nil {
		// 락 문서가 트랜잭션 밖에서 이미 만들어져 있었다면 Create 가 AlreadyExists 를 낸다.
		// 호출부에는 같은 뜻이므로 하나로 눕힌다.
		if status.Code(err) == codes.AlreadyExists {
			return "", ErrEmailTaken
		}
		return "", err
	}
	return ref.ID, nil
}

// Changes 는 고칠 항목이다. nil 인 것은 건드리지 않는다 —
// 이름만 고치려다 permissions 가 빈 맵으로 덮여 권한이 통째로 날아가는 일이 없어야 한다.
//
// Email 이 없는 것은 의도적이다. 이메일은 로그인 조회 키이자 락 문서의 ID 라 바꾸려면
// 락 문서를 옮기는 트랜잭션이 따로 필요하고, 그래서 변경을 아예 막는다.
type Changes struct {
	Name               *string
	IsActive           *bool
	IsAdmin            *bool
	Permissions        *map[string]bool
	MustChangePassword *bool
	PasswordHash       *string
}

// Empty 는 고칠 것이 하나도 없는지다.
func (c Changes) Empty() bool {
	return c.Name == nil && c.IsActive == nil && c.IsAdmin == nil &&
		c.Permissions == nil && c.MustChangePassword == nil && c.PasswordHash == nil
}

// Update 는 주어진 항목만 고치고 updatedAt 을 함께 올린다.
//
// 없는 문서를 만들지 않는다(Update 는 없는 문서에 NotFound 를 낸다) — 오타 난 ID 로
// 이메일도 해시도 없는 반쪽 계정이 생기면 목록에 이름 없는 줄이 뜨고 로그인은 안 된다.
func (s *Store) Update(ctx context.Context, id string, c Changes, now time.Time) error {
	if c.Empty() {
		return errors.New("admin: 고칠 항목이 없다")
	}
	updates := []firestore.Update{{Path: "updatedAt", Value: now}}
	if c.Name != nil {
		updates = append(updates, firestore.Update{Path: "name", Value: *c.Name})
	}
	if c.IsActive != nil {
		updates = append(updates, firestore.Update{Path: "isActive", Value: *c.IsActive})
	}
	if c.IsAdmin != nil {
		updates = append(updates, firestore.Update{Path: "isAdmin", Value: *c.IsAdmin})
	}
	if c.Permissions != nil {
		updates = append(updates, firestore.Update{Path: "permissions", Value: *c.Permissions})
	}
	if c.MustChangePassword != nil {
		updates = append(updates, firestore.Update{Path: "mustChangePassword", Value: *c.MustChangePassword})
	}
	if c.PasswordHash != nil {
		updates = append(updates, firestore.Update{Path: "passwordHash", Value: *c.PasswordHash})
	}

	_, err := s.fs.Collection(Collection).Doc(id).Update(ctx, updates)
	if status.Code(err) == codes.NotFound {
		return ErrNotFound
	}
	return err
}

func toAdmin(snap *firestore.DocumentSnapshot) (Admin, error) {
	var doc adminDoc
	if err := snap.DataTo(&doc); err != nil {
		return Admin{}, err
	}
	perms := doc.Permissions
	if perms == nil {
		perms = map[string]bool{}
	}
	return Admin{
		ID:                 snap.Ref.ID,
		Email:              doc.Email,
		PasswordHash:       doc.PasswordHash,
		Name:               doc.Name,
		IsActive:           doc.IsActive,
		IsAdmin:            doc.IsAdmin,
		Permissions:        perms,
		MustChangePassword: doc.MustChangePassword,
		CreatedAt:          doc.CreatedAt,
		UpdatedAt:          doc.UpdatedAt,
	}, nil
}
