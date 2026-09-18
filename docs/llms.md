# AI 공급자 단가 비교 (2026-09-18 조사)

통화분석은 외부 AI 공급자를 두 번 부른다 — 받아쓰기(ASR)와 정리·분석(LLM)이다.
이 문서는 「지금 쓰는 곳이 적정한가, 옮길 곳이 있는가」를 실제 공시 단가로 따져 본 기록이다.

**결론부터: 옮기지 않는다. 받아쓰기·분석 **둘 다** Alibaba 가 조사한 모든 동급 선택지 중 최저가다.**
이동 방향이 절감이 아니라 **증가**라서, 비용을 이유로 옮길 근거가 없다.

> ⚠️ 이 문서는 **조사 시점(2026-09-18)의 스냅샷**이다. 단가는 자주 바뀌고 프로모션은 끝난다
> (아래 Gemini 3.8 Flash 가 그 예다). 6개월 뒤에 이 표를 근거로 결정하지 말 것 — 그때는
> 다시 조사해야 한다. 조사 방법과 무엇을 확인 못 했는지를 §5, §6 에 남겨 두었다.

---

## 0. 전제

| 항목 | 값 |
|---|---|
| 기준 통화 | 28분 26초 상담 녹음 1건 (오디오 1,706초, 모노, 한국어) |
| LLM 사용량 | 입력 약 17,800 토큰 / 출력 약 2,200 토큰(추론 1,024 포함) |
| 환율 | 1 USD = **1,385 KRW** (2026-09-18, Bloomberg·Investing·XE 중간값) |
| 예상 사용량 | 월 60~300건 (하루 2~10건) |

🔴 **LLM 호출은 통화당 1회다.** 원문 전체를 한 번 읽히고 요약·할 일·상담 분석을
구조화 출력으로 한꺼번에 받는다. 세 번 나눠 부르면 같은 원문을 세 번 읽히므로 **입력
비용만 3배**가 된다 — 「요약만 먼저 빨리 보여 주자」 같은 요구가 들어오면 이 값을 먼저 계산할 것.

### 실측된 1건 비용

| 단계 | 모델 | 금액 | 비중 |
|---|---|---|---|
| 받아쓰기 | `qwen-audio-3.0-asr-flash-filetrans` | **82.7원** | **84.9%** |
| 정리·분석 | `qwen3.7-plus` | **14.7원** | 15.1% |
| 합계 | | **97.4원** | |

🔴 **비용의 85%가 받아쓰기다.** LLM 쪽을 아무리 줄여도 15원짜리 안에서 노는 것이고,
추론(thinking)을 꺼도 건당 3원이다. 절감 이야기가 나오면 **어느 쪽 이야기인지부터 가를 것.**

---

## 1. 받아쓰기(ASR) 비교 — 28분 통화 1건

🔴 **화자 분리(diarization)는 타협 불가 조건이다.** 상담사와 고객의 말을 가르지 못하면
「누가 무엇을 물었고 무엇을 답했나」가 무너져 분석 품질이 통째로 떨어진다. 그래서 지원
여부로 표를 갈랐다 — 아래쪽 표는 **후보가 아니라 참고**다.

### 화자 분리 지원 (실제 후보)

| 공급자 / 모델 | 단가 | 1건 | 화자분리 추가요금 | 한국어 |
|---|---|---|---|---|
| **Alibaba `qwen-audio-3.0-asr-flash-filetrans` (현행)** | $0.000035/초 | **82.7원** | 없음(모델 기본) | ✅ 공식 지원 |
| Speechmatics Melia 1 (batch) | $0.129/시 | 84.6원 | ⚠️ 무료로 보이나 명문 확인 불가 | ✅ |
| AssemblyAI Universal-2 + diar | $0.15/시 + $0.02/시 | 111.6원 | **별도 과금** | ✅ |
| Azure AI Speech (batch) | $0.18/시 | 118.1원 | 없음 | ✅ ko-KR |
| Google Chirp 3 + diar (Dynamic Batch 가정) | $0.003/분 | 118.1원 | 없음 | ✅ ko-KR |
| Gladia Growth (async) | $0.20/시~ | 131.3원 | 없음 | ✅ |
| ElevenLabs Scribe v2 | $0.22/시 | 144.5원 | 없음(최대 32명) | ⚠️ "Good"(WER 10~20%) |
| Deepgram Nova-3 Mono | $0.0043/분 | 169.4원 | 없음 | ✅ ko-KR |
| OpenAI `gpt-4o-transcribe-diarize` | 토큰 과금(≈$0.006/분) | 236.3원 | 모델 단가에 내재 | ❌ 한국어 명시 확인 불가 |
| Google Chirp 3 + diar (Standard 단가) | $0.016/분 | 630.0원 | 없음 | ✅ |

**싼 곳이 하나도 없다.** 가장 근접한 Speechmatics 가 2% 비싸고 나머지는 1.3~7.6배다.

### 화자 분리 미지원 (후보 아님, 참고용)

| 공급자 / 모델 | 1건 | 왜 못 쓰는가 |
|---|---|---|
| Groq `whisper-large-v3-turbo` | **26.3원** | API 에 diarization 파라미터 자체가 없다 |
| Groq `whisper-large-v3` | 72.9원 | 〃 |
| Google Chirp 2 | 630.0원 | 공식 문서 "Diarization: Not supported" |
| OpenAI `gpt-transcribe` / `whisper-1` | 177 / 236원 | `gpt-4o-transcribe-diarize` 만 화자 식별 지원 |

⚠️ **Groq 가 68% 싸다는 사실에 매번 눈이 간다.** 실제로 검토했고 결론은 「아니오」다:
화자 분리를 직접 붙여야 하고(pyannote 등), 한국어 화자분리 품질이 미검증이며, 파이프라인이
하나 더 늘어난다. 월 300건 기준 절감액이 **약 16,900원**이라 그 부담을 질 금액이 아니다.

### 월간 (ASR만)

| 공급자 | 월 60건 | 월 150건 | 월 300건 |
|---|---|---|---|
| **Alibaba (현행)** | **4,962원** | **12,405원** | **24,810원** |
| Speechmatics Melia 1 | 5,077원 | 12,694원 | 25,387원 |
| AssemblyAI + diar | 6,698원 | 16,745원 | 33,489원 |
| Azure batch | 7,088원 | 17,721원 | 35,442원 |
| Deepgram Nova-3 | 10,163원 | 25,408원 | 50,816원 |
| *(참고) Groq turbo — 화자분리 없음* | *1,579원* | *3,947원* | *7,894원* |

### 긴 오디오 처리와 무료 한도

상담 통화는 30분~1시간이라 **동기 호출 제한에 걸리는 곳이 많다.** 옮길 때 반드시 볼 항목이다.

| 공급자 | 방식 | 길이/용량 한도 | 무료 한도 |
|---|---|---|---|
| Alibaba filetrans | 비동기(파일 전송) | **12시간 / 2GB** | ASR 10시간 |
| Speechmatics | batch | 확인 불가 | $100 크레딧 + Melia 월 10시간 |
| AssemblyAI | 비동기(동기는 2분 제한) | 5GB / 10시간 | $50 크레딧 |
| Azure batch | 비동기 job | diarization 시 **240분 상한**, 모노 필수 | 없음 |
| Google Chirp 3 | **BatchRecognize 전용**(동기 1분) | 8시간 | 없음 |
| Deepgram | 10분 초과 시 504 → **callback 필수** | 2GB | $200 크레딧 |
| OpenAI | **동기만, Batch API 미지원** | **25MB** | 없음 |
| ElevenLabs | webhook 비동기 | 3GB / 10시간 | 플랜별 |
| Groq | 동기 | 25MB / 100MB | — |

---

## 2. 정리·분석(LLM) 비교 — 28분 통화 1건

### 「동급」을 어떻게 잡았나

`qwen3.7-plus` 는 Alibaba 라인업에서 최상위(`qwen3.7-max`, $2.5/$7.5) 바로 아래의
**범용 주력**이다. 각 사에서 같은 자리를 차지하는 모델을 1순위로 잡고, 위·아래 한 단계를
함께 적었다 — 「조금 더 쓰면 얼마나 좋아지나 / 한 단계 내리면 얼마나 아끼나」를 보기 위해서다.

| 회사 | 최상위 | **주력(동급)** | 경량 |
|---|---|---|---|
| Alibaba | qwen3.7-max | **qwen3.7-plus** | — |
| Google | Gemini 3.1 Pro | **Gemini 3.8 Flash** | Gemini 3.5 Flash-Lite |
| OpenAI | gpt-6-astra / gpt-5.6-sol | **gpt-5.6-terra** | gpt-5.6-luna |
| Anthropic | Claude Fable 5.1 / Opus 5 | **Claude Sonnet 5** | Claude Haiku 4.5 |

근거: Anthropic 은 공식 문서가 "Haiku for simple tasks, **Sonnet for most production
workloads**, Opus for the most complex reasoning" 으로 티어를 직접 규정한다. OpenAI 는
Sol=프런티어 / Terra=프로덕션 워크호스 / Luna=경량의 3단 구성. Google 은 Pro 라인이 3.1 에서
동결된 반면 Flash 라인이 3.6→3.7→3.8 로 갱신되며 실질 주력 자리를 차지했다.

> 🔴 **이 표가 이 문서에서 가장 약한 고리다.** 「동급」을 **역할**(각 사에서 트래픽 대부분을
> 받는 주력 모델)로 잡았지 **성능**으로 잡은 것이 아니다. 등급 이름을 붙이는 기준이 회사마다
> 달라서 Alibaba 의 max/plus 와 Google 의 Pro/Flash/Flash-Lite 는 깔끔하게 대응되지 않는다.
>
> 실제로 **「Google 의 동급은 Flash 가 아니라 Pro 아니냐」는 반론이 타당하다.** Flash 는
> 속도·비용에 최적화된 아래 등급이고 `qwen3.7-plus` 는 `qwen3.7-max` 바로 아래이기 때문이다.
> 그래서 아래 표에 **Pro 도 함께** 실었다 — 어느 쪽으로 맞추든 결론이 같기 때문이다:
>
> | 맞추는 기준 | Google 대응 | 1건 | vs 현행 |
> |---|---|---|---|
> | 역할(주력) | Gemini 3.8 Flash | 29.9원 | 2.0x |
> | **등급 이름** | **Gemini 3.1 Pro** | **85.9원** | **5.8x** |
>
> 등급 이름으로 맞추면 격차가 **더 벌어진다.** 그래서 이 표의 느슨함이 §4 의 결론을 흔들지는
> 않는다 — 다만 「구글은 2배밖에 안 비싸다」처럼 **한 줄로 기억하면 틀린다.**
>
> ⚠️ **비용은 이 문서가 답했지만 품질은 답하지 못했다.** 남은 질문은 「5배를 더 내면 5배 나은
> 요약이 나오는가」인데 단가표로는 답할 수 없다. 답하려면 **같은 통화 원문 하나를 네 곳에
> 넣어 결과를 나란히 놓고 보는 수밖에 없다.** 아직 하지 않았다.

### 1건당

| 공급자 | 모델 | 입력 $/1M | 출력 $/1M | 1건 | vs 현행 |
|---|---|---|---|---|---|
| **Alibaba** | **qwen3.7-plus (현행)** | 0.40 | 1.60 | **14.7원** | **1.0x** |
| Google | Gemini 3.5 Flash-Lite (아래) | 0.30 | 2.50 | 15.0원 | 1.02x |
| Google | **Gemini 3.8 Flash (동급)** | 0.75 | 3.75 | **29.9원** | 2.0x |
| Google | Gemini 3.8 Flash (**2027-01-01~**) | 1.50 | 7.50 | 59.8원 | 4.1x |
| Anthropic | Claude Haiku 4.5 (아래) | 1.00 | 5.00 | 39.9원 | 2.7x |
| Anthropic | **Claude Sonnet 5 (동급)** | 2.00 | 10.00 | **79.8원** | 5.4x |
| OpenAI | **gpt-5.6-terra (동급)** | 2.00 | 12.00 | **85.9원** | 5.8x |
| Google | Gemini 3.1 Pro (위) | 2.00 | 12.00 | 85.9원 | 5.8x |
| OpenAI | gpt-5.6-sol (위) | 4.00 | 20.00 | 159.6원 | 10.8x |
| Anthropic | Claude Opus 5 (위) | 5.00 | 25.00 | 199.4원 | 13.5x |
| *(참고)* | *gpt-5.6-luna (경량)* | *0.20* | *1.20* | *8.6원* | *0.58x* |

⚠️ **Gemini 3.8 Flash 의 2.0x 는 프로모션 가격이다.** 2026-12-31 종료가 공시돼 있고
그 뒤 4.1x 가 된다. 「구글이 2배밖에 안 비싸다」로 기억해 두면 내년에 틀린다.

### 월간 (LLM만)

| 모델 | 월 60건 | 월 150건 | 월 300건 |
|---|---|---|---|
| **qwen3.7-plus (현행)** | **884원** | **2,210원** | **4,421원** |
| Gemini 3.5 Flash-Lite | 901원 | 2,252원 | 4,504원 |
| Gemini 3.8 Flash | 1,795원 | 4,487원 | 8,975원 |
| Claude Haiku 4.5 | 2,393원 | 5,983원 | 11,966원 |
| Claude Sonnet 5 | 4,787원 | 11,966원 | 23,933원 |
| gpt-5.6-terra | 5,152원 | 12,881원 | 25,761원 |

### 기능 비교 — 옮길 때 실제로 걸리는 것들

| 항목 | qwen3.7-plus (현행) | Gemini 3.8 Flash | gpt-5.6-terra | Claude Sonnet 5 |
|---|---|---|---|---|
| 추론 기본 상태 | ON | ON (medium) | ON (medium) | ON (adaptive) |
| 추론 완전 OFF | 가능 | **불가**(최저 `low`) | 가능(`effort: none`) | 가능(`thinking: disabled`) |
| 추론 토큰 과금 | 출력 단가 | 출력 단가 | 출력 단가 | 출력 단가 |
| 구조화 출력 | 지원(사용 중) | 지원, 깊은 중첩 거부 가능 | 지원(strict) | 지원(constrained decoding) |
| 구조화 출력 제약 | — | 일부 키워드 미지원 | `additionalProperties:false` 필수 | `minimum`/`maximum`/`minLength`/재귀 **미지원** |
| 캐싱 최소 토큰 | 지원 | **4,096** | 1,024 | 1,024 |
| 장문 할증 | 256K 초과 시 입력 $1.2 | 없음 | 272K 초과 2x/1.5x | 없음(1M 균일) |
| 배치 할인 | — | 50% / 24시간 | 50% / 24시간 | 50% |

🔴 우리는 **구조화 출력(JSON 스키마)에 의존한다**(`internal/callai/prompts/call_analysis_v1.schema.json`).
Claude 로 옮긴다면 스키마에서 `minimum`/`maximum`/`minLength` 를 걷어내야 하고, Gemini 는
깊은 중첩을 거부할 수 있다. **단가만 보고 옮기면 여기서 걸린다.**

---

## 3. 절감 수단 — 검토했고 대부분 효과가 없다

| 수단 | 판정 | 이유 |
|---|---|---|
| **추론(thinking) 끄기** | ⚠️ **해볼 값어치 있음 — 단 비용이 아니라 속도 때문** | 출력 2,178 → 876 토큰. 건당 2.9원(월 300건 880원)뿐이지만 **지연이 33.6초 → 13.5초로 60% 줄어든다**(실측). 분석 품질 유지 여부는 실제 샘플 A/B 필요 |
| 프롬프트 캐싱 | ❌ | 통화마다 원문이 달라 캐시 대상은 시스템 프롬프트뿐. 절감 상한이 건당 1~2원인데, 통화 간격이 TTL(5~30분)보다 길면 캐시 쓰기 할증(1.25x)만 물어 **순손해** |
| 배치 API 50% 할인 | ❌ | 24시간 지연. 통화 직후 결과를 보여 주는 현재 UX 와 양립 불가 |
| Alibaba Savings Plan | ❌ | 음성 모델 3개월 선불 17% 할인이지만 **최소 약정 USD 150**. 월 300건이어도 ASR 지출이 $17.9/월이라 8개월치 선불이 필요하고 할인액은 월 4,200원 |
| Groq 전환 + 자체 화자분리 | ❌ | §1 참고. 월 300건 16,900원 절감 대비 구축·운영 부담과 한국어 품질 미검증 |
| 더 싼 LLM 티어로 하향 | ❌ | 현행보다 싼 것은 경량 티어뿐이고 동급이 아니다. 요약 품질이 이 제품의 존재 이유다 |

---

## 4. 최종 판단

| 시나리오 (ASR+LLM 합계) | 월 60건 | 월 150건 | 월 300건 |
|---|---|---|---|
| **현행 유지** | **5,846원** | **14,615원** | **29,230원** |
| 최선의 대안(Speechmatics + Gemini 3.8 Flash) | 6,872원 | 17,181원 | 34,362원 |
| 차액 | +1,026원 | +2,566원 | **+5,132원** |

하루 10건을 써도 **월 3만 원**이다. 여기서 몇 천 원을 움직이자고 공급자 마이그레이션
(API 재작성, 응답 포맷 변경, 화자분리 출력 구조 차이 흡수, 한국어 품질 재검증, 장애 대응
재구축)을 하는 것은 수지가 맞지 않는다. **게다가 방향이 절감이 아니라 증가다.**

**공급자를 섞는 것도 의미 없다.** ASR·LLM 모두 Alibaba 가 최저가라 섞으면 어느 조합이든
비용이 오르고 인증·에러 처리·모니터링만 두 벌이 된다.

### 다시 볼 조건

- 사용량이 **월 1,000건**을 넘으면 → Speechmatics 대량 할인(500시간 초과분 20%)과
  Alibaba Savings Plan 을 재계산
- **2026-12-31** 이후 → Gemini 프로모션 종료로 격차가 4.1배로 벌어진다(더 볼 것 없음)
- 화자 분리 요구가 사라지면 → Groq 가 68% 싸다. 다만 그 요구가 사라질 이유가 없다

---

## 5. 조사 방법

공개 가격 페이지를 직접 확인했다(전부 2026-09-18).

- **Alibaba**: alibabacloud.com/help/en/model-studio — model-pricing, asr-model, savings-plan / openrouter.ai
- **Google**: cloud.google.com/speech-to-text/pricing, v2/docs/chirp_3-model, chirp_2-model / ai.google.dev/gemini-api/docs — pricing, thinking, caching, structured-output, batch-api
- **OpenAI**: developers.openai.com/api/docs — pricing, models/gpt-4o-transcribe-diarize, models/gpt-5.6-terra, guides/speech-to-text, guides/batch, guides/prompt-caching, guides/structured-outputs
- **Anthropic**: claude.com/pricing / platform.claude.com/docs/en — about-claude/pricing, build-with-claude/thinking, prompt-caching, structured-outputs
- **Deepgram**: deepgram.com/pricing / developers.deepgram.com/docs — models-languages-overview, diarization, pre-recorded-audio
- **AssemblyAI**: assemblyai.com/pricing, /languages/korean
- **Azure**: azure.microsoft.com/pricing/details/speech / prices.azure.com retail API (eastus 실측)
- **Speechmatics**: speechmatics.com/pricing, docs.speechmatics.com/introduction/supported-languages
- **ElevenLabs**: elevenlabs.io/pricing/api, /docs/capabilities/speech-to-text
- **Groq**: console.groq.com/docs/speech-to-text, /docs/model/whisper-large-v3-turbo
- **Gladia**: gladia.io/pricing
- **환율**: Bloomberg USDKRW / Investing.com / XE

---

## 6. 확인하지 못한 것 (추측하지 않았다)

🔴 **1. Alibaba ASR 의 공식 싱가포르 리전 단가를 찾지 못했다.** Model Studio 공개 가격
페이지에 음성 인식 모델 가격표가 게재돼 있지 않다. 본 문서의 $0.000035/초는 ① OpenRouter 가
공시한 Qwen3 ASR Flash 단가와 ② 자체 추정치 83원의 역산이 **정확히 일치**한다는 두 근거로
뒷받침한 값이다. **콘솔 청구서로 대조할 것** — 이 숫자가 틀리면 §0 의 「85%가 받아쓰기」와
§4 의 결론이 함께 흔들린다.

2. Google Dynamic Batch($0.003/분) 할인이 Chirp 3 에 적용되는지. 각주에 `chirp`(V2)만 명시돼
   있다. 적용되지 않으면 Google 은 118원이 아니라 **630원**으로 최고가가 된다.
3. Speechmatics diarization 무료 여부의 공식 명문, 파일 크기·길이 제한, 실시간 단가.
4. OpenAI 전사 모델의 과금 시간 반올림 규칙, `gpt-4o-transcribe-diarize` 공식 지원 언어
   목록에 한국어가 있는지.
5. ElevenLabs 분 단위 과금의 올림/내림 규칙.
6. Speechmatics·Azure 의 **한국어** diarization 품질에 대한 공개 정보.
7. Anthropic 배치 API 지연 SLA, 구조화 출력과 extended thinking 병용 가능 여부.
8. Claude 는 토크나이저가 달라 같은 한국어 원문에서 토큰이 더 나올 수 있다. 표의 79.8원은
   동일 토큰 수 가정이고, +30% 를 가정하면 약 103.7원이 된다.
