package callai

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// Config 는 환경변수에서 읽은 공급자 설정이다. 여기 없는 값은 공급자 파일이 기본값을 정한다.
type Config struct {
	ASRProvider string
	ASRModel    string
	LLMProvider string
	LLMModel    string

	BaseURL     string // 스킴 없는 호스트가 올 수 있다. 공급자 파일이 https:// 를 붙인다.
	WorkspaceID string
	APIKey      string

	// ThinkingBudget 는 LLM 의 추론 토큰 상한이다.
	//
	// 🔴 공급자 기본값이 thinking ON 인 경우가 있어, 끄지 않으면 비용과 지연이 조용히 샌다.
	// 0 이하면 공급자 파일이 thinking 을 완전히 끈다.
	ThinkingBudget int
	// MaxTokens 는 답변 토큰 상한이다. thinking 토큰은 이것으로 막히지 않으므로 둘 다 건다.
	MaxTokens int

	// Timeout 은 공급자 호출 하나의 상한이다.
	//
	// 🔴 **이것은 backstop 이지 주 제어 장치가 아니다.** 호출 하나에 실제로 줄 시간은
	// internal/calls 가 남은 tick 예산에서 계산해 ctx 로 건다. 이 값이 그 창보다 **짧으면**
	// 언제나 여기가 먼저 끊기고, 그러면 우리 쪽에서 「공급자가 우리가 준 시간 안에 못
	// 끝냈다」를 일관되게 판단할 수 없게 된다 — 2026-09-18 에 25분짜리 통화가 그렇게
	// 깨졌다(그때 이 값은 30초였고, 전사문 정리에는 30초 안팎이 걸린다).
	//
	// 조건: internal/calls 의 최대 창(tickBudget - saveReserve = 45s) < Timeout < Cloud Run(60s).
	Timeout time.Duration
}

// FromEnv 는 환경변수를 읽는다.
//
// 🔴 **공급자에 기본값을 두지 않는다.** 예전에는 값이 없으면 fake 로 떨어지게 해 뒀는데,
// 그건 설정 실수 하나가 **운영에 가짜 분석을 쓰는 사고**로 이어지는 구조다 — fake 는
// "fake transcript" 를 그럴듯한 레코드로 저장하고, 화면에서는 진짜 분석과 구분되지 않는다.
// 환경변수 이름 오타나 terraform apply 전 배포처럼 흔한 실수로 벌어지고, 아무 에러도 안 난다.
// 비어 있으면 New 가 거부하고, 호출부(main.go)는 라우트를 켜지 않는다 — 분석이 아예 안 도는
// 쪽이 가짜 분석이 쌓이는 쪽보다 낫다. fake 는 **명시적으로 골랐을 때만** 쓰인다.
func FromEnv() Config {
	c := Config{
		ASRProvider:    os.Getenv("CALL_ASR_PROVIDER"),
		ASRModel:       env("CALL_ASR_MODEL", ""),
		LLMProvider:    os.Getenv("CALL_LLM_PROVIDER"),
		LLMModel:       env("CALL_LLM_MODEL", ""),
		BaseURL:        os.Getenv("CALL_AI_BASE_URL"),
		WorkspaceID:    os.Getenv("CALL_AI_WORKSPACE_ID"),
		APIKey:         os.Getenv("CALL_AI_API_KEY"),
		ThinkingBudget: envInt("CALL_LLM_THINKING_BUDGET", 1024),
		MaxTokens:      envInt("CALL_LLM_MAX_TOKENS", 4096),
		// 기본값 50초는 위 Timeout 주석의 조건에서 나왔다. 🔴 이 값을 45초 아래로 내리면
		// internal/calls 가 잡아 둔 LLM 창보다 짧아져, 25분 이상 통화가 **항상** 이 자리에서
		// 끊긴다. 올릴 때는 Cloud Run 요청 타임아웃(60초)을 넘지 않아야 한다.
		Timeout: time.Duration(envInt("CALL_AI_TIMEOUT_SECONDS", 50)) * time.Second,
	}
	return c
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// New 는 설정이 가리키는 공급자를 만든다.
//
// **공급자 추가는 여기 case 두 줄이 전부다.** 새 파일 하나를 이 디렉터리에 두고
// 생성자를 여기에 연결한다.
func New(c Config) (Transcriber, Analyzer, error) {
	var t Transcriber
	switch c.ASRProvider {
	case "":
		return nil, nil, errors.New("callai: CALL_ASR_PROVIDER 가 비어 있다")
	case "fake":
		log.Println("🔴 callai: ASR 공급자가 fake 다 — 가짜 전사문이 저장된다. 운영 설정이 아니다")
		t = &FakeTranscriber{Result: Transcript{Text: "fake transcript", Segments: []Segment{}}}
	case "alibaba":
		var err error
		if t, err = newAlibabaTranscriber(c); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("callai: 모르는 ASR 공급자 %q", c.ASRProvider)
	}

	var a Analyzer
	switch c.LLMProvider {
	case "":
		return nil, nil, errors.New("callai: CALL_LLM_PROVIDER 가 비어 있다")
	case "fake":
		log.Println("🔴 callai: LLM 공급자가 fake 다 — 가짜 분석이 저장된다. 운영 설정이 아니다")
		a = &FakeAnalyzer{}
	case "alibaba":
		var err error
		if a, err = newAlibabaAnalyzer(c); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("callai: 모르는 LLM 공급자 %q", c.LLMProvider)
	}
	return t, a, nil
}
