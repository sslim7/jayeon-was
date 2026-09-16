package users

import (
	"context"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sslim7/jayeon-was/internal/auth"
	"github.com/sslim7/jayeon-was/internal/credentials"
)

// Collection 은 사용자 계정 문서가 사는 곳이다. cmd/create-user 도 이 Store 를 거쳐
// 쓴다 — 컬렉션 이름이 두 곳에 문자열로 박히면 한쪽 오타가 "만들었는데 로그인이
// 안 된다" 가 되고, 콘솔에서 문서를 열어 봐도 멀쩡해 보인다.
const Collection = "users"

// EmailLockCollection 은 이메일 유일성 락 문서가 사는 곳이다. 문서 ID 가 곧
// 정규화된 이메일이다.
//
// **Firestore 에는 unique 제약이 없다.** "Where(email==) 로 없는 걸 확인하고 쓴다" 는
// 조회와 쓰기 사이에 틈이 있어 같은 이메일 두 건이 동시에 들어오면 둘 다 통과한다.
// 그러면 로그인 조회(FindByEmail)가 어느 문서를 집을지 알 수 없는 상태가 되고, 한쪽
// 계정의 비밀번호 변경과 tokenVersion 증가가 다른 쪽에는 반영되지 않는 유령 계정이
// 생긴다 — 바꾼 비밀번호로 로그인되는 날과 안 되는 날이 갈린다.
//
// 그래서 이메일을 **문서 ID 로 삼은 락 문서를 계정 문서와 한 트랜잭션 안에서 Create**
// 한다. 같은 ID 로 두 번 Create 할 수는 없으므로 경합하는 쪽이 반드시 실패한다.
// internal/admin 의 admins_by_email 과 같은 장치다.
//
// ⚠ 이메일이 문서 ID 가 되므로 `/` 가 들어간 값은 쓸 수 없다. 계정을 만드는 유일한
// 경로인 cmd/create-user 가 그런 값을 애초에 막는다.
const EmailLockCollection = "users_by_email"

// ErrEmailTaken 은 그 이메일을 이미 다른 사용자가 쓰고 있다는 뜻이다.
var ErrEmailTaken = errors.New("users: 이미 쓰이는 이메일")

// User 는 사용자 계정 한 건이다.
//
// ⚠ PasswordHash 가 들어 있다. **이 타입을 그대로 응답에 싣지 마라** — 응답으로 나가는
// 모양은 users.go 의 toDTO 하나뿐이고, 그 함수는 해시도 TokenVersion 도 옮기지 않는다.
type User struct {
	UserID       string
	Email        string
	PasswordHash string
	UserName     string
	IsActive     bool
	// MustChangePassword 는 초대 시 발급한 임시 비밀번호를 아직 안 바꿨다는 뜻이다.
	MustChangePassword bool
	// TokenVersion 의 뜻과 한계는 internal/auth/account.go 의 Account.TokenVersion
	// 주석에 적혀 있다. 여기서는 그 값을 담는 자리일 뿐이다.
	TokenVersion int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// userDoc 은 Firestore 문서다. 필드 이름의 정본이 여기다.
//
// TokenVersion 은 **옛 문서에 이 필드가 없을 수 있다.** 그때 Go 의 int 제로값인 0 으로
// 읽히는 것이 의도다 — 발급되지 않은 토큰과 대조할 기준이 0 이어야 첫 SetPassword 가
// 1 로 올리면서 그 전에 나간 토큰을 전부 어긋나게 만든다. 여기에 "없으면 1" 같은
// 보정을 넣으면 그 계정만 무효화 기준이 한 칸 밀린다.
type userDoc struct {
	Email              string    `firestore:"email"`
	PasswordHash       string    `firestore:"passwordHash"`
	UserName           string    `firestore:"userName"`
	IsActive           bool      `firestore:"isActive"`
	MustChangePassword bool      `firestore:"mustChangePassword"`
	TokenVersion       int       `firestore:"tokenVersion"`
	CreatedAt          time.Time `firestore:"createdAt"`
	UpdatedAt          time.Time `firestore:"updatedAt"`
}

// Store 는 사용자 계정 컬렉션에 대한 읽기·쓰기다.
//
// HTTP 핸들러와 `cmd/create-user` 가 **같은 Store 를 쓴다.** 도구가 Firestore 를 직접
// 만지면 필드 이름과 이메일 정규화 규칙이 두 벌이 되고, 갈린 쪽은 "터미널에서 만들었는데
// 로그인이 안 된다" 로 나타난다.
type Store struct {
	fs *firestore.Client
}

// 🔴 이 한 줄이 auth 와의 계약(internal/auth/account.go)을 **컴파일 타임에** 붙잡는다.
// AccountStore 에 메서드가 하나 늘거나 시그니처가 바뀌면 여기서 먼저 깨진다 —
// 배선은 main.go 가 하므로, 이 선언이 없으면 어긋남이 런타임까지 미뤄진다.
var _ auth.AccountStore = (*Store)(nil)

func NewStore(fs *firestore.Client) *Store { return &Store{fs: fs} }

// FindByEmail 은 로그인용 조회다. 없으면 auth.ErrAccountNotFound.
//
// 호출부(auth 핸들러)가 이미 정규화한 값을 넘기지만 여기서 한 번 더 눕힌다. 멱등한
// 연산이라 잃는 것이 없고, 저장과 조회의 정규화가 어긋나는 순간 "콘솔에는 문서가
// 멀쩡히 있는데 로그인만 안 되는" 상태가 되기 때문이다.
//
// 인덱스: users(email asc) — 단일 필드라 Firestore 가 자동으로 만든다.
func (s *Store) FindByEmail(ctx context.Context, email string) (auth.Account, error) {
	normalized := credentials.NormalizeEmail(email)
	if normalized == "" {
		return auth.Account{}, auth.ErrAccountNotFound
	}

	snaps, err := s.fs.Collection(Collection).
		Where("email", "==", normalized).
		Limit(1).
		Documents(ctx).GetAll()
	if err != nil {
		return auth.Account{}, err
	}
	if len(snaps) == 0 {
		return auth.Account{}, auth.ErrAccountNotFound
	}
	u, err := toUser(snaps[0])
	if err != nil {
		return auth.Account{}, err
	}
	return u.account(), nil
}

// Get 은 userId 로 계정을 읽는다. 없으면 auth.ErrAccountNotFound.
// 리프레시가 TokenVersion 과 IsActive 를 확인하는 데 쓴다.
func (s *Store) Get(ctx context.Context, userID string) (auth.Account, error) {
	u, err := s.GetProfile(ctx, userID)
	if err != nil {
		return auth.Account{}, err
	}
	return u.account(), nil
}

// SetPassword 는 비밀번호 해시를 바꾸고 tokenVersion 을 하나 올리며
// mustChangePassword 를 거짓으로 만든다. 없는 문서면 auth.ErrAccountNotFound.
//
// 🔴 **네 필드가 한 번의 Update 로 나간다.** 나눠 쓰면 해시만 바뀌고 버전이 안 오른
// 상태가 생길 수 있는데(두 번째 쓰기가 실패하거나, 그 사이에 프로세스가 죽거나), 그러면
// 비밀번호를 바꿨는데도 옛 리프레시 토큰이 TTL(90일) 동안 그대로 돌고 **아무 에러도 남지
// 않는다.** 겉으로는 비밀번호 변경이 성공한 화면만 보이고, 털린 세션이 살아 있다는 사실은
// 어디에도 드러나지 않는다. 편의를 위해서라도 쪼개지 마라.
//
// firestore.Increment 는 서버 쪽 변환이라 문서를 먼저 읽을 필요가 없다. 읽고-더하고-쓰면
// 같은 시각의 두 변경이 서로를 덮어써 버전이 한 번만 오르는 경우가 생긴다.
//
// 버전을 올리는 것만으로 옛 세션이 끊기는 것은 아니다. 실제로 끊는 자리는 리프레시가
// 이 값을 대조하는 한 줄이다(internal/auth/handler.go 의 refresh).
func (s *Store) SetPassword(ctx context.Context, userID, passwordHash string, now time.Time) error {
	_, err := s.fs.Collection(Collection).Doc(userID).Update(ctx, []firestore.Update{
		{Path: "passwordHash", Value: passwordHash},
		{Path: "tokenVersion", Value: firestore.Increment(1)},
		{Path: "mustChangePassword", Value: false},
		{Path: "updatedAt", Value: now},
	})
	if status.Code(err) == codes.NotFound {
		return auth.ErrAccountNotFound
	}
	return err
}

// Create 는 사용자 계정 하나를 넣고 그 ID 를 돌려준다. 이미 있는 이메일이면 ErrEmailTaken.
//
// 계정 문서와 이메일 락 문서를 **한 트랜잭션에서** 쓴다(EmailLockCollection 주석 참고).
// 락만 남고 계정이 없거나 그 반대인 상태가 생기면 그 이메일은 영영 쓸 수 없게 된다.
//
// 가입 API 가 없는 초대 전용 서비스라 이 함수를 부르는 곳은 cmd/create-user 하나뿐이다.
func (s *Store) Create(ctx context.Context, u User) (string, error) {
	email := credentials.NormalizeEmail(u.Email)
	if email == "" || strings.Contains(email, "/") {
		return "", errors.New("users: 이메일이 비어 있다")
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
			"userId":    ref.ID,
			"createdAt": u.CreatedAt,
		}); err != nil {
			return err
		}
		return tx.Create(ref, map[string]any{
			"email":              email,
			"passwordHash":       u.PasswordHash,
			"userName":           u.UserName,
			"isActive":           u.IsActive,
			"mustChangePassword": u.MustChangePassword,
			"tokenVersion":       u.TokenVersion,
			"createdAt":          u.CreatedAt,
			"updatedAt":          u.UpdatedAt,
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

// GetProfile 은 GET /users/me 가 쓰는 계정 읽기다. 없으면 auth.ErrAccountNotFound.
//
// "없음" 에 auth 의 센티넬을 그대로 쓴다. 이 패키지가 자기 ErrNotFound 를 따로 두면
// 같은 뜻의 에러가 두 개가 되고, 호출부가 한쪽만 errors.Is 로 받으면 없는 계정이
// 401 이 아니라 500 으로 나간다.
func (s *Store) GetProfile(ctx context.Context, userID string) (User, error) {
	if userID == "" {
		return User{}, auth.ErrAccountNotFound
	}
	snap, err := s.fs.Collection(Collection).Doc(userID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return User{}, auth.ErrAccountNotFound
		}
		return User{}, err
	}
	return toUser(snap)
}

// account 는 auth 가 필요로 하는 만큼만 옮긴 모양이다.
//
// toDTO 와 정반대의 함수다. 이쪽은 인증에 쓰라고 PasswordHash 와 TokenVersion 을 **일부러**
// 실어 보내므로, 여기서 나온 값이 응답 쪽으로 흘러가지 않게 두 경로를 섞지 마라.
func (u User) account() auth.Account {
	return auth.Account{
		UserID:             u.UserID,
		Email:              u.Email,
		PasswordHash:       u.PasswordHash,
		UserName:           u.UserName,
		IsActive:           u.IsActive,
		MustChangePassword: u.MustChangePassword,
		TokenVersion:       u.TokenVersion,
	}
}

func toUser(snap *firestore.DocumentSnapshot) (User, error) {
	var doc userDoc
	if err := snap.DataTo(&doc); err != nil {
		return User{}, err
	}
	return User{
		UserID:             snap.Ref.ID,
		Email:              doc.Email,
		PasswordHash:       doc.PasswordHash,
		UserName:           doc.UserName,
		IsActive:           doc.IsActive,
		MustChangePassword: doc.MustChangePassword,
		TokenVersion:       doc.TokenVersion,
		CreatedAt:          doc.CreatedAt,
		UpdatedAt:          doc.UpdatedAt,
	}, nil
}
