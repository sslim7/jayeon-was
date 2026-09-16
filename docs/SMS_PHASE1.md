> 후속 확장: 템플릿·이미지 MMS·Excel 가져오기·수신자별 이력은 [MESSAGING_EXTENSION_CONTRACT.md](MESSAGING_EXTENSION_CONTRACT.md)를 함께 참고하세요. 아래 Phase 1의 첨부/MMS 제외 범위는 후속 구현으로 확장되었습니다.

# SMS Phase 1 구현·검증 기록

## 담당과 범위

- WAS 세션: 수신자 CRUD, 캠페인 snapshot, 발송 확보/결과/중단/복구 API, 문서, 백엔드 검증.
- 앱 세션: 수신자/작성/이력 UI, Expo Native Module, Kotlin SmsManager·PendingIntent·권한, 순차 발송 및 로컬 결과 ledger, Android 실기기 검증.
- 공통 계약: [SMS_API_CONTRACT.md](SMS_API_CONTRACT.md). 앱 피드백은 `SMS_APP_FEEDBACK.md`에 기록한다.
- 실제 SMS·Provider INSERT·SMS 업체 API·예약·MMS·Queue는 WAS에서 구현하지 않는다.

## 구현 전 확인한 기존 구조

### Backend

- Go 1.26.0, Go 표준 net/http ServeMux 메서드 패턴. 추가 웹 프레임워크 없음.
- Cloud Firestore SDK v1.25.0, ORM/SQL/migration runner 없음. 신규 사용자 하위 컬렉션만 추가하므로 기존 데이터 변환 없음.
- `auth.TokenIssuer.Middleware`가 Bearer access JWT를 검증해 `auth.UserID(ctx)` 주입. 실제 접근 거부는 각 handler가 담당.
- `httpx.WriteJSON`, `WriteError`, `DecodeJSON` 및 `{code,message,details?}` 재사용. JSON camelCase, 한국어 오류.
- 기존 users/admin은 Register + Handler + Store 구조. 별도 대형 service/repository framework를 추가하지 않는다.
- 기존 `users.NewStore` 계정 검증을 신규 `userguard`에서 재사용하고 신규 도메인에만 적용한다.
- 기존 인증·어드민 API는 변경하지 않는다.

### Frontend (참조만, 이 세션은 수정하지 않음)

- Expo ~57.0.9, React Native 0.86.2, React19.2.3, TypeScript~6.0.3.
- Expo Router ~57.0.9 및 typedRoutes. Zustand5 상태관리.
- `src/lib/api.ts`의 기존 API 클라이언트와 access/refresh 인증, SecureStore/native bridge 사용.
- `src/config/env.ts`에서 EXPO_PUBLIC_API_URL 등 환경변수를 통합하고 개발기기 host 보정.
- 기존 로그인 폼/UI 및 WebShell이 존재. 모바일 인증 이후 web shell을 사용하는 구조이므로 앱 담당이 native SMS 호출 경계를 명확히 유지해야 한다.
- Android package `kr.redhead.nature`. 분석 시점 checked-out `android/`, `modules/` 없음. SEND_SMS 미설정.
- Expo Go만으로 SMS 발송 검증 불가. 앱 담당이 native build 및 실제 SIM 단말 검증 수행.

## 데이터 설계

- `users/{uid}/recipients/{id}`: 현재 수신자. 번호 유일성 락을 같은 사용자 범위에서 트랜잭션으로 관리.
- `users/{uid}/smsCampaigns/{id}`: 캠페인 및 최대50건의 수신자 snapshot/진행상태를 함께 저장.
- Firestore 단일 캠페인 트랜잭션으로 대상 상태와 집계가 어긋나지 않게 한다.
- 모든 접근은 인증 사용자 경로만 사용. 다른 사용자 id로 조회·수정해도 데이터가 노출되지 않는다.
- 시도별 상세 결과는 캠페인 하위 `attempts` 컬렉션에 같은 트랜잭션으로 저장한다. 캠페인 문서에는 최대20회 시도의 식별자·상태·시간만 유지해, 장문 snapshot과 오류 상세 이력이 Firestore 1MiB 한도를 넘지 않게 한다.
- message2000문자·50명·현재 오류길이 상한을 검증한다.
- 목록 정렬은 기존 단일 필드/문서ID 인덱스를 활용하며 별도 복합 인덱스나 인프라 적용 없이 실행한다.

## 안전성 경계

- 서버 claim과 단말 SMS 발송은 분산된 두 작업이다. 정확히 1회 발송을 서버만으로 보장한다고 주장하지 않는다.
- claim 응답 유실/앱 종료 시 서버 SENDING 유지. 앱은 native ledger/callback 증거로 결과만 동기화한다.
- SENDING 자동 만료나 READY 복귀 없음. SENT 자동 재발송 없음. FAILED 재시도는 명시적 별도 API.
- 앱과 합의: OUTCOME_UNKNOWN/PARTIAL_SENT는 FAILED로 동기화하되 ‘확인 필요’로 표시하고 Phase1 서버/앱/Native 모두 재시도 금지.
- 중단은 이후 발송 확보를 막는다. 이미 단말로 넘긴 SMS는 취소하지 못하므로 늦은 결과 저장을 허용한다.
- Native는 모든 multipart SENT 결과를 모아야 한다. SENT는 수신/읽음 상태가 아니다.
- 앱이 Provider에 직접 INSERT하지 않는 것과 OS 자동 저장은 다르다. [Android 공식 문서](https://developer.android.com/reference/android/telephony/SmsManager)에 비기본 SMS 앱의 발송을 시스템이 자동 기록할 수 있다고 명시되어 있다.

## 작업 상태

- [x] 기존 WAS/앱 구조 분석
- [x] 공통 REST 계약/중복방지 규칙 작성
- [x] 수신자 CRUD/검색/번호 정규화
- [x] 캠페인 snapshot/멱등 생성/상태 전이
- [x] 사용자 인증 가드 및 main 배선
- [x] 단위 테스트 및 Firestore 동시성·격리 통합 테스트
- [x] OpenAPI/README 갱신
- [x] Inspector 최종 독립 검증
- [x] 앱 담당의 REST 계약 수용 및 UNKNOWN/늦은 SENT 보정 협의
- [ ] 앱과 실제 WAS를 연결한 화면 동작 확인 (앱 세션 담당)
- [ ] Android 실제 SIM·권한·multipart·중단/복구 검증 (앱 세션/사용자 담당)

## 검증 명령

```bash
go build ./...
go vet ./...
go test ./...
firebase emulators:exec --only firestore --project demo-test \
  --config firebase.test.json 'go test -race ./... -count=1'
```

일반 go test에서는 FIRESTORE_EMULATOR_HOST가 없는 통합 테스트가 skip된다.
2026-09-16 최종 결과:

| 검사 | 결과 |
| --- | --- |
| `go build ./...` | 통과 |
| `go vet ./...` | 통과 |
| `FIRESTORE_EMULATOR_HOST=127.0.0.1:8095 go test -race ./... -count=1 -timeout=180s` | 기존 인증·어드민 포함 전체 통과 |
| 실제 JWT·사용자 가드를 연결한 50명 HTTP 처리 | 중단·재개·실패 재시도 후50명 완료 통과 |
| 12개 동시 동일 claim | 최초 발송 허가 정확히1개, 반복 검증 통과 |
| 최대50명·2000 Unicode 문자·20시도 및 오류 이력 | Firestore 저장/조회 통과 |
| 전화번호 중복·수정/삭제·사용자 격리·snapshot | 통과 |
| 늦은 UNKNOWN의 SENT 보정·일반 최종 상태 불변 | 통과 |
| 동일 시각의 상태 전이·멱등 재요청 | Revision으로 실전이와 무변경을 구분, 통과 |
| 목록 cursor·동일 생성 시각 페이지 분할 | 통과 |
| OpenAPI YAML·참조·operationId | 파싱 및 정합성 검사 통과 |
| `gofmt -l`, `git diff --check` | 이상 없음 |
| Inspector 독립 검증 | 발견 사항 수정·재검증, 미해결 결함 없음 |

Inspector 발견 사항 중 전화번호 정규화 비멱등 입력, Firestore 단일 문서 이력 크기,
OpenAPI 중복 YAML anchor를 수정했다. 동시 요청 재검증에서 확인한 불필요한 멱등 쓰기도 제거했다.
실제 SMS 발송은 수행하지 않았다. 앱 화면과 실제 SIM 단말은 위 미완료 항목의 담당자가 검증한다.
커밋·푸시·배포는 실행하지 않았다.

## 앱 담당에게 전달하는 현재 상태

2026-09-16: REST API 전체 및 `main.go` 배선 구현 완료. 앱 계약의 UNKNOWN/PARTIAL_SENT
재시도 금지와 동일 attempt UNKNOWN의 늦은 SENT 보정을 반영했다. 실제 Go 라우트/JWT/Firestore를
연결한 50명 흐름(4건 후 중단·재개, 49성공1실패, 명시 재시도 후50성공) 테스트가 통과했다.
사용자별 소유권·동시 claim·snapshot도 검증했다. 전체 패키지 최종 race 및 Inspector 검증을 통과했다.
실행 중인 WAS가 이전 바이너리이면 재시작해야 신규 API가 등록된다. 이 세션은 개발 WAS를 재시작하지 않았다.
