package calls

import (
	"log"
	"math"
	"os"
	"strconv"
)

// cost.go 는 통화 한 건에 **실제로 얼마가 들었는가**를 원화로 계산한다.
//
// # 왜 서버가 계산하는가
//
// 앱은 단가를 알 필요가 없다. 단가가 개정되거나 공급자가 바뀌어도 **앱 배포 없이** 숫자가
// 맞아야 하고, 그러려면 계산이 서버에 있어야 한다. 앱에 단가를 내려보내고 앱이 곱하게
// 하면 구버전 앱이 옛 단가로 계산한 금액을 계속 보여 주는데, 그 화면은 틀렸다는 표시가
// 어디에도 없다.
//
// # 🔴 단가를 코드에 상수로 박지 않는다
//
// 이 프로젝트는 공급자 교체(Vertex AI·Bedrock 등)를 전제로 설계돼 있고(§internal/callai),
// 공급자 단가는 우리 배포 주기와 무관하게 개정된다. 단가가 소스에 있으면 요금표가 바뀔
// 때마다 빌드→배포가 필요하고, 그 사이 화면의 금액은 **조용히 틀린 채** 떠 있다.
//
// # 🔴 설정이 모자라면 계산하지 않는다 — 기본값을 지어내지 않는다
//
// 이 숫자는 사용자가 **공급자 교체를 판단하는 근거**다. 그럴듯한 기본 단가를 넣어 두면
// 설정 실수(오타, terraform apply 전 배포)가 「그럴듯하지만 틀린 금액」으로 나타나는데,
// 화면만 봐서는 진짜와 구분되지 않는다. 비용 칸이 아예 없는 쪽이 낫다 — 없는 것은 눈에
// 띄지만 틀린 것은 눈에 띄지 않는다.

// Pricing 은 사용량을 원화로 바꾸는 단가다. **전부 환경변수에서 온다.**
//
// 🔴 네 값은 **전부 있거나 전부 없거나** 둘 중 하나로만 취급한다(set 참고). 일부만 있을 때
// 그 부분만 계산해 내보내면, 앱은 「받아쓰기 96원」만 그리고 사용자는 그것을 한 건의 총액으로
// 읽는다 — 분석 단가가 빠졌다는 사실은 화면 어디에도 나타나지 않는다.
type Pricing struct {
	// ASRUSDPerHour 는 오디오 **1시간**당 ASR 단가(USD)다. 공급자 요금표가 대개 시간당으로
	// 적혀 있어 그대로 옮겨 적을 수 있는 단위를 골랐다(초당으로 바꿔 적다가 0 을 하나 빠뜨리면
	// 금액이 10배로 틀리는데, 그것을 알아챌 방법이 화면에 없다).
	ASRUSDPerHour float64
	// LLMUSDPerMillionInput / LLMUSDPerMillionOutput 은 **100만 토큰당** LLM 단가(USD)다.
	//
	// ⚠️ 출력 토큰은 `usage.completion_tokens` 그대로다. 추론(thinking) 토큰은 그 값에 이미
	// **포함**되어 있고 `reasoning_tokens` 는 그 안의 내역일 뿐이다(§internal/callai/alibaba_llm.go).
	// 둘을 더하면 추론 토큰이 두 번 청구된 것처럼 보인다.
	LLMUSDPerMillionInput  float64
	LLMUSDPerMillionOutput float64
	// USDToKRW 는 USD→KRW 환율이다. 단가는 공급자 요금표 그대로 USD 로 적고 환산은 여기서
	// 한 번만 한다 — 단가마다 원화로 미리 곱해 두면 환율이 바뀔 때 고칠 자리가 세 군데가 된다.
	USDToKRW float64
	// set 은 네 값이 **전부 설정에서 왔는지**다.
	//
	// 🔴 `> 0` 로 판단하지 않는다. 0 은 「무료 구간」이라는 실제 단가일 수 있어서, 값이
	// 없는 것과 0 인 것을 값만 보고 구분할 수 없다. 환경변수가 실제로 놓여 있었는지를
	// 파싱 시점에 기억해 둔다.
	set bool
}

// CostUsage 는 금액의 **근거가 된 사용량 원본**이다.
//
// 🔴 금액만 내보내면 나중에 「이 숫자가 왜 이렇지」를 확인할 길이 없다. 단가는 배포 환경에
// 있고 사용량은 작업 문서에 있어서, 응답에 둘 중 하나라도 빠지면 화면의 금액을 손으로
// 검산할 수 없다. 사용량은 크기가 작으니 함께 싣는다.
type CostUsage struct {
	// AudioSeconds 는 ASR 이 청구한 오디오 길이다. 통화 길이와 다를 수 있다 —
	// 재시도한 전사의 초까지 **누적**된 값이기 때문이다(§pipeline.go 의 Usage 누적 주석).
	AudioSeconds float64 `json:"audio_seconds"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	// ReasoningTokens 는 OutputTokens 안에 **포함된** 추론 토큰이다. 0 이 아니면 thinking 이
	// 켜져 있다는 뜻이라 요금이 왜 높은지의 단서가 된다. 금액 계산에는 따로 더하지 않는다.
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Cost 는 통화 한 건의 원가다. **응답 전용**이며 Firestore 에 저장하지 않는다.
//
// 🔴 저장하면 단가 개정 전에 계산된 금액이 굳은 채 남는다 — 그 레코드만 옛 단가로 계산된
// 금액을 영원히 보여 주는데, 화면에는 그 사실이 나타나지 않는다. audio_url 과 같은 이유로
// 조회할 때마다 다시 계산한다(§model.go 의 listRecord 가 저장 경로에서 이 값을 비운다).
type Cost struct {
	// Currency 는 언제나 "KRW" 다. 앱이 통화 기호를 가정하지 않게 명시해 둔다.
	Currency string `json:"currency"`
	// Transcription / Analysis 를 **나눠서** 내보낸다. 이 파이프라인은 비용의 대부분이
	// 받아쓰기라, 합계만 주면 그 사실이 가려져 사용자가 어느 단계를 바꿔야 하는지 알 수 없다.
	Transcription float64   `json:"transcription"`
	Analysis      float64   `json:"analysis"`
	Total         float64   `json:"total"`
	Usage         CostUsage `json:"usage"`
}

// PricingFromEnv 는 단가 설정을 읽는다.
//
// 🔴 **여기서 기동을 막지 않는다.** 단가 하나 때문에 통화분석 전체가 꺼질 이유가 없다 —
// 모자라면 응답에 비용만 빠지고 나머지는 그대로 돈다(internal/admin, CALL_AUDIO_BUCKET 관례와 같다).
func PricingFromEnv() Pricing {
	asr, okASR := envMoney("CALL_ASR_USD_PER_HOUR")
	in, okIn := envMoney("CALL_LLM_USD_PER_MILLION_INPUT_TOKENS")
	out, okOut := envMoney("CALL_LLM_USD_PER_MILLION_OUTPUT_TOKENS")
	// 환율만은 0 이 말이 되지 않는다. 0 이면 모든 금액이 0원이 되어 **공짜로 보인다.**
	fx, okFX := envMoney("CALL_AI_USD_TO_KRW")
	p := Pricing{ASRUSDPerHour: asr, LLMUSDPerMillionInput: in, LLMUSDPerMillionOutput: out, USDToKRW: fx}
	p.set = okASR && okIn && okOut && okFX && fx > 0
	if !p.set {
		log.Println("calls: 단가 설정(CALL_ASR_USD_PER_HOUR / CALL_LLM_USD_PER_MILLION_INPUT_TOKENS / CALL_LLM_USD_PER_MILLION_OUTPUT_TOKENS / CALL_AI_USD_TO_KRW)이 모자라다 — 응답에 비용을 넣지 않는다")
	}
	return p
}

// envMoney 는 단가 하나를 읽는다. **설정되지 않은 것과 0 을 구분해서** 돌려준다.
//
// 음수·NaN·Inf 는 설정 실수다. 그대로 쓰면 마이너스 요금이나 JSON 인코딩 실패로 이어지므로
// 「없음」으로 취급한다 — 그러면 네 값 규칙에 걸려 비용 칸 전체가 빠진다.
func envMoney(key string) (float64, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		log.Printf("calls: %s 값을 읽을 수 없다(%q) — 비용을 계산하지 않는다", key, raw)
		return 0, false
	}
	return v, true
}

// costOf 는 누적 사용량을 원화 금액으로 바꾼다. **모르면 nil 이다.**
//
// ⚠️ nil 이 되는 경우가 둘이고 **둘 다 정상**이다:
//
//	① 단가 설정이 없다 → 계산할 수 없다.
//	② 사용량이 하나도 없다 → 파이프라인 이전에 만들어진 통화이거나, 공급자를 한 번도 부르지
//	   못하고 실패한 통화다.
//
// 🔴 ②를 0원으로 내보내면 안 된다. **0원과 「모름」은 다르다** — 0원은 "돈이 안 들었다"는
// 주장이고, 그 화면을 보고 공급자 단가를 판단하면 통째로 틀린다.
func (p Pricing) costOf(u jobUsage) *Cost {
	if !p.set {
		return nil
	}
	seconds, in, out := atLeast0(u.AudioSeconds), atLeast0n(u.PromptTokens), atLeast0n(u.CompletionTokens)
	if seconds == 0 && in == 0 && out == 0 {
		return nil
	}
	asr := seconds / 3600 * p.ASRUSDPerHour * p.USDToKRW
	llm := (in*p.LLMUSDPerMillionInput + out*p.LLMUSDPerMillionOutput) / 1e6 * p.USDToKRW
	// 🔴 유한하지 않은 값은 encoding/json 이 인코딩하지 못한다 — 그러면 응답 본문이 통째로
	// 깨져 상세 화면이 「서버 오류」가 된다. 손상된 작업 문서 하나가 그 통화의 화면 전체를
	// 막지 않도록, 계산이 이상하면 비용만 뺀다.
	if !finite(asr) || !finite(llm) {
		return nil
	}
	asr, llm = won(asr), won(llm)
	return &Cost{
		Currency: "KRW", Transcription: asr, Analysis: llm, Total: won(asr + llm),
		// 🔴 사용량도 **계산에 실제로 들어간 값**을 싣는다. 작업 문서의 원본을 그대로 넣으면
		// 손상된 NaN 하나가 encoding/json 을 실패시켜 상세 응답 전체가 깨지고, 무엇보다
		// 화면의 금액과 근거가 어긋난다 — 검산하려고 싣는 값이 검산을 방해하게 된다.
		Usage: CostUsage{
			AudioSeconds: seconds, InputTokens: int(in),
			OutputTokens: int(out), ReasoningTokens: max(u.ReasoningTokens, 0),
		},
	}
}

// won 은 금액을 0.0001원 단위로 자른다.
//
// 🔴 **정수 원으로 반올림하지 않는다.** 짧은 통화 한 건은 1원이 안 될 수 있고, 그것을 여기서
// 0 으로 만들면 앱은 「0원」밖에 그릴 수 없어 **공짜로 읽힌다.** 표기 반올림은 화면이 한다
// (§jayeon-app `lib/call-cost.ts` — 1원 미만은 소수점을 남기고, 그보다 작으면 「0.01원 미만」).
//
// 자르는 자리를 0.01원이 아니라 0.0001원에 둔 이유도 같다. 서버가 0.01원 단위로 미리 뭉개면
// 그보다 싼 공급자로 갈아탔을 때 **모든 금액이 0 으로 붙어** 비교 자체가 불가능해진다 —
// 화면은 그것을 「공짜」로 그린다. 자르는 목적은 정밀도 축소가 아니라 부동소수
// 잔여물(96.40000000000001)을 응답에 싣지 않는 것뿐이다.
func won(v float64) float64 { return math.Round(v*10000) / 10000 }

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// atLeast0 / atLeast0n 은 손상된 작업 문서의 음수·NaN 사용량을 0 으로 본다. 마이너스 요금은
// 없고, 음수를 그대로 곱하면 합계가 줄어 **다른 단계의 비용까지 가려진다.**
func atLeast0(v float64) float64 {
	if !finite(v) || v < 0 {
		return 0
	}
	return v
}

func atLeast0n(v int) float64 {
	if v < 0 {
		return 0
	}
	return float64(v)
}
