package callai

// live_test.go 는 **진짜 Alibaba 로 나가는** 테스트다. 기본적으로 건너뛴다.
//
//	CALL_AI_LIVE=1 \
//	CALL_AI_BASE_URL=ws-xxxx.ap-southeast-1.maas.aliyuncs.com \
//	CALL_AI_API_KEY=sk-xxxx \
//	CALL_AI_LIVE_AUDIO=/path/to/short.m4a \
//	go test ./internal/callai/ -run Live -v
//
// 🔴 .env 를 읽지 않는다. 환경변수를 직접 받는다 — 이 테스트는 돈이 나가고 실제 쿼터를
// 쓰기 때문에, 파일이 옆에 있다는 이유만으로 우연히 켜지면 안 된다.
// 🔴 전사 내용과 분석 내용은 출력하지 않는다. 길이와 사용량만 남긴다.

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func liveConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("CALL_AI_LIVE") == "" {
		t.Skip("실호출 테스트: CALL_AI_LIVE=1 로 켠다")
	}
	base, key := os.Getenv("CALL_AI_BASE_URL"), os.Getenv("CALL_AI_API_KEY")
	if base == "" || key == "" {
		t.Skip("CALL_AI_BASE_URL / CALL_AI_API_KEY 가 필요하다")
	}
	return Config{
		ASRProvider: "alibaba", LLMProvider: "alibaba",
		ASRModel: os.Getenv("CALL_ASR_MODEL"), LLMModel: os.Getenv("CALL_LLM_MODEL"),
		BaseURL: base, APIKey: key,
		ThinkingBudget: 0, // 실호출에서 thinking 이 정말 꺼지는지 reasoning_tokens 로 확인한다.
		MaxTokens:      4096,
		Timeout:        60 * time.Second,
	}
}

func TestLiveTranscribeAndAnalyze(t *testing.T) {
	cfg := liveConfig(t)
	audioPath := os.Getenv("CALL_AI_LIVE_AUDIO")
	if audioPath == "" {
		t.Skip("CALL_AI_LIVE_AUDIO 에 짧은 오디오 파일 경로가 필요하다")
	}
	fi, err := os.Stat(audioPath)
	if err != nil {
		t.Fatalf("오디오 파일을 읽을 수 없다: %v", err)
	}

	tr, err := newAlibabaTranscriber(cfg)
	if err != nil {
		t.Fatalf("transcriber 생성 실패: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	in := Audio{
		Open:         func(context.Context) (io.ReadCloser, error) { return os.Open(audioPath) },
		Size:         fi.Size(),
		FileName:     fi.Name(),
		Language:     "ko",
		SpeakerCount: 2,
	}
	st, err := tr.Start(ctx, in)
	if err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	t.Logf("제출 완료: model=%s request_id=%s", st.Model, st.RequestID)

	deadline := time.Now().Add(9 * time.Minute)
	for st.Status != StatusDone {
		if time.Now().After(deadline) {
			t.Fatal("전사가 제한 시간 안에 끝나지 않았다")
		}
		time.Sleep(5 * time.Second)
		st, err = tr.Poll(ctx, st.Token)
		if err != nil {
			t.Fatalf("Poll 실패: kind=%v code=%s request_id=%s err=%v", KindOf(err), CodeOf(err), RequestIDOf(err), err)
		}
	}
	if st.Transcript == nil {
		t.Fatal("완료인데 Transcript 가 nil 이다")
	}
	// 🔴 내용이 아니라 크기만 남긴다.
	t.Logf("전사 완료: 글자수=%d 세그먼트=%d 오디오초=%.1f request_id=%s",
		len([]rune(st.Transcript.Text)), len(st.Transcript.Segments), st.Usage.AudioSeconds, st.RequestID)
	if st.Transcript.Segments == nil {
		t.Error("Segments 가 nil 이다")
	}
	var last float64
	for i, s := range st.Transcript.Segments {
		if s.Start < last || s.End < s.Start {
			t.Fatalf("세그먼트[%d] 정렬/구간이 깨졌다: start=%.2f end=%.2f (직전 start=%.2f)", i, s.Start, s.End, last)
		}
		last = s.Start
	}

	an, err := newAlibabaAnalyzer(cfg)
	if err != nil {
		t.Fatalf("analyzer 생성 실패: %v", err)
	}
	res, err := an.Analyze(ctx, *st.Transcript)
	if err != nil {
		t.Fatalf("Analyze 실패: kind=%v code=%s request_id=%s err=%v", KindOf(err), CodeOf(err), RequestIDOf(err), err)
	}
	t.Logf("분석 완료: model=%s prompt=%s request_id=%s prompt_tokens=%d completion_tokens=%d reasoning_tokens=%d",
		res.Model, res.PromptVersion, res.RequestID,
		res.Usage.PromptTokens, res.Usage.CompletionTokens, res.Usage.ReasoningTokens)
	if res.Usage.ReasoningTokens != 0 {
		// 🔴 200 이 떴다고 파라미터가 먹힌 게 아니다. 이 값이 0 이 아니면 thinking 이 살아 있다.
		t.Errorf("thinking 을 껐는데 reasoning_tokens=%d 다 — 파라미터가 무시됐을 수 있다", res.Usage.ReasoningTokens)
	}
	if res.Content.Summary == "" {
		t.Error("요약이 비었다")
	}
	t.Logf("분석 항목 수: details=%d todos=%d decisions=%d",
		len(res.Content.Details), len(res.Content.Todos), len(res.Content.Decisions))
	if res.Content.Decisions == nil || res.Content.Consulting.Questions == nil {
		t.Error("nil 슬라이스가 정규화되지 않았다")
	}
}
