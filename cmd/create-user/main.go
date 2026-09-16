// create-user는 초대 계정을 만든다. 기본 dry-run은 Firestore에 연결하지 않는다.
// .env의 에뮬레이터 설정이 운영 대상과 섞이지 않도록 환경변수만 읽는다.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore"

	"github.com/sslim7/jayeon-was/internal/auth"
	"github.com/sslim7/jayeon-was/internal/credentials"
	"github.com/sslim7/jayeon-was/internal/users"
)

func main() {
	// dry-run 이 기본이라 "실행" 쪽에 플래그를 요구한다.
	email := flag.String("email", "", "사용자 로그인 이메일. 소문자로 눕혀 저장한다")
	name := flag.String("name", "", "프로필에 보일 이름")
	password := flag.String("password", "", "초기 비밀번호(8자 이상, 72바이트 이하). 첫 로그인에서 바꾸게 된다")
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
		// 이 값이 users_by_email 의 문서 ID 가 된다(internal/users/store.go).
		log.Fatalf("-email 형식이 올바르지 않다: %q", *email)
	}
	userName := strings.TrimSpace(*name)
	if userName == "" {
		log.Fatal("-name 이 필요하다 (프로필에 이 이름이 보인다)")
	}
	if utf8.RuneCountInString(*password) < auth.MinPasswordRunes {
		log.Fatalf("-password 는 %d자 이상이어야 한다", auth.MinPasswordRunes)
	}

	if len(*password) > 72 {
		log.Fatal("-password 는 72바이트 이하여야 한다")
	}
	target := "운영 (실제 데이터)"
	if fsEmulator != "" {
		target = "에뮬레이터 " + fsEmulator + " (운영 데이터가 아니다)"
	}
	mode := "dry-run (아무것도 쓰지 않는다)"
	if *write {
		mode = "쓰기"
	}
	log.Print("──────── create-user ────────")
	log.Printf("  프로젝트 : %s", projectID)
	log.Printf("  대상     : %s", target)
	log.Printf("  모드     : %s", mode)
	log.Printf("  이메일   : %s", normalized)
	log.Printf("  이름     : %s", userName)
	// 비밀번호는 찍지 않는다. 터미널 스크롤과 CI 로그에 그대로 남는다.
	log.Printf("  비밀번호 : (%d자, 표시하지 않는다)", utf8.RuneCountInString(*password))
	log.Print("  계정     : isActive=true · mustChangePassword=true · tokenVersion=0")
	log.Print("──────────────────────────────")

	if fsEmulator == "" && *write && !*yes {
		log.Printf("운영 프로젝트 %s 에 사용자 계정을 만들려 한다.", projectID)
		log.Print("무엇을 할지 먼저 보려면 --write 없이(dry-run), 그대로 진행하려면 --write --yes 를 붙여 다시 실행한다.")
		os.Exit(1)
	}
	if !*write {
		log.Print("dry-run 이라 여기서 멈춘다. 실제로 만들려면 --write 를 붙인다.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fs, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("Firestore 클라이언트 생성 실패: %v", err)
	}
	defer fs.Close()

	store := users.NewStore(fs)

	hash, err := credentials.HashPassword(*password)
	if err != nil {
		log.Fatalf("비밀번호 해시 실패: %v", err)
	}

	now := time.Now()
	id, err := store.Create(ctx, users.User{
		Email:        normalized,
		PasswordHash: hash,
		UserName:     userName,
		IsActive:     true,
		// 이 명령의 평문 비밀번호가 셸 히스토리에 남아 있다. 반드시 바꾸게 한다.
		MustChangePassword: true,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		if errors.Is(err, users.ErrEmailTaken) {
			log.Fatalf("같은 이메일의 계정이 이미 있다. 만들지 않았다: %s", normalized)
		}
		log.Fatalf("사용자 계정 생성 실패: %v", err)
	}

	log.Printf("사용자 계정을 만들었다: userId=%s email=%s", id, normalized)
	log.Print("첫 로그인에서 비밀번호를 바꿔야 한다(mustChangePassword=true).")
}
