package callai

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// fake.go 는 네트워크 없이 파이프라인 전체를 돌리기 위한 공급자다.
// 테스트 파일에 두지 않는 이유는 internal/calls 테스트와 로컬 개발(CALL_ASR_PROVIDER=fake)에서도
// 그대로 쓰기 때문이다 — _test.go 안의 타입은 다른 패키지에서 import 할 수 없다.

// FakeTranscriber 는 「몇 번 폴링해야 끝나는」 비동기 공급자를 흉내 낸다.
//
// Token 은 실제 공급자처럼 stateless 다 — 남은 폴링 횟수를 토큰 문자열에 담아,
// 인스턴스가 바뀌어도 이어지는 상황을 그대로 재현한다.
type FakeTranscriber struct {
	// Polls 는 StatusDone 이 나오기까지 필요한 Poll 호출 수다. 0 이면 Start 에서 바로 끝난다.
	Polls int
	// Result 는 끝났을 때 돌려줄 전사문이다.
	Result Transcript
	// AudioSeconds 는 과금 단위 검증용 사용량이다.
	AudioSeconds float64
	// StartErr / PollErr 가 있으면 그 호출이 실패한다. FailTimes 만큼만 실패하고 그 뒤에는 정상 동작한다
	// (0 이면 계속 실패한다).
	StartErr  error
	PollErr   error
	FailTimes int

	mu         sync.Mutex
	StartCalls int
	PollCalls  int
	failed     int
	// LastAudio 는 마지막으로 받은 오디오 메타다. 오디오를 실제로 읽었는지 확인할 때 쓴다.
	LastAudio Audio
	// ReadBytes 는 Start 가 실제로 읽어 낸 오디오 바이트 수다.
	ReadBytes int64
}

func (f *FakeTranscriber) shouldFail() bool {
	if f.FailTimes == 0 {
		return true
	}
	if f.failed < f.FailTimes {
		f.failed++
		return true
	}
	return false
}

func (f *FakeTranscriber) Start(ctx context.Context, in Audio) (TranscribeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.StartCalls++
	f.LastAudio = in
	if f.StartErr != nil && f.shouldFail() {
		return TranscribeState{}, f.StartErr
	}
	// 실제 공급자는 여기서 오디오를 전부 읽어 자기 스토리지로 올린다.
	// 호출부가 Open 을 제대로 채웠는지 여기서 확인한다.
	if in.Open != nil {
		rc, err := in.Open(ctx)
		if err != nil {
			return TranscribeState{}, err
		}
		n, err := readAll(rc)
		rc.Close()
		if err != nil {
			return TranscribeState{}, err
		}
		f.ReadBytes = n
	}
	st := TranscribeState{Status: StatusRunning, Token: "fake:" + strconv.Itoa(f.Polls), Model: "fake-asr", RequestID: "fake-req"}
	if f.Polls <= 0 {
		st.Status = StatusDone
		t := f.Result
		st.Transcript = &t
		st.Usage = Usage{AudioSeconds: f.AudioSeconds}
	}
	return st, nil
}

func (f *FakeTranscriber) Poll(ctx context.Context, token string) (TranscribeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PollCalls++
	if f.PollErr != nil && f.shouldFail() {
		return TranscribeState{}, f.PollErr
	}
	left, err := strconv.Atoi(strings.TrimPrefix(token, "fake:"))
	if err != nil {
		return TranscribeState{}, &Error{Kind: KindPermanent, Code: "FakeBadToken", Message: "unknown token"}
	}
	left--
	if left > 0 {
		return TranscribeState{Status: StatusRunning, Token: "fake:" + strconv.Itoa(left), Model: "fake-asr", RequestID: "fake-req"}, nil
	}
	t := f.Result
	return TranscribeState{Status: StatusDone, Token: token, Transcript: &t, Usage: Usage{AudioSeconds: f.AudioSeconds}, Model: "fake-asr", RequestID: "fake-req"}, nil
}

// FakeAnalyzer 는 전사문을 받아 미리 정해 둔 분석을 돌려준다.
type FakeAnalyzer struct {
	Result AnalysisContent
	// Err 이 있으면 FailTimes 만큼 실패한다(0 이면 계속 실패).
	Err       error
	FailTimes int

	mu     sync.Mutex
	Calls  int
	failed int
	// LastTranscript 는 마지막으로 받은 전사문이다. 전사를 다시 하지 않고 재분석했는지 확인할 때 쓴다.
	LastTranscript Transcript
}

func (f *FakeAnalyzer) Analyze(ctx context.Context, t Transcript) (Analysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.LastTranscript = t
	if f.Err != nil {
		if f.FailTimes == 0 || f.failed < f.FailTimes {
			f.failed++
			return Analysis{}, f.Err
		}
	}
	c := f.Result
	if c.Summary == "" {
		// 호출부가 요약 없는 분석을 저장하지 못하도록, 기본값도 유효한 모양으로 만든다.
		c = AnalysisContent{Summary: "요약: " + firstRunes(t.Text, 40), Details: []Detail{}, Todos: []Todo{}, Decisions: []string{}, Consulting: emptyConsulting()}
	}
	return Analysis{Content: c, Usage: Usage{PromptTokens: 100, CompletionTokens: 50}, Model: "fake-llm", PromptVersion: "fake-1", RequestID: "fake-req"}, nil
}

func emptyConsulting() Consulting {
	return Consulting{CustomerNeeds: []string{}, Questions: []string{}, Concerns: []string{}, Objections: []string{}, ImportantPoints: []string{}, Followups: []string{}}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func readAll(r io.Reader) (int64, error) { return io.Copy(io.Discard, r) }

// FakeError 는 테스트에서 분류별 실패를 만들 때 쓴다.
func FakeError(kind Kind, code string) error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf("fake %s", kind)}
}
