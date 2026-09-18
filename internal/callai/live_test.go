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
	"strconv"
	"strings"
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

// ── 분석 지연 실측 ─────────────────────────────────────────────────────────────
//
// TestLiveAnalyzeLatency 는 **「전사문 하나를 정리하는 데 몇 초가 걸리는가」** 를 잰다.
// internal/calls 의 예산 상수(llmNeed 등)는 이 숫자 위에 얹혀 있고, 숫자를 모르는 채로
// 상수를 정하면 2026-09-18 사고가 그대로 되풀이된다 — 예산이 모자란 줄 모르고 공급자를
// 부르고, 30초에 끊긴 뒤 통화를 확정 실패로 버렸다.
//
// 🔴 **실제 상담 내용을 쓰지 않는다.** 길이만 흉내 낸 합성 전사문이다. 우리가 재려는 것은
// 「이만한 분량이 들어가면 몇 초가 걸리는가」뿐이고, 진짜 통화 내용은 이 목적에 필요 없다.
//
//	CALL_AI_LIVE=1 CALL_AI_BASE_URL=... CALL_AI_API_KEY=... \
//	CALL_AI_LIVE_MINUTES=25 CALL_AI_LIVE_RUNS=3 CALL_AI_LIVE_THINKING=1024 \
//	go test ./internal/callai/ -run LiveAnalyzeLatency -v -timeout 30m
func TestLiveAnalyzeLatency(t *testing.T) {
	cfg := liveConfig(t)
	// 실측이 우리 타임아웃에 먼저 걸리면 재는 의미가 없다. 여유를 크게 준다.
	cfg.Timeout = 10 * time.Minute
	if v := os.Getenv("CALL_AI_LIVE_THINKING"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("CALL_AI_LIVE_THINKING 을 숫자로 읽을 수 없다: %v", err)
		}
		cfg.ThinkingBudget = n
	}
	minutes := 25
	if v := os.Getenv("CALL_AI_LIVE_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("CALL_AI_LIVE_MINUTES 가 잘못됐다: %q", v)
		}
		minutes = n
	}
	runs := 1
	if v := os.Getenv("CALL_AI_LIVE_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("CALL_AI_LIVE_RUNS 가 잘못됐다: %q", v)
		}
		runs = n
	}

	an, err := newAlibabaAnalyzer(cfg)
	if err != nil {
		t.Fatalf("analyzer 생성 실패: %v", err)
	}
	tr := syntheticTranscript(minutes)
	// 공급자에 실제로 들어가는 바이트 수다. 상수를 정할 때 「몇 분」보다 이 값이 기준이 된다.
	prompt := buildUserMessage(tr, maxPromptBytes)
	t.Logf("입력: %d분 세그먼트=%d 본문바이트=%d 프롬프트바이트=%d thinking_budget=%d",
		minutes, len(tr.Segments), len(tr.Text), len(prompt), cfg.ThinkingBudget)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var worst time.Duration
	for i := 1; i <= runs; i++ {
		start := time.Now()
		res, err := an.Analyze(ctx, tr)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%d회차 Analyze 실패(%.1fs): kind=%v code=%s request_id=%s err=%v",
				i, elapsed.Seconds(), KindOf(err), CodeOf(err), RequestIDOf(err), err)
		}
		if elapsed > worst {
			worst = elapsed
		}
		// 🔴 내용이 아니라 시간·토큰·개수만 남긴다.
		t.Logf("%d회차: 소요=%.1fs prompt_tokens=%d completion_tokens=%d reasoning_tokens=%d details=%d todos=%d model=%s",
			i, elapsed.Seconds(), res.Usage.PromptTokens, res.Usage.CompletionTokens, res.Usage.ReasoningTokens,
			len(res.Content.Details), len(res.Content.Todos), res.Model)
		if res.Content.Summary == "" {
			t.Errorf("%d회차: 요약이 비었다", i)
		}
	}
	t.Logf("최악 소요=%.1fs (%d회 중) — internal/calls 의 llmCallNeed 는 이 값에 여유를 더해 잡는다", worst.Seconds(), runs)
}

// syntheticTranscript 는 분 단위 길이에 맞춘 **합성** 상담 전사문을 만든다.
// 문장은 어느 통화에서도 나올 법한 일반 업무 문장이고, 실제 상담 내용이 아니다.
func syntheticTranscript(minutes int) Transcript {
	lines := []string{
		"안녕하세요, 어제 문의 주신 건으로 연락드렸습니다.",
		"네, 기다리고 있었습니다. 지금 통화 괜찮습니다.",
		"보내주신 자료는 확인했고 몇 가지만 더 여쭤보겠습니다.",
		"수량이 늘어나면 단가가 어떻게 달라지는지 궁금합니다.",
		"기본 단가는 동일하고 백 개 이상부터 할인이 들어갑니다.",
		"납기는 주문 확정일 기준으로 이 주 정도 잡고 있습니다.",
		"그 일정이면 저희 내부 일정과도 맞을 것 같습니다.",
		"결제 조건은 세금계산서 발행 후 말일 정산으로 진행합니다.",
		"계약서 초안은 이번 주 안으로 전달드리겠습니다.",
		"담당자분 성함과 연락처를 한 번 더 확인해 주시겠어요.",
		"설치 교육은 현장에서 두 시간 정도 진행됩니다.",
		"기존에 쓰시던 장비와 연동되는지도 확인이 필요합니다.",
		"그 부분은 기술팀에 확인해서 내일 회신드리겠습니다.",
		"예산 범위를 조금 넘어서 내부 결재가 필요할 것 같습니다.",
		"그러면 견적을 두 가지 안으로 나눠 드리겠습니다.",
		"유지보수는 첫 해 무상이고 이후에는 연 단위 계약입니다.",
		"장애가 생기면 연락은 어디로 드리면 되나요.",
		"대표번호로 주시면 담당자가 바로 배정됩니다.",
		"그럼 다음 주 화요일 오전에 다시 통화하시죠.",
		"네, 정리해서 메일로도 보내드리겠습니다.",
	}
	// 상담 통화의 발화는 대체로 3~5초다. 4초로 잡으면 25분에 375토막이 된다.
	const secPerLine = 4.0
	total := int(float64(minutes) * 60 / secPerLine)
	segs := make([]Segment, 0, total)
	var text strings.Builder
	for i := 0; i < total; i++ {
		line := lines[i%len(lines)]
		speaker := "1"
		if i%2 == 1 {
			speaker = "2"
		}
		start := float64(i) * secPerLine
		segs = append(segs, Segment{Start: start, End: start + secPerLine, Text: line, Speaker: speaker})
		text.WriteString(line)
		text.WriteByte(' ')
	}
	return Transcript{Text: strings.TrimSpace(text.String()), Segments: segs, Language: "ko"}
}
