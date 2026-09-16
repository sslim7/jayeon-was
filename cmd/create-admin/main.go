// create-admin 은 **첫 어드민 계정**을 Firestore 에 넣는 커맨드다.
//
// 어드민 계정은 화면으로 만들 수 있지만, 그 화면에 들어가려면 이미 어드민이어야 한다.
// 닭이 먼저냐 달걀이 먼저냐를 끊는 자리가 여기다. 그래서 이 도구는 **한 번 쓰고 잊는
// 것이 정상**이다 — 두 번째부터는 어드민 화면의 계정 관리에서 만든다.
//
// 하는 일:
//
//	admins/{자동ID}          ← email · passwordHash(bcrypt) · name ·
//	                            isActive:true · isAdmin:true · permissions:{} ·
//	                            mustChangePassword:true
//	admins_by_email/{email}  ← 이메일 유일성 락 (internal/admin/store.go)
//
// 두 문서를 한 트랜잭션에 쓰는 것도, 이메일을 소문자로 눕히는 것도 전부
// internal/admin.Store 가 한다. **이 도구가 Firestore 를 직접 만지지 않는 이유**가
// 그것이다 — 필드 이름이나 정규화 규칙이 두 벌이 되면 "터미널에서 만들었는데 로그인이
// 안 된다" 가 되고, 그때 콘솔에서 문서를 열어 봐도 멀쩡해 보인다.
//
// # isAdmin: true · mustChangePassword: true
//
// 첫 계정은 전권이어야 다른 계정을 만들 수 있다. 그리고 **초기 비밀번호는 반드시 한 번
// 바꾸게 한다** — 이 명령을 친 사람의 셸 히스토리와 터미널 스크롤에 평문이 남아 있기
// 때문이다. 로그인 직후 SPA 가 비밀번호 변경 화면으로 보낸다.
//
// # 안전장치
//
//   - **dry-run 이 기본이다.** 실제로 쓰려면 --write, 운영에 쓰려면 --yes 까지 필요하다.
//     dry-run 은 입력 검증과 "무엇을 할지" 출력까지만 한다 — Firestore 를 읽지도 쓰지도 않는다.
//   - 이미 있는 이메일이면 만들지 않고 이유를 말하고 멈춘다. 조용히 덮어쓰면 그 계정의
//     비밀번호가 바뀌어 쓰던 사람이 로그인하지 못하게 된다.
//   - 에뮬레이터 설정과 운영 프로젝트가 섞이면 고르지 말고 멈춘다.
//
// # .env 를 읽지 않는다
//
// .env 에는 FIRESTORE_EMULATOR_HOST 와 demo-* 프로젝트가 들어 있고, godotenv 는
// "비어 있는 환경변수만" 채우므로 셸에서 지워도 오히려 .env 값이 들어온다. 운영 작업인
// 줄 알고 에뮬레이터에 계정을 만든 뒤 "로그인이 안 된다" 로 헤매기 딱 좋다.
// 환경변수는 실행할 때 직접 준다.
//
// ADMIN_JWT_SECRET 은 필요 없다. 이 도구는 토큰을 발급하지 않고 계정만 넣는다.
//
// 실행:
//
//	# 로컬(에뮬레이터) — 무엇을 할지 먼저 본다
//	GOOGLE_CLOUD_PROJECT=demo-jayeon FIRESTORE_EMULATOR_HOST=127.0.0.1:8080 \
//	  GOOGLE_APPLICATION_CREDENTIALS= \
//	  go run ./cmd/create-admin -email admin@nature.kr -name 홍길동 -password '초기비밀번호8자이상'
//
//	# 로컬에 실제로 쓴다
//	GOOGLE_CLOUD_PROJECT=demo-jayeon FIRESTORE_EMULATOR_HOST=127.0.0.1:8080 \
//	  GOOGLE_APPLICATION_CREDENTIALS= \
//	  go run ./cmd/create-admin -email admin@nature.kr -name 홍길동 -password '초기비밀번호8자이상' --write
//
//	# 운영에 실제로 쓴다
//	FIRESTORE_EMULATOR_HOST= GOOGLE_APPLICATION_CREDENTIALS= \
//	  GOOGLE_CLOUD_PROJECT=<운영 프로젝트> \
//	  go run ./cmd/create-admin -email … -name … -password … --write --yes
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore"

	"github.com/sslim7/nature-was/internal/admin"
	"github.com/sslim7/nature-was/internal/credentials"
)

func main() {
	// dry-run 이 기본이라 "실행" 쪽에 플래그를 요구한다.
	email := flag.String("email", "", "어드민 로그인 이메일. 소문자로 눕혀 저장한다")
	name := flag.String("name", "", "화면과 활동 로그에 보일 이름")
	password := flag.String("password", "", "초기 비밀번호("+strconv.Itoa(admin.MinPasswordRunes)+"자 이상). 첫 로그인에서 바꾸게 된다")
	write := flag.Bool("write", false, "실제로 쓴다. 없으면 dry-run(Firestore 를 읽지도 쓰지도 않는다)")
	yes := flag.Bool("yes", false, "운영 데이터에 실제로 쓴다는 확인. 운영 쓰기 실행에만 필요하다")
	flag.Parse()

	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		log.Fatal("GOOGLE_CLOUD_PROJECT 환경변수가 필요하다 (이 커맨드는 .env 를 읽지 않는다 — 파일 상단 주석 참고)")
	}
	fsEmulator := os.Getenv("FIRESTORE_EMULATOR_HOST")

	// 모순되는 설정은 고르지 말고 멈춘다. 한쪽을 기본값으로 택하면 운영 프로젝트에
	// 붙었다고 믿으면서 에뮬레이터에 계정을 만드는 사고가 조용히 일어난다.
	if fsEmulator != "" && !strings.HasPrefix(projectID, "demo-") {
		log.Fatalf("설정이 모순이다: GOOGLE_CLOUD_PROJECT=%s (운영 프로젝트)인데 FIRESTORE_EMULATOR_HOST=%s 가 설정돼 있다.\n"+
			"  운영에 붙으려면 FIRESTORE_EMULATOR_HOST= 로 비우고, 에뮬레이터에 붙으려면 GOOGLE_CLOUD_PROJECT=demo-* 를 쓴다.",
			projectID, fsEmulator)
	}

	// 입력 검증은 Firestore 에 붙기 전에 끝낸다. 연결부터 하고 나서 "이름이 비었다" 로
	// 죽으면 자격증명이 없는 자리에서는 무엇이 틀렸는지조차 알 수 없다.
	normalized := credentials.NormalizeEmail(*email)
	if normalized == "" {
		log.Fatal("-email 이 필요하다")
	}
	if !strings.Contains(normalized, "@") || strings.ContainsAny(normalized, " /") {
		// 이 값이 admins_by_email 의 문서 ID 가 된다(internal/admin/store.go).
		log.Fatalf("-email 형식이 올바르지 않다: %q", *email)
	}
	adminName := strings.TrimSpace(*name)
	if adminName == "" {
		log.Fatal("-name 이 필요하다 (활동 로그에 이 이름이 남는다)")
	}
	if utf8.RuneCountInString(*password) < admin.MinPasswordRunes {
		log.Fatalf("-password 는 %d자 이상이어야 한다", admin.MinPasswordRunes)
	}

	target := "운영 (실제 데이터)"
	if fsEmulator != "" {
		target = "에뮬레이터 " + fsEmulator + " (운영 데이터가 아니다)"
	}
	mode := "dry-run (아무것도 쓰지 않는다)"
	if *write {
		mode = "쓰기"
	}
	log.Print("──────── create-admin ────────")
	log.Printf("  프로젝트 : %s", projectID)
	log.Printf("  대상     : %s", target)
	log.Printf("  모드     : %s", mode)
	log.Printf("  이메일   : %s", normalized)
	log.Printf("  이름     : %s", adminName)
	// 비밀번호는 찍지 않는다. 터미널 스크롤과 CI 로그에 그대로 남는다.
	log.Printf("  비밀번호 : (%d자, 표시하지 않는다)", utf8.RuneCountInString(*password))
	log.Print("  권한     : isAdmin=true · permissions={} · mustChangePassword=true")
	log.Print("──────────────────────────────")

	if fsEmulator == "" && *write && !*yes {
		log.Printf("운영 프로젝트 %s 에 어드민 계정을 만들려 한다.", projectID)
		log.Print("무엇을 할지 먼저 보려면 --write 없이(dry-run), 그대로 진행하려면 --write --yes 를 붙여 다시 실행한다.")
		os.Exit(1)
	}
	if !*write {
		log.Print("dry-run 이라 여기서 멈춘다. 실제로 만들려면 --write 를 붙인다.")
		return
	}

	ctx := context.Background()
	fs, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("Firestore 클라이언트 생성 실패: %v", err)
	}
	defer fs.Close()

	store := admin.NewStore(fs)

	// 트랜잭션이 경합을 막아 주지만, 미리 확인해야 사람에게 쓸 만한 문구를 줄 수 있다.
	// (트랜잭션만 믿으면 "AlreadyExists" 같은 말이 그대로 나간다.)
	if existing, err := store.FindByEmail(ctx, normalized); err == nil {
		log.Printf("이미 그 이메일의 어드민이 있다: adminId=%s name=%s isAdmin=%v isActive=%v",
			existing.ID, existing.Name, existing.IsAdmin, existing.IsActive)
		log.Fatal("만들지 않았다. 비밀번호를 잊었다면 다른 isAdmin 계정으로 PATCH /admin/admins/{adminId} 를 쓴다")
	} else if !errors.Is(err, admin.ErrNotFound) {
		log.Fatalf("기존 계정 확인 실패: %v", err)
	}

	hash, err := credentials.HashPassword(*password)
	if err != nil {
		log.Fatalf("비밀번호 해시 실패: %v", err)
	}

	now := time.Now()
	id, err := store.Create(ctx, admin.Admin{
		Email:        normalized,
		PasswordHash: hash,
		Name:         adminName,
		IsActive:     true,
		// 첫 계정은 전권이다. 이 계정으로 나머지를 만든다.
		IsAdmin: true,
		// isAdmin 이 전 메뉴를 열므로 개별 권한을 줄 필요가 없다.
		Permissions: map[string]bool{},
		// 이 명령의 평문 비밀번호가 셸 히스토리에 남아 있다. 반드시 바꾸게 한다.
		MustChangePassword: true,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		if errors.Is(err, admin.ErrEmailTaken) {
			log.Fatalf("방금 사이에 같은 이메일이 만들어졌다. 만들지 않았다: %s", normalized)
		}
		log.Fatalf("어드민 계정 생성 실패: %v", err)
	}

	log.Printf("어드민 계정을 만들었다: adminId=%s email=%s", id, normalized)
	log.Print("첫 로그인에서 비밀번호를 바꿔야 한다(mustChangePassword=true).")
}
