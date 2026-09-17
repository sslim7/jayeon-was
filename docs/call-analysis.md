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
- `GET /calls?cursor=`: 통화일시 내림차순(같은 시각이면 통화 ID 내림차순) **페이지당 30건 고정**. transcript/analysis 없이 최상위 `summary` 160자만 준다. `nextCursor`가 있으면 다음 페이지가 있고, 없으면 마지막 페이지다. **서버는 이름 검색을 하지 않는다** — 앱이 전체를 받아 클라이언트에서 거른다. 앱이 보내는 `limit` 파라미터는 서버가 무시한다.
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
