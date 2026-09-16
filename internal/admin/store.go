package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"golang.org/x/crypto/bcrypt"
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

// dummyPasswordHash 는 **없는 계정에 대해서도 bcrypt 비교를 한 번 돌리기 위한** 미끼다.
//
// 계정이 없을 때 즉시 401 을 돌려주면 응답이 수십 밀리초 빨라진다. bcrypt 는 일부러
// 느리게 설계된 함수라 그 차이가 밖에서 또렷하게 보이고, 그러면 로그인 API 하나로
// "이 이메일이 어드민으로 등록돼 있는가" 를 훑어낼 수 있다. 어드민 이메일 목록은
// 표적 피싱의 출발점이므로 그 정보를 시간으로 흘리지 않는다.
//
// 아무 비밀번호와도 맞지 않는 임의 값의 해시다(cost 는 bcrypt.DefaultCost = 10 으로
// 실제 계정과 같아야 소요 시간이 같다).
const dummyPasswordHash = "$2a$10$KcFvHV9FpL6YiD8PfrNs9esrwER0RrgDqXbg6fe5lx2ILe28BuNsq"

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

// NormalizeEmail 은 저장·조회에 쓰는 이메일 표준형이다.
//
// 소문자로 눕히고 앞뒤 공백을 턴다. 저장할 때와 로그인할 때 **반드시 같은 함수를**
// 거쳐야 한다 — 대문자로 만든 계정에 소문자로 로그인하면 "등록된 어드민 계정이
// 아닙니다" 가 나오는데, 콘솔에서 문서를 보면 멀쩡히 있어서 원인을 찾기 어렵다.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// HashPassword 는 비밀번호를 bcrypt 해시로 바꾼다.
//
// bcrypt 는 **72바이트를 넘는 입력을 조용히 자르지 않고 에러를 낸다**(x/crypto v0.53).
// 그래서 호출부가 길이를 미리 막지 않아도 여기서 걸린다.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword 는 해시와 평문이 맞는지 본다.
//
// hash 가 비어 있으면 dummyPasswordHash 로 비교한다 — 계정이 없는 경우에도 호출부가
// 같은 코드를 지나가게 해서 응답 시간으로 가입 여부가 새지 않게 한다.
func VerifyPassword(hash, plain string) bool {
	if hash == "" {
		hash = dummyPasswordHash
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

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
	email := NormalizeEmail(a.Email)
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
