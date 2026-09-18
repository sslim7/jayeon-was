package calls

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/sslim7/nature-was/internal/callai"
)

// cost_test.go 는 **금액이 조용히 틀리는 경우**를 막는다.
//
// 이 숫자는 사용자가 공급자 교체를 판단하는 근거라, 여기서 확인해야 하는 것은 셋이다:
// ①설정한 단가대로 계산되는가 ②설정이 없을 때 지어내지 않는가 ③모르는 것을 0원으로
// 내보내지 않는가. ②③ 은 화면에서 눈에 띄지 않는 종류의 고장이라 테스트가 유일한 방어선이다.

// testPricing 은 **손으로 검산할 수 있는** 단가다. 실제 요금표를 쓰면 기대값이 어디서
// 왔는지 알 수 없어, 계산이 틀렸을 때 테스트를 고치게 된다.
//
//	ASR : 0.36 USD/시간 × 1000 KRW/USD = 초당 0.1원
//	입력: 1 USD/100만 토큰 × 1000      = 토큰당 0.001원
//	출력: 4 USD/100만 토큰 × 1000      = 토큰당 0.004원
func testPricing() Pricing {
	return Pricing{ASRUSDPerHour: 0.36, LLMUSDPerMillionInput: 1, LLMUSDPerMillionOutput: 4, USDToKRW: 1000, set: true}
}

// ① 단가가 있으면 설정한 단가 그대로 계산되고, 근거가 된 사용량도 함께 나간다.
//
// 오디오 1706초는 2026-09-18 실측값(28분 26초 통화)이다. 가짜 분석기의 토큰은 100/50 이다.
//
//	받아쓰기 = 1706 × 0.1        = 170.6원
//	분석     = 100×0.001 + 50×0.004 = 0.3원   ← 🔴 1원 미만이 실제로 나온다
func TestCostUsesConfiguredRates(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 1706}, &callai.FakeAnalyzer{})
	h.audioH.pricing = testPricing()
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()

	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Cost == nil {
		t.Fatal("단가를 설정했는데 비용이 없다")
	}
	if got.Cost.Currency != "KRW" {
		t.Fatalf("통화 단위=%q", got.Cost.Currency)
	}
	if got.Cost.Transcription != 170.6 {
		t.Fatalf("받아쓰기 비용=%v, 기대 170.6", got.Cost.Transcription)
	}
	// 🔴 1원 미만이 0 으로 뭉개지면 앱은 「0원」밖에 그릴 수 없다 — 공짜로 읽힌다.
	if got.Cost.Analysis != 0.3 {
		t.Fatalf("분석 비용=%v, 기대 0.3", got.Cost.Analysis)
	}
	if got.Cost.Total != 170.9 {
		t.Fatalf("합계=%v, 기대 170.9", got.Cost.Total)
	}
	// 🔴 금액만 있으면 나중에 「이 숫자가 왜 이렇지」를 검산할 수 없다.
	want := CostUsage{AudioSeconds: 1706, InputTokens: 100, OutputTokens: 50}
	if got.Cost.Usage != want {
		t.Fatalf("사용량=%+v, 기대 %+v", got.Cost.Usage, want)
	}

	// 🔴 저장 바이트에는 들어가지 않는다. 단가는 개정되는데 저장된 금액은 그대로 굳는다.
	stored, err := h.recs.Get(context.Background(), "owner-a", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Cost != nil {
		t.Fatalf("비용이 Firestore 에 저장됐다: %+v", stored.Cost)
	}
	// 목록에는 붙이지 않는다 — 금액의 근거인 사용량이 작업 문서에만 있어서, 목록에 실으면
	// 한 페이지마다 작업 문서를 그만큼 더 읽어야 한다.
	if body := h.do(t, "GET", "/calls", "owner-a", "").Body.String(); strings.Contains(body, `"cost"`) {
		t.Fatalf("목록에 비용이 실렸다: %s", body)
	}
}

// ② 단가 설정이 없으면 **아무 금액도 내보내지 않는다.** 그럴듯한 기본값으로 메우지 않는다.
func TestCostAbsentWithoutPricing(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 1706}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()

	w := h.do(t, "GET", "/calls/call-1", "owner-a", "")
	// 필드 자체가 없어야 한다. `"cost":null` 도 앱에서는 값이 온 것처럼 읽힐 여지가 있다.
	if body := w.Body.String(); strings.Contains(body, `"cost"`) {
		t.Fatalf("단가가 없는데 비용이 나갔다: %s", body)
	}
	if got := decodeRecordBody(t, w); got.Cost != nil {
		t.Fatalf("비용=%+v", got.Cost)
	}
	// 나머지는 그대로 돌아야 한다 — 단가 하나 때문에 통화분석이 꺼지지 않는다.
	if got := decodeRecordBody(t, w); got.Status != "COMPLETED" || got.Analysis == nil {
		t.Fatalf("단가가 없다고 분석까지 사라졌다: %s", w.Body.String())
	}
}

// ③ **사용량이 없는 통화는 0원이 아니라 「없음」이다.** 0원은 "돈이 안 들었다"는 주장이라,
// 그 화면을 보고 단가를 판단하면 통째로 틀린다.
func TestCostAbsentWhenUsageUnknown(t *testing.T) {
	// (가) 파이프라인 이전에 만들어졌거나 실패해 사용량이 비어 있는 작업 문서.
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 1706}, &callai.FakeAnalyzer{})
	h.audioH.pricing = testPricing()
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()
	j := h.job(t, "call-1")
	j.Usage = jobUsage{}
	if err := h.jobs.Put(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if body := h.do(t, "GET", "/calls/call-1", "owner-a", "").Body.String(); strings.Contains(body, `"cost"`) {
		t.Fatalf("사용량이 없는데 비용이 나갔다: %s", body)
	}

	// (나) 기기 업로드 경로로 저장된 통화. 작업 문서 자체가 없다.
	b, _ := json.Marshal(fixture())
	if w := h.do(t, "PUT", "/calls/call-1", "owner-b", string(b)); w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if body := h.do(t, "GET", "/calls/call-1", "owner-b", "").Body.String(); strings.Contains(body, `"cost"`) {
		t.Fatalf("기기 경로 통화에 비용이 나갔다: %s", body)
	}
}

// 🔴 응답 전용 필드는 PUT 요청에 들어오면 400 이다. cost 를 받아들이면 동결된 요청 스키마가
// 조용히 넓어지고, 그다음부터는 앱이 보낸 금액이 저장된다.
func TestDeviceUploadRejectsCost(t *testing.T) {
	b, _ := json.Marshal(fixture())
	body := strings.Replace(string(b), `"call_id"`, `"cost":{"currency":"KRW","transcription":1,"analysis":1,"total":2,"usage":{"audio_seconds":1,"input_tokens":1,"output_tokens":1,"reasoning_tokens":0}},"call_id"`, 1)
	h := newHarness(&callai.FakeTranscriber{}, &callai.FakeAnalyzer{})
	if w := h.do(t, "PUT", "/calls/call-1", "owner-a", body); w.Code != 400 {
		t.Fatalf("cost 가 들어온 기기 요청을 받아들였다: %d %s", w.Code, w.Body.String())
	}
}

// 🔴 단가는 **넷이 다 있을 때만** 쓴다. 일부만 설정하면 앱은 「받아쓰기 170원」만 그리고
// 사용자는 그것을 총액으로 읽는다 — 빠진 단가가 있다는 사실은 화면에 나타나지 않는다.
func TestPricingFromEnvNeedsEveryRate(t *testing.T) {
	full := map[string]string{
		"CALL_ASR_USD_PER_HOUR":                  "0.36",
		"CALL_LLM_USD_PER_MILLION_INPUT_TOKENS":  "1",
		"CALL_LLM_USD_PER_MILLION_OUTPUT_TOKENS": "4",
		"CALL_AI_USD_TO_KRW":                     "1000",
	}
	setAll := func(t *testing.T, m map[string]string) {
		t.Helper()
		for k, v := range m {
			t.Setenv(k, v)
		}
	}
	t.Run("전부 있으면 쓴다", func(t *testing.T) {
		setAll(t, full)
		p := PricingFromEnv()
		if !p.set || p.ASRUSDPerHour != 0.36 || p.LLMUSDPerMillionInput != 1 || p.LLMUSDPerMillionOutput != 4 || p.USDToKRW != 1000 {
			t.Fatalf("단가를 읽지 못했다: %+v", p)
		}
	})
	for key := range full {
		t.Run("빠지면 "+key, func(t *testing.T) {
			setAll(t, full)
			t.Setenv(key, "")
			if p := PricingFromEnv(); p.set {
				t.Fatalf("%s 없이 계산하려 한다: %+v", key, p)
			}
		})
	}
	for _, bad := range []string{"공짜", "-1", "NaN", "Inf"} {
		t.Run("이상한 값 "+bad, func(t *testing.T) {
			setAll(t, full)
			t.Setenv("CALL_ASR_USD_PER_HOUR", bad)
			if p := PricingFromEnv(); p.set {
				t.Fatalf("%q 를 단가로 받아들였다: %+v", bad, p)
			}
		})
	}
	// 🔴 환율 0 은 모든 금액을 0원으로 만든다 — 공짜로 보인다. 설정 없음으로 본다.
	t.Run("환율 0", func(t *testing.T) {
		setAll(t, full)
		t.Setenv("CALL_AI_USD_TO_KRW", "0")
		if p := PricingFromEnv(); p.set {
			t.Fatal("환율 0 으로 계산하려 한다")
		}
	})
	// 단가 0(무료 구간)은 설정 없음과 다르다. 값이 놓여 있으면 그대로 쓴다.
	t.Run("단가 0 은 유효하다", func(t *testing.T) {
		setAll(t, full)
		t.Setenv("CALL_ASR_USD_PER_HOUR", "0")
		p := PricingFromEnv()
		if !p.set || p.ASRUSDPerHour != 0 {
			t.Fatalf("무료 구간 단가를 버렸다: %+v", p)
		}
	})
}

// 손상된 작업 문서의 음수·NaN 사용량이 마이너스 요금이나 인코딩 불가 값으로 새지 않는다.
// 🔴 JSON 이 NaN 을 인코딩하지 못하면 응답 본문이 통째로 깨져 상세 화면이 「서버 오류」가 된다.
func TestCostIgnoresCorruptUsage(t *testing.T) {
	p := testPricing()
	if c := p.costOf(jobUsage{AudioSeconds: -100, PromptTokens: -5, CompletionTokens: -5}); c != nil {
		t.Fatalf("음수 사용량으로 금액을 만들었다: %+v", c)
	}
	c := p.costOf(jobUsage{AudioSeconds: math.NaN(), PromptTokens: 100, CompletionTokens: 50})
	if c == nil {
		t.Fatal("토큰 사용량이 있는데 비용이 없다")
	}
	if c.Transcription != 0 || c.Analysis != 0.3 {
		t.Fatalf("받아쓰기=%v 분석=%v", c.Transcription, c.Analysis)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("응답을 인코딩할 수 없다: %v", err)
	}
	// 사용량 원본은 그대로 싣되, 인코딩할 수 없는 값이 섞이면 안 된다.
	if strings.Contains(string(b), "NaN") {
		t.Fatalf("NaN 이 응답에 섞였다: %s", b)
	}
}
