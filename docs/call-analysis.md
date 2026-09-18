# 통화 분석 저장 API

서버는 기기에서 이미 생성한 결과만 저장한다. STT/LLM 실행, 외부 AI 연결, 원본 오디오 업로드 경로는 없다. 운영 전송은 기존 HTTPS Cloud Run ingress를 사용한다.

## 🔴 배포 순서 — 뒤집으면 운영만 깨진다

1. **`redhead-terraform` 먼저 apply.** `apps/nature/call-analysis.tf` 의 `google_firestore_field` 가 `record`/`data` byte 필드의 인덱싱을 면제한다.
2. **그다음 WAS 배포.**

이 순서를 지켜야 하는 이유는 **에뮬레이터가 인덱스 한도를 강제하지 않기** 때문이다. 단위·통합 테스트가 전부 통과해도 운영에서만 쓰기가 실패할 수 있고, 증상은 저장 API의 500 하나뿐이다. 인덱스 면제를 지우거나 apply를 건너뛰면 400 KB짜리 byte 필드가 다시 색인 대상이 된다.

> 배포 전 확인 항목: 면제가 실제로 적용됐는지 콘솔(Firestore → 색인 → 단일 필드 예외)에서 눈으로 확인한다. 색인된 필드 값에는 Firestore가 정하는 크기 한도가 걸리며, **면제하지 않으면 400 KB byte 필드에 대한 쓰기가 거부될 수 있다.** 어느 한도에 어떻게 걸리는지(거부인지 잘림인지)는 문서만으로 단정하지 말고, 스테이징에서 1 MiB 이상 transcript를 한 번 저장해 확인한 뒤 운영에 올린다.

## 엔드포인트

- `PUT /calls/{call_id}`: 앱 `src/types/calls.ts`의 전체 CallRecord (transcript/analysis/ai 필수). 같은 사용자/ID/정규화 payload는 재시도해도 한 번만 저장한다. 다른 payload는 409. 완료된 원문/분석은 이 API로 덮어쓰지 않는다.
  **응답은 목록용 요약(`CallSummary`)뿐이다.** 방금 받은 본문을 그대로 되돌려 주면 응답 직렬화에 같은 크기가 한 벌 더 필요해 인스턴스 메모리가 그만큼 더 든다. 원본은 앱이 이미 갖고 있다.
- `GET /calls?q=&limit=&cursor=`: 통화일시 내림차순(같은 시각이면 통화 ID 내림차순). transcript/analysis 없이 최상위 `summary` 160자만 준다. `limit`은 1~100이고 기본 30이다.
  - `q`는 **이름 부분일치(대소문자 무시) 또는 전화번호 부분일치(뒷자리 포함)** 다. 하이픈·공백·괄호·`+82` 표기 차이는 양쪽 모두 걷어 내고 숫자만 비교한다. 글자와 숫자가 섞이면 글자는 이름, 숫자는 번호를 가리킨다(`김영 7649`). 규칙은 앱 `src/lib/recipient-search.ts`와 같고, 서버는 `internal/recipients/search.go` 한 곳에만 둔다(`GET /sms/history`도 같은 함수를 쓴다).
  - 🔴 **Firestore는 부분 문자열 검색을 인덱스로 못 한다.** 그래서 검색은 최신순으로 문서를 읽어 **서버 메모리에서 거른다.** 한 요청이 훑는 문서 수에는 상한(300건)이 있고, 상한에 걸리면 찾은 만큼만 돌려주고 `nextCursor`를 남긴다. 즉 **검색 중에는 `items`가 `limit`보다 적어도 마지막 페이지가 아니다** — 앱은 `nextCursor`가 사라질 때까지(또는 화면이 찰 때까지) 이어 불러야 한다.
  - 저장 시 검색용 필드(소문자 이름·숫자만 번호)를 따로 심고 prefix 쿼리를 쓰는 방법도 있었지만, **뒷자리 검색은 prefix로 안 되어** "뒷4자리" 같은 파생 필드를 또 심어야 하고 이미 저장된 통화 전부를 백필해야 한다. 통화 요약은 `record` 필드에 통째 JSON으로 들어 있어 백필이 곧 전 문서 재작성이다. 사용자 1명·하루 수십 건 규모에서는 스캔 상한이 있는 메모리 필터가 읽기 비용도 복잡도도 더 싸다(검색 한 번 최대 300 읽기 = 무료 쿼터 50,000/일의 0.6%).
  - 커서는 **검색어에 귀속된다.** `q`를 바꾸면서 이전 커서를 보내면 400이다.
- `GET /calls/{call_id}`: 전체 분석/원문/구조화 할 일 반환. 타 사용자 데이터는 404.

기존 userguard로 활성 계정/비밀번호 변경 상태를 검사한다. 사용자 ID는 JWT에서만 가져오며 클라이언트 owner 필드는 거부한다. 응답은 no-store, 서버 로그에는 요청 본문/통화 원문을 남기지 않는다. 목록에서 디코딩할 수 없는 문서를 만나면 **그 문서만 건너뛰고 커서는 전진한다** — 손상된 한 건이 목록 전체를 막지 않게 하기 위해서다. 무엇이 저장돼 있었는지 로그로도 남기지 않는다.

## 상태 코드

- `400` 형식·값 검증 실패. 같은 본문을 다시 보내도 결과는 같다.
- `409` 같은 ID에 다른 결과가 이미 있다.
- `413` 요청 본문 또는 **실제 저장 바이트**가 한도를 넘었다(`CALL_TOO_LARGE`). 재시도 대상이 아니다.
- `500` 서버 문제. 클라이언트는 재시도해도 된다. **크기 초과가 여기로 새어 나오면 안 된다** — 클라이언트가 일시 오류로 보고 영원히 재시도한다.

## 저장 구조와 크기 한도

Firestore: `users/{uid}/calls/{id}`에는 목록 metadata, 정규화 내용 SHA-256, 서버 동기화 시각, shard/todo 수만 저장한다. `transcript/{index}`에는 JSON byte shard (400000 bytes), `analysis/v1`에는 todos를 제외한 JSON, `todos/{index}`에는 구조화 할 일 JSON 및 향후 완료 플래그를 독립 저장한다. Firestore 문서 한도를 피하고 모든 문서를 동일 transaction으로 발행한다. 쿼리는 오직 부모 문서 `recordedAt`+`documentId`만 쓴다.

🔴 **한도는 요청 크기가 아니라 실제 저장 바이트를 기준으로 건다.** `encoding/json` 기본값은 `<`, `>`, `&`를 `<` 형태로 바꿔 한 글자를 6바이트로 만든다. 그런 문자가 많은 4 MiB 원문이 24 MiB JSON이 되어 Firestore Commit 요청 한도(10 MiB)를 넘기고, 그 결과는 400이 아니라 500이다. 저장·검증 경로는 `SetEscapeHTML(false)`를 쓰는 인코더 하나(`marshalCompact`)만 쓰고, Go가 여전히 escape하는 문자(` `, 제어문자 등)로 남는 증폭은 아래 한도가 막는다.

| 대상 | 한도 | 초과 시 |
| --- | --- | --- |
| 요청 본문 | 6 MiB | 413 |
| `transcript.text` | 4 MiB (UTF-8 바이트) | 400 |
| 저장되는 transcript JSON | 5 MiB | 413 |
| transcript shard 수 | 14 (shard당 400000 bytes) | 413 |
| 저장되는 analysis JSON + todos 합계 | 400000 bytes | 413 |
| todos 개수 | 100 | 400 |
| 한 transaction의 문서 수 | 500 | 413 |
| 한 transaction의 총 바이트 | 8 MiB (Commit 한도 10 MiB에 여유) | 413 |

검증·직렬화·digest는 `prepare` 한 곳에서 한 번에 끝낸다. 같은 데이터를 digest용·저장용으로 두 번 직렬화하지 않고, 직렬화가 끝난 디코딩본은 즉시 버린다.

## 값 규약

날짜는 ISO RFC3339 시각, due_date는 null 또는 YYYY-MM-DD다. **recorded_at은 서버 시각 기준 5분 뒤까지 허용한다** — 여유 없이 거부하면 기기 시계가 1초만 빨라도 그 통화는 영원히 업로드되지 않는다. 저장 시 UTC로 정규화한다. 전화번호는 + 선택적 접두사와 숫자 7~15자리(일반 유선 포함). 연락처 이름은 100자(rune 기준, `internal/sms/history.go`와 같은 기준). 음성 duration/segment는 초 단위 최대 24시간. 누락 배열은 허용하지 않으며 불명확한 owner/due_date는 null이다. ai.processed_on_device=true는 계약 표식이며 서버가 실제 기기 실행을 검증하는 attestation은 아니다.

## 검증

단위 검증: `go test ./internal/calls`. 실제 transaction/원문 shard/멱등/사용자 격리/손상 문서 처리/페이징 경계:

```
firebase emulators:exec --only firestore --project demo-test --config firebase.test.json \
  'FIRESTORE_EMULATOR_HOST=localhost:8095 go test -count=1 ./internal/calls'
```

에뮬레이터 미설정이면 통합 검증은 skip한다. 배포/마이그레이션 작업은 자동 실행하지 않는다.

---

# 서버 통화분석 파이프라인

기기 업로드 경로(`PUT /calls/{id}`)는 **그대로 남아 있다.** 아래는 그 옆에 추가된 서버 경로다. 오디오를 GCS 에 올리면 서버가 전사(ASR)와 분석(LLM)을 대신 돌린다.

🔴 **앱이 쓰는 조회 API 는 바뀌지 않는다.** 진행 상태는 기존 `GET /calls`, `GET /calls/{id}` 로 그대로 보인다. `audio/complete` 시점에 플레이스홀더 통화 레코드를 만들고, 단계가 바뀔 때마다 그 레코드의 요약 필드를 갱신하기 때문이다. 앱에 새 폴링 API 를 만들게 하지 않는 것이 이 설계의 핵심이다.

## 앱이 부르는 순서

```
POST /calls/{id}/audio/upload-url   → 서명 URL 과 객체 경로를 받는다
PUT  <서명 URL>                      → 앱(브라우저)이 GCS 로 **직접** 올린다. 서버는 관여하지 않는다
POST /calls/{id}/audio/complete     → 객체를 확인하고 큐잉. 이때 통화 레코드가 생긴다
GET  /calls/{id}                    → status/stage/progress 를 폴링. COMPLETED 면 transcript+analysis 가 들어 있다
POST /calls/{id}/reanalyze          → (선택) 전사문을 그대로 두고 분석만 다시 돌린다
```

🔴 **서버는 오디오 바이트를 절대 통과시키지 않는다.** 업로드는 앱→GCS 직접이고, 파이프라인은 GCS→공급자 스트리밍이다. 서버가 중계하면 30분 통화 28 MB 가 인스턴스 메모리를 그대로 먹고, Cloud Run 요청 타임아웃(60초) 안에 느린 회선의 업로드가 끝나지 않는다.

### 업로드는 단순 서명 PUT 이다 (resumable 아님)

진행률과 취소는 단순 PUT 으로도 전부 된다 — `XMLHttpRequest.upload.onprogress` 로 진행률, `xhr.abort()`(또는 `AbortController`)로 취소. resumable 이 추가로 주는 것은 **중단 후 이어올리기** 하나뿐인데, 30분 통화 ≈ 28 MB 에서 그것을 위해 왕복 한 번과 세션 수명 관리, `Location` 헤더 파싱, 청크 분할 로직을 앱에 얹을 값어치가 없다. 실패하면 처음부터 다시 올리면 된다.

> 나중에 resumable 이 필요해지면 서버 변경만으로 얹을 수 있다. 버킷 CORS 가 이미 `POST` 와 `Location`·`x-goog-resumable` 노출을 허용하고 있다. **지금은 구현하지 않는다.**

🔴 **서명 URL 로 PUT 할 때 `Content-Type` 헤더를 응답의 `headers` 값과 글자 그대로 똑같이 보내야 한다.** Content-Type 이 서명에 들어가므로 다르면 GCS 가 403 `SignatureDoesNotMatch` 를 돌려주는데, 응답만 봐서는 원인이 전혀 드러나지 않는다.

🔴 **서명 URL 만료는 60분이다.** 100 MB 를 느린 회선으로 올리는 동안 URL 이 만료되면 업로드가 중간에 401 로 죽는데, 그 실패는 브라우저에서만 보이고 서버 로그에는 아무것도 남지 않는다.

🔴 **객체 경로는 결정적이다**: `calls/{uid}/{callId}/audio{ext}`. 랜덤 이름을 쓰면 업로드 재시도마다 고아 객체가 생기고 보관 기간(366일) 내내 요금이 나간다. 같은 경로면 재시도가 그냥 덮어쓴다. 그래서 `upload-url` 은 `complete` 전이라면 몇 번이든 다시 부를 수 있다(큐잉 후에는 409 — 오디오를 갈아 끼우면 전사문과 원본이 어긋난다).

## 상태 기계

```
AWAITING_UPLOAD → QUEUED → ASR_RUNNING → ASR_POLLING ⇄ ASR_POLLING → TRANSCRIBED → ANALYZING → COMPLETED
                                      ↘ TRANSCRIPTION_FAILED                     ↘ ANALYSIS_FAILED
```

| 상태 | 뜻 |
| --- | --- |
| `AWAITING_UPLOAD` | `upload-url` 로 작업 문서만 만든 상태. tick 이 집지 않는다. |
| `QUEUED` | `audio/complete` 가 객체를 확인하고 큐잉했다. |
| `ASR_RUNNING` | 🔴 공급자 `Start` 를 **부르기 직전에** 저장하는 상태. 호출 도중 인스턴스가 죽으면 공급자 쪽에는 작업이 생겼는데 우리에게는 토큰이 없다 — 그 사실(상태=ASR_RUNNING, 토큰=빈 값)이 문서에 남아야 다음 tick 이 「토큰을 잃어버린 작업」으로 알아보고 다시 집는다. |
| `ASR_POLLING` | 토큰을 얻어 폴링 중. 진행 중이면 이 상태에 머문다. |
| `TRANSCRIBED` | 전사문을 저장했다. **독립 상태다 — 분석만 실패해도 전사를 다시 하지 않는다.** ASR 이 가장 비싸고 느린 단계다. |
| `ANALYZING` | LLM 호출 직전에 저장하는 상태. |
| `COMPLETED` / `TRANSCRIPTION_FAILED` / `ANALYSIS_FAILED` | 종료 상태. 스윕 쿼리의 status 목록에 없어 다시 조회되지 않는다. |

### 재시도

- 간격: `1m, 2m, 4m, 8m, 16m`, 상한 `30m`.
- 🔴 **시도 상한은 단계별로 다르다: ASR 3회, 분석 5회.** 공급자는 모르는 실패 코드를 `Retryable` 로 돌려주므로 **이 상한이 곧 우리가 지불하는 금액의 상한**이다. ASR 은 과금 단위가 오디오 초라 1시간 통화를 5번 재시도하면 5시간치 요금이 나간다. 분석은 저장된 전사문을 재사용하므로 훨씬 싸다.
- `callai.Retryable(err) == false` 면 **즉시** 확정 실패한다(재시도 낭비 금지). `KindContentFiltered` 도 즉시 확정이되 분류를 따로 남겨 사용자 안내 문구를 나눌 수 있게 한다.
- 🔴 **데드라인은 누구 것이었는지로 갈린다.** ①tick 예산·요청 취소(우리 쪽)는 실패가 아니다 — 상태와 시도 횟수를 그대로 두고 lease 만 풀어 다음 tick 에 넘긴다. ②공급자가 우리가 준 시간 안에 답하지 못한 것은 진짜 실패다 — `*callai.Error{Kind: KindRetryable, Code: "ProviderTimeout"}` 로 감싸여 시도 횟수를 쓰고 백오프가 걸린다. 2026-09-18 에 이 둘이 한 줄 `errors.Is(err, context.DeadlineExceeded)` 에 뭉개져 25분짜리 통화가 재시도 한 번 없이 `ANALYSIS_FAILED` 로 확정됐다.
- 🔴 **`callai.Retryable` 은 분류(`*callai.Error`)를 ctx 검사보다 먼저 본다.** 순서를 뒤집으면 KindRetryable 로 감싼 공급자 타임아웃이 안에 든 `context.DeadlineExceeded` 때문에 재시도 불가로 뒤집힌다.
- `KindInputUnavailable` 은 공급자가 우리 오디오를 받아 가지 못했다는 뜻이라 폴링을 이어가 봐야 끝나지 않는다. `QUEUED` 로 되돌려 처음부터 다시 올린다(단 ASR 시도 상한은 그대로 적용).
- 🔴 **자동으로 회복 가능한 실패는 사용자에게 노출하지 않는다.** 재시도 예약 중에는 상태가 종료 상태가 아니므로 통화 레코드의 `error` 가 비어 나가고, 앱은 「분석 중」을 그대로 보여 준다. 사용자는 화면을 나가도 되고 돌아왔을 때 진행돼 있으면 된다. `summaryRecord()` 가 **종료 상태에서만** `error` 를 채우는 것이 그 약속이다 — 그 조건을 넓히지 마라.
- 🔴 **종료 상태의 코드는 「사용자가 할 수 있는 일」을 말해야 한다.** 회복 가능한 실패를 상한까지 시도하고 멈췄으면 마지막 공급자 코드가 아니라 `RETRIES_EXHAUSTED` 를 내보낸다. 마지막 공급자 코드를 그대로 내보내면 앱이 그것을 「일시적인 오류입니다. 잠시 뒤 다시 시도해 주세요」로 옮기는데, 서버는 이미 다 해 본 뒤라 사용자에게 떠넘기는 말이 된다(2026-09-18 에 실제로 화면에 뜬 문구다). 마지막 공급자 코드·HTTP status·request_id 는 로그와 작업 문서에 남는다.
- 🔴 **누적 사용량(`usage.audioSeconds` 등)은 재시도분까지 전부 더해 작업 문서에 남긴다.** 실패한 시도의 요금도 실제로 청구되므로, 성공분만 기록하면 원가 집계가 청구서와 어긋나고 그 차이는 아무도 설명하지 못한다.

### 🔴 분석을 먼저 저장하고 통화 레코드를 나중에 확정하는 순서

`ANALYZING` 에서 LLM 이 성공하면 **분석 JSON 을 `callJobs/{id}/analysis/v1` 에 먼저 저장하고(`hasAnalysis=true`), 그 다음** 통화 레코드에 확정한다. 이 순서 덕분에 레코드 저장이 일시 실패해 재시도로 돌아와도 비싼 LLM 호출은 한 번뿐이다(`hasAnalysis` 가 true 면 저장된 분석을 재사용한다).

## tick

`POST /internal/calls/tick` — Cloud Scheduler 가 1분마다 두드린다.

### 🔴 이 핸들러가 유일한 방어선이다

Cloud Run 서비스가 `allow_unauthenticated = true` 다. **Cloud Run IAM 은 이 경로를 전혀 막아 주지 않는다** — 아무나 curl 로 때릴 수 있고, 통과시키면 그 요청마다 외부 ASR/LLM 요금이 나간다. 검증 항목은 네 개다:

1. 구글 서명 (`idtoken.Validate`)
2. `aud` 일치 — 🔴 **Cloud Run 서비스 URL 이고 경로가 붙지 않는다**(`CALL_TICK_AUDIENCE`)
3. `email` 클레임이 `CALL_TICK_CALLER` 와 일치
4. 🔴 `email_verified == true` — 빠뜨리면 검증되지 않은 이메일 클레임을 그대로 믿게 된다

검증 실패 시 무슨 일이 있어도 200 을 돌려주지 않는다. 실패 이유는 응답에 적지 않고 토큰 값은 로그에도 남기지 않는다.

로컬·테스트에서는 `CALL_TICK_TOKEN` 공유 비밀을 `Authorization: Bearer` 로 보낼 수 있다(상수시간 비교). 🔴 **`CALL_TICK_CALLER` 와 `CALL_TICK_TOKEN` 이 둘 다 없으면 라우트를 아예 등록하지 않는다** — 설정 실수 하나가 곧 무방비 라우트다. `userguard` 로는 감싸지 않는다(사용자 JWT 와 무관한 경로다).

### 50초 예산과 🔴 응답 후 고루틴 금지

Cloud Run 이 `timeout 60s` / `cpu_idle = true` / `min_instances = 0` 이다. **응답을 보낸 뒤 계속 도는 고루틴은 금지다** — 응답 직후 CPU 가 스로틀돼 뒤에서 돌던 작업이 로그 한 줄 없이 조용히 끊긴다. 모든 일은 요청 안에서 끝나고, 못 끝낸 것은 상태로 저장해 다음 tick 에 넘긴다.

tick 은 50초를 쓰고 나머지를 상태 저장·응답에 남긴다. 한 작업을 예산이 허락하는 한 여러 단계 전진시키되(Start → 5초 대기 → Poll → … → TRANSCRIBED → ANALYZING → COMPLETED), 남은 예산이 모자라면 상태를 저장하고 반환한다. 폴링 사이 대기는 `time.Sleep` 이 아니라 `ctx` 를 존중하는 타이머다.

예산이 모자라 멈춘 작업은 lease 를 바로 풀어 다음 tick 이 이어받게 한다. 풀지 않으면 lease 만료(2분)까지 그 통화가 놀게 된다.

#### 🔴 남은 예산이 모자라면 공급자를 아예 부르지 않는다

단계마다 필요한 시간이 다르다. 하나의 상수로 판단하면 **가장 오래 걸리는 단계가 조용히 먼저 깨진다** — 2026-09-18 에 ASR 폴링에 예산을 쓴 tick 이 남은 13초로 LLM 을 불렀고, 돌아온 것은 타임아웃뿐이었다.

| 상수 | 값 | 무엇 |
|---|---|---|
| `tickBudget` | 50s | 한 tick 이 쓰는 시간 |
| `tailReserve` | 3s | 응답을 쓸 시간 |
| `saveReserve` | 5s | 공급자 호출이 끝난 뒤 **결과를 저장할** 시간 |
| `asrStartNeed` | 30s | 업로드 정책 + 오디오 전체 업로드 + 전사 제출 |
| `asrPollNeed` | 15s | 작업 조회 + 완료 시 전사 결과 내려받기 |
| `llmCallNeed` | 45s | 전사문 정리(창 40s + `saveReserve`) |
| `leaseDuration` | 2m | lease |
| `CALL_AI_TIMEOUT_SECONDS` | 50s | 공급자 HTTP backstop |

지켜야 하는 관계: `leaseDuration > tickBudget`, `tickBudget < Cloud Run(60s) − tailReserve`, `tickBudget ≥ llmCallNeed`, `CALL_AI_TIMEOUT_SECONDS > tickBudget − saveReserve`(= 우리가 거는 최대 창 45s).

🔴 `llmCallNeed` 는 **실측에서 나왔다.** `internal/callai/live_test.go` 의 `TestLiveAnalyzeLatency` 로 25분 분량(프롬프트 60KB / 15.6k 토큰) 합성 전사문을 `qwen3.7-plus` 에 넣어 잰 값이 28.5s / 29.8s / 33.6s 이고, 50분 분량(30.5k 토큰)도 31.1s / 32.3s 다(지연은 입력 길이보다 **출력 토큰 수**가 지배한다). `thinking_budget=0` 이면 13.0s / 13.5s 로 떨어진다. **모델·프롬프트·thinking 설정을 바꾸면 다시 재고 이 값을 고쳐야 한다.**

예산이 모자라 시작하지 못한 단계는 **실패가 아니다**: 상태·시도 횟수를 그대로 두고 lease 만 풀어 다음 tick 에 넘긴다(`nextAttemptAt` 오름차순이라 다음 tick 이 먼저 집는다). 앱에는 「분석 중」이 그대로 유지된다.

⚠️ 무한 미루기는 `deferCount`/`maxDefers`(5)가 막는다. 세는 조건은 **「빈 tick 이어도 그 단계가 들어가지 않을 때」뿐이다** — 앞 통화가 예산을 써서 밀린 것까지 세면 큐가 밀리는 날 멀쩡한 통화가 줄줄이 확정 실패한다. 상한을 넘으면 `BUDGET_TOO_SMALL` 로 종료 상태에 세운다(사람이 예산 상수나 Cloud Run 타임아웃을 손봐야 한다는 뜻이다).

응답: `{"claimed":n,"advanced":n,"failed":n,"remaining_budget_ms":n}` — 🔴 **통화 내용은 한 글자도 넣지 않는다.**

### lease 와 중복 실행

Scheduler 주기는 1분, lease 는 **2분**이다. tick 이 1분을 넘겨 다음 tick 과 겹쳐도(Cloud Run concurrency 80 / max_instances 3 이라 실제로 겹친다) 앞 tick 이 집은 작업은 lease 가 살아 있어 뒤 tick 의 스윕에 잡히지 않는다. lease 가 주기보다 짧으면 두 tick 이 같은 작업을 동시에 붙잡아 공급자를 두 번 부른다 — **요금이 그대로 두 배다.**

Scheduler 는 `retry_count = 0` 이다. 작업 단위 재시도는 우리 `nextAttemptAt` 이 소유한다.

### 스윕 쿼리 — 인프라가 만든 복합 인덱스 모양 그대로

```go
fs.Collection("callJobs").
   Where("status", "in", []string{"QUEUED","ASR_RUNNING","ASR_POLLING","TRANSCRIBED","ANALYZING"}).
   OrderBy("nextAttemptAt", firestore.Asc).
   Limit(batch)
```

🔴 **`Where("nextAttemptAt","<=",now)` 를 붙이면 안 된다.** 부등호가 붙는 순간 준비된 인덱스 `callJobs(status ASC, nextAttemptAt ASC)` 모양에서 벗어나고, 증상은 **운영에서만 나는 `FAILED_PRECONDITION`** 이다(에뮬레이터는 인덱스를 요구하지 않아 테스트가 전부 통과한다). 대신 오름차순 정렬을 이용해 코드에서 미래 항목을 끊는다 — 첫 번째가 미래면 뒤도 전부 미래다.

`nextAttemptAt` 하나가 **스케줄링과 lease 를 겸한다**:
- 큐잉/재시도 예약: `nextAttemptAt = now + backoff`
- lease 획득: transaction 안에서 아직 `<= now` 인지 다시 확인하고 `now + 2m` 으로 민다

🔴 쿼리 결과를 그대로 믿으면 안 된다. 쿼리와 쓰기 사이에 다른 tick 이 같은 작업을 집어 갈 수 있으므로 transaction 안에서 한 번 더 확인한다.

## 저장 구조

작업 문서는 **최상위 `callJobs/{callId}`** 다(uid 는 필드). 기존 `users/{uid}/calls` 문서의 인덱스 면제 설정(`call-analysis.tf`, `prevent_destroy`)을 건드리지 않기 위해서다. tick 이 전체 사용자를 가로질러 한 번에 조회해야 하는 것도 이유다.

🔴 **`callJobs/{callId}` 본문 문서에는 큰 byte 필드를 절대 넣지 마라.** 이 컬렉션에는 색인 면제가 없어 Firestore 단일 필드 색인 항목 한도(1500 bytes)에 걸려 쓰기가 거부된다. 전사문과 분석은 하위 컬렉션에 넣는다:

| 경로 | 내용 |
| --- | --- |
| `callJobs/{callId}/transcript/{00000}` | 전사문 JSON byte shard (`data` 필드) |
| `callJobs/{callId}/analysis/v1` | 분석 JSON (`data` 필드) |

🔴 **이 이름을 바꾸면 안 된다.** `google_firestore_field` 의 `collection` 인자는 **컬렉션 그룹 ID** 이고, 컬렉션 그룹 ID 는 경로의 **마지막 세그먼트** 이름이다. 기존 면제가 `{calls:record, transcript:data, analysis:data, todos:data}` 로 걸려 있으므로 `callJobs/{id}/transcript/{i}` 와 `callJobs/{id}/analysis/v1` 에도 **그대로 적용된다**(terraform 변경이 필요 없다). 이름을 바꾸면 면제가 안 먹고, 그 증상은 운영에서만 나는 쓰기 거부다.

작업 문서 본문 필드(전부 작은 값): `uid`, `callId`, `status`(내부 상태), `stage`, `progress`, `audio{bucket,object,size,contentType,fileName}`, 연락처·통화 메타, `createdAt`/`updatedAt`, `nextAttemptAt`, `asrAttempt`/`analysisAttempt`, `asrToken`, `asrRequestId`/`llmRequestId`, 공급자·모델·프롬프트 버전, `usage{...}`, `errorCode`/`errorKind`/`errorAt`, `transcriptShards`, `hasAnalysis`, `deferCount`.

🔴 Firestore 필드 `status` 와 **API 응답의 `status` 는 같은 단어지만 값 집합이 완전히 다르다**(아래 사상표 참고).

## 🔴 status 사상 — 앱의 STAGE_INDEX 는 닫힌 집합이다

앱(`src/lib/call-progress.ts`)은 `STAGE_INDEX[status]` 로 진행률을 계산한다. **모르는 값이 들어오면 `undefined` 가 되어 진행률과 단계 표시가 통째로 깨진다.** 응답 필드를 *추가*하는 것은 안전하지만 기존 필드의 **값 집합을 넓히는 것은 안전하지 않다.** 그래서 내부 상태는 `job_state` 로 따로 내보내고 `status` 는 앱 어휘로 사상한다.

| 내부 `callJobs.status` | API `status` |
| --- | --- |
| `AWAITING_UPLOAD` | `PENDING` |
| `QUEUED` | `PREPARING` |
| `ASR_RUNNING`, `ASR_POLLING` | `TRANSCRIBING` |
| `TRANSCRIBED`, `ANALYZING` | `ANALYZING` |
| `COMPLETED` | `COMPLETED` |
| `TRANSCRIPTION_FAILED` | `TRANSCRIPTION_FAILED` |
| `ANALYSIS_FAILED` | `ANALYSIS_FAILED` |

`UPLOADING` / `UPLOAD_*` 는 기기 경로 전용이라 서버는 쓰지 않는다. **기기 경로(`PUT /calls/{id}`)가 만드는 레코드의 `status` 는 지금처럼 `COMPLETED` 고정이고 `job_state` 는 붙지 않는다.**

## 응답에 추가된 필드

`CallSummary`/`CallDetail` 에 다음이 붙었다. 전부 `omitempty` 이고 **응답 전용**이다.

| 필드 | 목록 | 상세 | 뜻 |
| --- | --- | --- | --- |
| `job_state` | ✅ | ✅ | 내부 작업 상태 원문. 더 세밀한 표시가 필요할 때 쓴다. |
| `stage` | ✅ | ✅ | 사용자에게 그대로 보여 줄 한국어 단계명(예 `받아쓰는 중`). |
| `has_audio` | ✅ | ✅ | GCS 에 원본 오디오가 있는지. |
| `ai.provider` | ✅ | ✅ | 서버 파이프라인이 쓴 공급자 이름. |
| `audio_url` | ❌ | ✅ | 재생용 **서명 GET URL**(15분 만료). |

🔴 **`PUT /calls/{id}` 의 요청 스키마는 동결이다.** 핸들러가 `DisallowUnknownFields` 라 **우리가 응답에 붙인 필드를 앱이 그대로 되돌려 보내는 순간 400** 이 된다. 그래서 위 필드는 ①`omitempty` 로 기기 경로 응답에 나타나지 않게 하고 ②기기 경로 검증이 **요청에 들어오면 400 으로 거부**한다. `CallInput` 스키마는 바뀌지 않았다.

### `audio_url`

- 🔴 **Firestore 에 저장하지 않는다.** 만료되는 값이라 저장하면 굳은 URL 이 남아 그것을 읽은 앱이 403 만 받는다. `GET /calls/{id}` 를 처리할 때마다 새로 발급한다.
- 🔴 **목록에는 넣지 않는다.** 목록 한 페이지(최대 100건)마다 그만큼 서명을 만드는 것은 목록 조회에 불필요한 비용이고(서명마다 IAM signBlob 왕복이 날 수 있다) 목록 화면은 재생 버튼을 쓰지 않는다. 목록에는 `has_audio` 만 준다.
- **값이 없는 경우는 둘 다 정상이다:** ①기기 업로드 경로로 저장된 통화(GCS 에 원본이 없다) ②보관 기간(366일)이 지나 원본이 삭제된 통화. 앱은 값이 없으면 재생 버튼을 잠근다.
- 🔴 **보관 만료를 「분석 실패」로 보이게 하면 안 된다.** `status` 는 `COMPLETED` 그대로이고 `audio_url` 만 없다. 전사문·분석은 Firestore 에 영구히 남는다.
- 🔴 매 조회마다 `object.Attrs()` 로 존재 확인을 **하지 않는다.** 통화 상세를 열 때마다 GCS 왕복이 붙는데 서명 자체는 존재 여부와 무관한 로컬 계산이다. 만료된 객체의 URL 은 발급되지만 GET 하면 404 가 나고, 앱은 이미 그 경로를 「재생 불가」로 처리한다.

## 재분석

`POST /calls/{id}/reanalyze` 는 **오디오를 다시 전사하지 않는다.** `COMPLETED`/`ANALYSIS_FAILED`/`TRANSCRIBED` 이고 전사문이 있으면 `hasAnalysis=false`, `analysisAttempt=0`, 상태 `TRANSCRIBED` 로 되돌린다. 작업 문서의 shard 가 없으면 통화 레코드에서 전사문을 읽어 다시 써 넣는다. 전사문이 아예 없으면 409.

**보관 기간과의 관계**: 오디오를 다시 전사해야 하는 경로만 366일 제한을 받는다. 재분석은 전사문만 쓰므로 **오디오가 사라진 뒤에도 동작한다.**

## GCS 권한 — 🔴 버킷 메타데이터를 읽지 마라

런타임 서비스 계정에는 `roles/storage.objectAdmin` 만 있고 **`storage.buckets.get` 이 없다.** `bucket.Attrs(ctx)` 를 부르는 코드를 새로 쓰는 순간 최소 권한을 넓혀야 하고, 그것을 알아차리는 시점은 보통 「운영에서만 403 이 난다」는 신고를 받은 뒤다. 기동 시 버킷 존재 확인, 헬스체크에서의 버킷 조회도 금지다. 쓰는 것은 셋뿐이다: `BucketHandle.SignedURL`, `ObjectHandle.Attrs`(= `storage.objects.get`), `ObjectHandle.NewReader`.

서명 URL 은 런타임 SA 에 자기 자신에 대한 `roles/iam.serviceAccountTokenCreator` 가 붙어 있어 개인키 없이 IAM `signBlob` 으로 서명된다. ⚠️ **로컬 개발에서는 사용자 ADC 가 다른 경로를 타서 이 실패가 재현되지 않는다.** 운영에서 서명 URL 발급이 실패하면 십중팔구 `iam.serviceAccounts.signBlob` 권한이다 — 원인은 로그에만 남고 응답은 500 이다.

## 환경변수

| 이름 | 없으면 | 설명 |
| --- | --- | --- |
| `CALL_AUDIO_BUCKET` | **오디오·tick 라우트를 켜지 않는다** | 오디오 버킷 이름 |
| `CALL_AUDIO_RETENTION_DAYS` | 366 | 응답·문서용 정보일 뿐. **삭제는 GCS 수명주기 정책이 한다** |
| `CALL_TICK_AUDIENCE` | OIDC 검증 불가 | Cloud Run 서비스 URL. 🔴 tick 경로를 붙이지 않는다 |
| `CALL_TICK_CALLER` | **tick 라우트를 켜지 않는다** | 허용할 Scheduler 서비스 계정 이메일 |
| `CALL_TICK_TOKEN` | — | 로컬·테스트 전용 공유 비밀. 운영에서는 비워 둔다 |
| `CALL_TICK_BATCH` | 5 | 한 tick 이 집는 작업 수 |
| `CALL_ASR_LANGUAGE` | `ko` | ASR 언어 힌트 |

공급자 설정(`CALL_ASR_PROVIDER`, `CALL_LLM_PROVIDER`, `CALL_AI_*` 등)은 `internal/callai/config.go` 가 소유한다.

🔴 **설정이 모자라도 기동은 계속된다.** AI 설정이나 버킷 이름 하나 때문에 SMS·로그인까지 죽을 이유가 없다 — 모자라면 오디오·tick 라우트만 꺼지고 기존 기기 업로드 경로는 그대로 돈다(`internal/admin.Register` 관례와 같다).

## 검증

```
go test ./internal/calls/...        # 상태 기계 전체를 네트워크 없이 돈다(가짜 공급자)

firebase emulators:exec --only firestore --project demo-test --config firebase.test.json \
  'FIRESTORE_EMULATOR_HOST=localhost:8095 go test -count=1 ./internal/calls/...'
```

상태 기계·재시도 상한·lease 논리는 저장소를 인터페이스로 뽑아 메모리 구현으로 검증한다 — 그것을 확인하려고 매번 에뮬레이터를 띄우면 테스트가 개발 머신 설정에 묶인다. 에뮬레이터 테스트는 메모리 구현이 흉내 낼 수 없는 것만 본다: 실제 transaction, 실제 쿼리 모양, 조각 문서 삭제, 구버전 문서 하위호환.
