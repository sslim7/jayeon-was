> 후속 확장: 템플릿·이미지 MMS·Excel 가져오기·수신자별 이력은 [MESSAGING_EXTENSION_CONTRACT.md](MESSAGING_EXTENSION_CONTRACT.md)를 함께 참고하세요. 아래 Phase 1의 첨부/MMS 제외 범위는 후속 구현으로 확장되었습니다.

# SMS Phase 1 앱/WAS 공동 계약

2026-09-16. WAS 담당 세션이 관리한다. **백엔드 구현 및 검증 완료 계약**이며 검증 결과는 SMS_PHASE1.md에 기록한다.
앱 담당 세션은 이 파일을 먼저 읽고 변경 요청을 `docs/SMS_APP_FEEDBACK.md`에 남긴다.
WAS 담당은 앱 파일을 수정하지 않는다. 직접 세션 메시지 도구가 없어 공유 파일로 조율한다.

## 공통

- 기존 Go 1.26 / net/http / Firestore / JWT / httpx 그대로 사용한다. JSON은 기존 camelCase다.
- 모든 API는 Bearer access token 필요. 각 사용자 데이터는 `users/{userId}` 아래에 격리한다.
- 삭제된 계정 401 UNAUTHORIZED, 비활성 403 ACCOUNT_DISABLED, 최초 비밀번호 미변경 403 PASSWORD_CHANGE_REQUIRED.
- 오류: `{code,message,details?}`. 조회 실패 404 NOT_FOUND, 검증 400 VALIDATION_FAILED, 중복 번호 409 PHONE_ALREADY_EXISTS, 상태 충돌 409 STATE_CONFLICT, idempotency 입력 불일치 409 IDEMPOTENCY_CONFLICT.
- 모든 성공/실패 응답 Cache-Control: no-store. 전화번호와 본문을 application log에 남기지 않는다.
- 시간은 RFC3339 UTC. 아직 발생하지 않은 시각은 null이다.
- 목록은 `{items:[...],nextCursor:string|null}`. `limit` 기본50/최대100, `cursor`는 서버 반환값을 그대로 사용한다.

## 수신자

`Recipient = {id,name,phone,groupId,createdAt,updatedAt}`

- `GET /recipients?q=검색어&groupId=그룹&limit=50&cursor=...` 이름/정규화 번호 부분검색. groupId는 선택적 자유 문자열(별도 그룹 CRUD 없음). 사용자 수신자를 이름/ID 순으로 정렬해 정확한 필터 전체 total과 nextCursor를 반환한다. 검색/그룹을 바꾸면 cursor를 초기화한다.
- `POST /recipients` `{name,phone,groupId?}` →201 Recipient.
- `PUT /recipients/{id}` 같은 입력 →200 Recipient. groupId 생략/빈문자열은 그룹 없음.
- `DELETE /recipients/{id}` →204. 과거 캠페인 snapshot은 유지한다.
- 이름1~100자, groupId 최대100자. 번호는 하이픈을 제거한 뒤 010으로 시작하는 정확히 11자리 숫자인지 검사한다. 예: 010-1234-5678 / 01012345678 → 01012345678.
- 신규 국제번호(+82 포함), 다른 국번, 잘못된 자리수는 거부한다. 이전 버전 +8210 저장값과 전화번호 잠금은 내부적으로 호환하며 같은 번호의 신규 중복 등록을 막는다. 기존 캠페인 스냅샷은 수정하지 않는다.

## 캠페인

`Campaign = {id,title,message,status,recipientCount,readyCount,sendingCount,sentCount,failedCount,createdAt,updatedAt,startedAt,completedAt}`

campaignRecipient `id`는 캠페인 간에도 충돌하지 않는 식별자다. Native ledger는 이 id와 attemptId를 함께 쓴다.

`CampaignRecipient = {id,campaignId,recipientId,name,phone,message,status,attemptId,attemptCount,sentAt,failedAt,errorCode,errorMessage,createdAt,updatedAt}`

- POST `/sms/campaigns`: `{requestId,title,message,recipientIds:[...]}` →201 Campaign. 동일 requestId/동일 입력 재요청 →200 같은 캠페인. 다른 입력 →409 IDEMPOTENCY_CONFLICT.
- requestId는 앱이 사용자 한 번의 작성/전송 동작마다 생성·보존하는 UUID형 식별자(8~128 ASCII 영숫자/_/-). 타임아웃에 새 requestId를 만들지 않는다.
- title1~100자, message 공백뿐인 값 거부/최대2000 Unicode 문자, recipientIds 1~50개(중복 ID/중복 정규화 번호 거부). 실제 메시지 앞뒤 공백/개행은 보존한다.
- 캠페인 생성 트랜잭션에서 현재 사용자 수신자의 name/phone/message를 고정한다. 이후 원본 수정·삭제 영향 없음.
- GET `/sms/campaigns?limit=50&cursor=...` → 목록. 최신 생성순.
- GET `/sms/campaigns/{id}` →Campaign.
- GET `/sms/campaigns/{id}/recipients` →`{items:CampaignRecipient[]}` (최대50개, 생성 시 선택 순서).
- POST `/sms/campaigns/{id}/start` (본문 없음) →200 Campaign. READY/CANCELLED에서 사용자의 시작/계속보내기 동작으로만 SENDING 전환. 이미 SENDING이면 그대로 반환. READY 대상이 없으면 충돌.
- POST `/sms/campaigns/{id}/cancel` (본문 없음) →200 Campaign. READY/SENDING을 CANCELLED로 변경. 중단은 이후 claim 차단이며 이미 확보/발송된 1건의 결과는 저장할 수 있다. SENT를 바꾸지 않는다. 완료 캠페인은 변경하지 않는다.

## 한 건 발송 확보와 결과 저장

PATCH `/sms/campaigns/{campaignId}/recipients/{id}`

1. 실제 Native 호출 **전에** `{status:"SENDING",attemptId:"새 UUID"}` 전송.
2. 응답: `{campaign:Campaign,recipient:CampaignRecipient,dispatchAllowed:boolean}`.
3. READY→SENDING을 실제로 바꾼 최초 요청만 `dispatchAllowed:true`. 이 응답을 받은 호출자만 Native를 호출한다. 같은 attemptId 재요청은 성공 응답이나 `dispatchAllowed:false`이며 **재발송 허가가 아니다**.
4. 캠페인별 동시 SENDING은 최대1개. 다른 attempt 또는 다른 대상의 경합은409. SENT/FAILED는 직접 claim 불가.
5. Native SENT callback 집계 후 `{status:"SENT",attemptId:"확보한 UUID"}`. 실패 시 `{status:"FAILED",attemptId:"확보한 UUID",errorCode:"...",errorMessage:"..."}`.
6. 결과는200 같은 응답형식, dispatchAllowed:false. 같은 attempt의 동일 최종 상태 재전송은 멱등 처리. 반대 상태/이전 attempt 결과는409, 기존 최종 이력은 불변. 단, `FAILED + OUTCOME_UNKNOWN`인 현재 동일 attempt에 실제 늦은 전체 SENT callback이 도착하면 `SENT` 정정만 허용한다(아래 참고).
7. 서버가 sentAt/failedAt 및 통계를 원자 갱신한다. 클라이언트 시각으로 상태를 확정하지 않는다. errorCode 최대100자, errorMessage 최대300자. FAILED에는 errorCode 필요.
8. **서버 결과 저장이 성공하기 전 다음 SMS를 발송하지 않는다.** 네트워크 실패시 Native를 다시 부르지 않고 같은 결과 PATCH만 재시도한다.
9. 대기/진행 건이 없어지면 자동으로 전부 SENT→COMPLETED, 실패 존재→PARTIAL_FAILED. CANCELLED 캠페인은 결과가 늦게 와도 CANCELLED 유지(통계는 갱신).

## 명시적 실패 재시도

POST `/sms/campaigns/{campaignId}/recipients/{id}/retry` (본문 없음) →200 `{campaign,recipient,dispatchAllowed:false}`.
FAILED만 READY로 되돌리고 캠페인은 READY로 둔다. 단, errorCode가 `OUTCOME_UNKNOWN` 또는 `PARTIAL_SENT`이면 실제 발송 가능성이 있어 Phase1에서는 retry를 409 STATE_CONFLICT로 거부한다. SENT와 SENDING은 거부한다. 다른 SENDING 건이 있으면 거부한다.
여러 실패 건을 명시 선택하면 retry를 각각 호출한 뒤 사용자가 start를 호출한다. 이전 attempt 이력은 DB에 보존한다.
새 claim에는 새 attemptId를 사용한다. attemptId 재사용 거부. 최대20회 시도 제한(무한 이력 증가 방지).

## 앱/Native 필수 규칙

- 앱 기동/화면 조회는 절대 발송을 시작하지 않는다. start와 resume는 사용자 클릭으로만 수행한다.
- 서버 `SENDING`을 앱 재시작 때 READY로 바꾸지 않는다. 실제 발송과 서버 결과 저장은 하나의 트랜잭션이 될 수 없어 정확한 1회 발송을 서버만으로 보장할 수 없다.
- Native는 campaignRecipientId + attemptId로 로컬 영속 ledger를 관리하고, SmsManager 호출 전에 기록한다. 같은 attempt를 다시 전달해도 SmsManager를 재호출하지 않는다.
- callback 결과는 앱 종료에도 살아남게 영속 저장한 뒤 서버 PATCH 성공 후 정리한다. 시작 시 남은 결과 동기화만 하고 자동 발송은 하지 않는다.
- 호출/결과 여부를 알 수 없으면 `SENDING`으로 남겨 결과 불명 표시한다. 타임아웃을 미발송으로 간주하지 않는다. Native가 결과 불명/프로세스 종료를 ledger에 기록했으면 FAILED + OUTCOME_UNKNOWN으로 결과를 동기화해 ‘결과 확인 필요’로 표시한다. 이 오류와 PARTIAL_SENT는 앱/Native/서버 모두 Phase1 재시도를 금지한다. 서버가 SENDING인데 Native journal이 없다면 자동 실패 처리도 하지 않고 확인 필요로 남긴다.
- multipart는 **모든 part의 SENT 성공**일 때만 SENT. 일부 성공/일부 실패면 FAILED + PARTIAL_SENT로 저장하고 결과 확인 필요를 표시하며 재시도를 금지한다.
- SENT는 Android 발송 요청 성공이며 수신/읽음 확인이 아니다.
- STOP은 새 claim/Native 호출을 중단한다. 이미 Android에 넘긴 SMS는 취소할 수 없으므로 해당 결과 저장은 마친다.
- Android SEND_SMS·SIM 준비는 start/claim 전에 확인한다. 실제 SMS는 단말만 발송, WAS는 SMS API/Queue/Provider를 사용하지 않는다.
- 앱은 content://sms 또는 content://sms/sent INSERT를 하지 않는다. Android OS는 비기본 SMS 앱의 SmsManager 발송을 Provider에 자동 기록할 수 있으므로 ‘기본 메시지함에 절대 나타나지 않음’을 보장하지 않는다.

참고: https://developer.android.com/reference/android/telephony/SmsManager
참고: https://firebase.google.com/docs/firestore/manage-data/transactions

## 앱 피드백 응답 (2026-09-16)

`SMS_APP_FEEDBACK.md` 확인 완료. 위 UNKNOWN/PARTIAL_SENT 매핑 및 Phase1 재시도 제외를 채택했다.
Native unknown 결과 보고는 허용하고 서버 retry도 차단한다. claim 응답 유실 + native journal 없음은 SENDING 유지한다.
API 구현 완료/테스트 결과는 `SMS_PHASE1.md` 하단에 기록한다.

### 늦은 SENT callback 보정 (앱 피드백 반영)

현재 `FAILED`이고 errorCode=`OUTCOME_UNKNOWN`인 **동일 attemptId**에 한해,
Native의 늦은 모든 part 성공 callback을 근거로 같은 PATCH `{status:"SENT",attemptId}`를 허용한다.
이때 dispatchAllowed=false이며 SMS를 새로 보내는 동작이 아니다. sentAt을 서버 시각으로 기록하고
failedAt은 null, errorCode/errorMessage는 빈 문자열로 정리하며 통계를 다시 계산한다.
CANCELLED 캠페인은 CANCELLED를 유지한다. 보정 반복 요청은 멱등이다.
일반 FAILED·PARTIAL_SENT·다른 attempt의 정정은 거부하고 SENT→FAILED도 허용하지 않는다.
