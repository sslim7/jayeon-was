package bg

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

// captureLog 는 표준 로거의 출력을 가로챈다. 테스트가 끝나면 원래대로 돌려놓는다.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr) // 표준 로거의 기본 출력
		log.SetFlags(flags)
	})
	return &buf
}

// TestRecoverStopsPanic 은 이 패키지의 존재 이유를 못박는다.
// panic 이 고루틴 밖으로 새면 프로세스가 죽고, 그 순간 처리 중이던 다른 요청이 전부 끊긴다.
func TestRecoverStopsPanic(t *testing.T) {
	buf := captureLog(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer Recover("테스트 작업")
		panic("터졌다")
	}()
	// 여기까지 돌아왔다는 것 자체가 panic 이 잡혔다는 뜻이다.
	// 잡히지 않았다면 테스트 프로세스가 이 줄에 닿기 전에 죽는다.
	wg.Wait()

	out := buf.String()
	if !strings.Contains(out, "[ERROR]") {
		t.Errorf("[ERROR] 수준으로 남아야 한다: %q", out)
	}
	if !strings.Contains(out, "테스트 작업") {
		t.Errorf("어느 작업이었는지 label 이 남아야 한다: %q", out)
	}
	if !strings.Contains(out, "터졌다") {
		t.Errorf("panic 값이 남아야 한다: %q", out)
	}
	// 스택이 없으면 원인을 영영 못 찾는다. panic 을 낸 이 파일이 들어 있어야 한다.
	if !strings.Contains(out, "bg_test.go") {
		t.Errorf("스택 트레이스가 남아야 한다: %q", out)
	}
	if !strings.Contains(out, "goroutine") {
		t.Errorf("스택 트레이스 형식이 아니다: %q", out)
	}
}

// TestRecoverPassesThroughNormalPath 는 panic 이 없을 때 아무 일도 하지 않는지 본다.
// 정상 경로에 로그가 끼면 진짜 봐야 할 로그 사이에서 노이즈가 된다.
func TestRecoverPassesThroughNormalPath(t *testing.T) {
	buf := captureLog(t)

	done := false
	func() {
		defer Recover("테스트 작업")
		done = true
	}()

	if !done {
		t.Fatal("정상 경로가 끝까지 돌지 않았다")
	}
	if buf.Len() != 0 {
		t.Errorf("panic 이 없으면 아무것도 남기지 않아야 한다: %q", buf.String())
	}
}

// TestRecoverHandlesErrorValue 는 문자열이 아닌 panic 값도 다루는지 본다.
// 런타임 panic(nil map 쓰기, 인덱스 초과)은 error 값으로 올라온다.
func TestRecoverHandlesErrorValue(t *testing.T) {
	buf := captureLog(t)

	func() {
		defer Recover("테스트 작업")
		panic(errors.New("경계 밖 접근"))
	}()

	if !strings.Contains(buf.String(), "경계 밖 접근") {
		t.Errorf("error 값 panic 이 남지 않았다: %q", buf.String())
	}
}

// TestRecoverLogsOnlyLabelAndPanic 은 label 외에 호출부 변수가 새지 않는지 본다.
// 알림 발송 고루틴의 클로저에는 전화번호와 문구가 잡혀 있다.
func TestRecoverLogsOnlyLabelAndPanic(t *testing.T) {
	buf := captureLog(t)

	const phoneNo = "01012345678"
	content := "[자연] 홍길동님, 요청하신 안내입니다"

	func() {
		defer Recover("notify: 안내 문자 발송")
		// 클로저가 번호와 문구를 붙들고 있는 상태에서 터뜨린다.
		if phoneNo != "" && content != "" {
			panic("발송 중 오류")
		}
	}()

	out := buf.String()
	for _, secret := range []string{phoneNo, content, "홍길동"} {
		if strings.Contains(out, secret) {
			t.Errorf("로그에 민감정보가 노출됐다 (%q): %q", secret, out)
		}
	}
}
