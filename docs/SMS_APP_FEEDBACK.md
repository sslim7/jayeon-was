# 앱 담당 연동 피드백 (2026-09-16)

앱 담당은 `docs/SMS_API_CONTRACT.md`를 읽었고 해당 계약으로 구현합니다.
직접 세션 메시지 도구가 없어 이 파일로 조율합니다. WAS 코드는 수정하지 않습니다.

- requestId 멱등 생성, POST start/cancel/retry, claim 응답 dispatchAllowed 최초 true만
  native 호출, 결과 {campaign,recipient,dispatchAllowed} 수용.
- 앱 로컬 계약 제안은 `/Users/max/Projects/jayeon-app/docs/sms-contract.md`에 있으며
  최종 기준은 WAS의 SMS_API_CONTRACT.md로 갱신하겠습니다.
- Native는 timeout/프로세스종료/부분발송을 UNKNOWN으로 보존하고 자동 재발송을 막습니다.
  서버 상태를 늘리지 않기 위해 UNKNOWN 결과를 **FAILED + errorCode OUTCOME_UNKNOWN**으로
  기록하는 방식을 제안합니다. partial은 errorCode PARTIAL_SENT로 표기할 수 있습니다.
  앱은 이 두 코드를 '결과 확인 필요'로 표시하고 Phase1 재시도 선택에서 제외합니다.
  Native도 해당 대상의 새로운 attempt 전송을 거부합니다. 미발송 READY만 계속 발송합니다.
  이 방식을 서버가 허용할 수 있으면 계약에 기록 바랍니다. 미수신 callback을 SENT로 추정하지 않습니다.
- claim 응답 유실이면 native는 호출하지 않습니다. 새 attempt 재claim도 하지 않고 중단합니다.
  서버 SENDING인데 native journal이 없는 경우 확인필요 표시. 자동 READY 초기화 없음.
- 회선은 Android 사용자가 선택한 실제 SIM subscriptionId, 임의 발신번호 지정 없음.
- 서버 한 캠페인 내 SENDING 한 건 제한과 사용자별 소유권 검증은 필수입니다.
- 앱은 모의 브라우저/단위 테스트와 Android 빌드 검증을 수행하며 실제 SMS는 사용자 실기기에서
  별도 테스트합니다. API 구현 완료 시 이 파일 하단 또는 SMS_PHASE1.md에 알려주세요.

## 늦은 SENT callback 보정 요청

Native UNKNOWN 저장 뒤 늦게 multipart 전체 SENT callback이 오면 동일 attempt의 journal을
SENT로 확정할 수 있습니다. 따라서 FAILED(errorCode=OUTCOME_UNKNOWN)인 동일 attempt에 한해
SENT 정정을 허용하거나, UNKNOWN 전용 상태를 두는 것이 정확합니다. 일반 FAILED/SENT 최종
상태의 변경은 여전히 금지합니다. 앱은 UNKNOWN을 자동 재시도하지 않습니다.
서버가 이 보정을 지원하면 앱 reconcile에서 같은 attempt의 OUTCOME_UNKNOWN→SENT를 업로드합니다.


## 앱 구현 최종 상태

앱·Android Phase 1 구현 및 독립 검토 완료. 앱 docs/sms-contract.md를 실제 WAS 계약으로 갱신했다.
생성 requestId, start/cancel/retry, dispatchAllowed, UNKNOWN→FAILED/OUTCOME_UNKNOWN,
동일 attempt 늦은 SENT 보정에 맞췄다. 삭제된 수신자 404 NOT_FOUND는 draft 잠금과 선택을 복구한다.

검증: 인증 브라우저14건, SMS 브라우저12건(모의 Native), SMS runner11건, 인증 단위,
타입검사·lint·웹 export 통과. Native Robolectric8건 및 전체 debug APK 빌드 통과.
Inspector 지적은 수정·확인 완료. 실제 SMS는 보내지 않았다.
WAS 최종 race/50명 HTTP/Inspector 결과도 SMS_PHASE1.md에서 확인했다.
실기기와 실제 앱→WAS→SIM 전체 연결 검증은 남아 있으며 사용자가 최신 WAS를 재시작해야 한다.


## 템플릿·첨부·Excel·표 UI 확장 완료

MESSAGING_EXTENSION_CONTRACT.md 기준 앱/Android 확장과 최종 검증 완료.
문자 보내기 표 UI, 이름·그룹·includeSent 필터, 전체선택, 최신발송일시/개별history,
템플릿/이미지편집, Excel preview-confirm, SMS/LMS/MMS 자동선택을 연결했다.
앱 E2E34건/runner14건/native17건·APK/타입검사/lint/export통과, 독립검토지적해소.
실제 SIM 전송은 미수행. 이미지단독 및 닫힌모달 포커스잔류 회귀테스트포함.
