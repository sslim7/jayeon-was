# 메시지 기능 확장 계약

기존 Bearer 인증/사용자 격리/오류 형식을 그대로 사용한다.

- Recipient: 기존 필드 + `latestSentAt:string|null`(SENT만).
- POST/PUT /recipients: 기존 입력 유지. GET /recipients: `includeSent=false`이면 SENT 이력 없는 사람만, true/미지정 전체.
- GET /recipients/{id}/history?limit=50&cursor=...: `{items:[{id,campaignId,campaignTitle,recipientId,name,phone,message,status,sentAt,failedAt,errorCode,errorMessage,createdAt,updatedAt,attachments:Attachment[]}],nextCursor}`. 최신 결과 우선, 시도별 이력.
- Attachment `{id,name,mimeType,size,createdAt}`. JPEG/PNG만, 개별 300KiB 이하, 캠페인/템플릿당 최대 3개, 총 600KiB 이하. 원본 바이트는 별도 문서로 관리하고 snapshot에는 메타데이터만 저장.
- POST /sms/attachments JSON `{name,mimeType,dataBase64}`: 201 Attachment.
- GET /sms/attachments/{id}/content: `{...Attachment,dataBase64}`. GET /sms/attachments/{id}: 메타데이터. DELETE 동일 경로: 204 논리삭제(기존 캠페인/템플릿 참조는 다운로드 가능).
- Template `{id,name,message,attachments:Attachment[],createdAt,updatedAt}`.
- GET /sms/templates?limit=50&cursor=...: `{items,nextCursor}`. POST /sms/templates 및 PUT /sms/templates/{id}: `{name,message,attachmentIds:string[]}`. GET/DELETE /sms/templates/{id}.
- POST /sms/campaigns 기존 입력 + 선택 `attachmentIds:string[]`. Campaign 및 CampaignRecipient 응답에 `attachments:Attachment[]` 추가. 이름/메시지/첨부는 생성 시 snapshot 고정. 첨부가 있으면 빈 메시지도 허용.
- POST /recipients/imports/preview JSON `{name,mimeType,dataBase64}`: xlsx 첫 시트, 필수 헤더 `이름`과 `전화번호` 또는 `연락처`, 선택 헤더 `그룹` (영문 `name,phone,groupId` 가능). 최대 200 데이터행, 2MiB. 전화번호는 텍스트 셀 권장.
- Preview 응답 `{id,addedCount,excludedCount,items:[{row,name,phone,groupId,status:'ADD'|'EXCLUDED',reason:string}],createdAt}`. 제외 원인 한국어. 기존 번호 및 파일내 중복/유효하지 않은 행 제외.
- POST /recipients/imports/{id}/confirm 빈 JSON: 동일 shape, 실제 추가/제외 결과. preview 시 제외 행은 그대로 제외하고 ADD 행만 commit 때 중복 재검증. 같은 import ID 반복 confirm은 최초 결과 반환. 한 transaction 원자적으로 저장.

최종 발송일시와 history는 SENT/FAILED 결과 저장과 동일 transaction에 기록한다. 예전 데이터 history는 과거 캠페인 snapshot에서 조회하는 fallback을 제공한다.

발송결과 PATCH에 선택 `transport: 'SMS'|'LMS'|'MMS'`를 전달하며 CampaignRecipient/history 응답에 저장된다. Native 결정 결과를 기록하고 서버가 메시지 길이로 추측하지 않는다.

## 수신자 목록·Excel 후속 확장

- Recipient에 `customFields:[{name,value}]`(항상 배열), `sentCount:number`(성공 SENT 시도 수)를 추가한다. 동일 결과 재요청은 중복 집계하지 않는다. 과거 캠페인 시도도 집계한다.
- POST/PUT의 `customFields`는 선택이다. 수정 시 생략/null이면 기존값을 보존하고 `[]`이면 삭제한다. 이름 1~100자, 값 1000자 이하, 최대 20개, 전체 UTF-8 JSON 2000bytes 이하. 필드명은 trim 후 중복될 수 없다.
- Excel은 `이름`과 `전화번호` 또는 `연락처`(영문 name/phone 허용) 헤더와 두 값만 필수다. `그룹`(groupId)은 헤더 누락 또는 빈 셀을 허용한다. `전화번호`와 `연락처`를 함께 쓰면 별칭 중복으로 거부한다. 추가 열은 원본 헤더 순서대로 customFields로 보존한다. 빈 헤더/중복 헤더/필수헤더 별칭중복/헤더 없는 데이터 열은 파일 검증 오류다. 추가값 제한초과는 해당 행 제외 사유로 표시한다.
- GET /recipients는 이름(유니코드 문자열)/ID 오름차순이고 `{items,nextCursor,total}`을 반환한다. total은 q/groupId/includeSent 필터를 모두 적용한 전체 수이며 다음 페이지도 같은 필터를 사용한다.
- 템플릿의 attachments는 기존 DB값이 null이거나 누락되어도 HTTP 응답에서 항상 `[]`로 정규화한다.

Excel의 첫 행은 열 제목이며 두 번째 행부터 데이터를 읽는다. 추가 항목을 포함한 사전검증 결과 전체가 750KiB를 초과하면 저장 전에 명확한 400 오류를 반환하며 사용자가 파일을 나누도록 안내한다. 값을 임의로 잘라 저장하지 않는다.

전화번호 신규 입력/Excel은 하이픈 제거 후 `^010[0-9]{8}$`만 허용하며 저장값은 11자리 숫자다. +82 신규 입력도 제외한다. 기존 +8210 DB값은 내부적으로만 국내형으로 읽고 두 형식의 번호 잠금을 확인해 중복 등록을 막는다. 과거 캠페인 원본 스냅샷은 변경하지 않는다.

확정은 저장된 미리보기의 ADD 행에도 현재 입력 규칙(010 번호, 필수 이름, 추가 항목 제한)을 다시 적용한다. 이전 버전에서 허용됐던 +82 번호·이름 누락 등은 제외 사유로 반환하고, 정규화로 발생한 파일 내 중복도 다시 제외한다. 이미 확정된 import 재요청은 최초 확정 결과를 그대로 반환한다.


## 문자보내기 전체 발송이력

`GET /sms/history?q=이름또는번호뒷자리&limit=50&cursor=...` → `{items: RecipientHistory[],nextCursor:string|null,total:number}`. q는 발송 당시 snapshot 의 **이름 부분검색(대소문자 무시) 또는 전화번호 부분검색(뒷자리 포함)** 이며 최대 100자다. 하이픈·공백·괄호·`+82` 표기 차이는 양쪽 모두 걷어 내고 숫자만 비교하고, 글자와 숫자가 섞이면 글자는 이름·숫자는 번호를 가리킨다(`김영 7649`). 규칙은 앱 `src/lib/recipient-search.ts` 및 `GET /calls?q=` 와 같으며 서버는 `internal/recipients/search.go` 한 곳에 둔다. limit은 1~100, 기본 50이다. 기존 수신자별 이력과 동일한 row shape(`campaignTitle`, 항상 배열인 `attachments` 포함)를 반환한다.

순서는 `sentAt`, 없으면 `failedAt`, 둘 다 없으면 `updatedAt`의 내림차순이며 같은 시각에는 고유 이력 ID 내림차순이다. 완료된 SENT/FAILED 시도와 현재 SENDING 시도를 포함하고, 아직 시도하지 않은 READY는 제외한다. 실패 후 재시도 준비로 READY가 되어도 이전 실패 시도는 남는다. 수신자를 수정·삭제해도 과거 스냅샷으로 조회한다. cursor는 사용자와 q에 귀속되므로 검색어 변경 시 초기화한다.

소규모 도구 범위에서 계정 하위 캠페인과 시도 문서를 함께 조회한다. 별도의 전체 사용자 collection-group 조회를 사용하지 않는다.


## 외부 발송 등록

`POST /recipients/{id}/external-sends` 입력 `{requestId,sentAt}`. requestId는 ASCII 영문·숫자·`_`·`-` 8~128자, sentAt은 RFC3339 발송일시다. 미래 시각과 잘못된 시각은 400으로 거부한다. 시간대가 다른 동일 시각은 UTC로 정규화한다.

최초 201, 같은 요청 재전달 200으로 `{recipient,history}`를 반환한다. 동일 사용자 내 requestId에 다른 수신자 또는 일시를 사용하면 409 `IDEMPOTENCY_CONFLICT`다. 결과 재전달은 최초 응답 스냅샷이며 최신 수신자 정보가 필요하면 목록을 다시 조회한다.

성공 시 `sentCount`는 1 증가하고 `latestSentAt`은 기존 시각과 등록 시각 중 나중 값으로 저장한다. 앱 발송과 외부 발송을 함께 집계하며 기존 성공자 제외 필터에도 반영한다. 해당 수신자 소유권 확인·수신자 집계·외부 이력·수신자별 이력을 한 트랜잭션으로 기록한다.

이력은 당시 이름·번호 스냅샷과 `source:"EXTERNAL"`, `status:"SENT"`, 선택한 `sentAt`, `campaignTitle:"외부 발송 등록"`, `message:""`, `attachments:[]`를 가진다. 캠페인 없는 수동 등록이므로 `campaignId`는 생략한다. 기존 앱 이력은 `source:"ANDROID"`로 구분한다. 수신자별/전체 이력 모두 포함하며 실제 Native SMS 발송은 수행하지 않는다.
