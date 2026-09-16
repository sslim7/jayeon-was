# nature-was

Nature 서비스의 백엔드(WAS)다. 외부에 공개하는 상용 서비스가 아니라 내부에서 쓰는 서비스다.

**Go 1.26 · Google Cloud Firestore (SQL 아님) · 표준 `net/http` · Cloud Run**

웹 프레임워크도 ORM 도 두지 않는다. 라우팅은 Go 1.22+ `http.ServeMux` 의 메서드+경로 패턴을
그대로 쓴다. 개발 단계에서는 GCP 대신 **로컬 Firestore 에뮬레이터**를 쓴다.

- 목표 공개 API 도메인: `https://nature-api.redhead.kr` (DNS/Hosting 전환은 별도 수동 적용)
- 운영 인프라(GCP 프로젝트 `redhead`, DNS 포함)는 별도 레포 `redhead-terraform` 에서 관리한다.

> **Android SIM SMS 도구의 백엔드다.** 초대 전용 인증, 사용자별 수신자 관리, 캠페인 snapshot,
> 발송 상태·이력·중단/복구 API를 제공한다. 실제 SMS는 Android 앱이 발송하며 서버는 결과만 저장한다.
> 앱 연동은 [SMS API 계약](docs/SMS_API_CONTRACT.md), 검증 및 담당 범위는
> [SMS Phase 1 기록](docs/SMS_PHASE1.md)을 참고한다.

## 디렉토리 구조

```
.
├── main.go              엔트리포인트. 환경변수 검사 → 라우터 조립 → CORS → graceful shutdown
├── internal/            도메인 패키지
│   ├── admin            어드민 로그인·계정·활동로그, 어드민 라우트 가드
│   ├── auth             사용자 로그인·비밀번호 변경·JWT 발급 및 검증
│   ├── credentials      사용자와 어드민이 공유하는 비밀번호 해시·이메일 정규화
│   ├── users            Firestore 사용자 저장소와 내 프로필
│   ├── userguard        활성 계정·최초 비밀번호 변경 여부를 확인하는 도메인 가드
│   ├── recipients       사용자별 수신자 CRUD·검색·전화번호 정규화
│   ├── sms              캠페인 snapshot·발송 확보·결과·중단/재개
│   ├── bg               응답을 보낸 뒤 도는 백그라운드 고루틴의 panic 안전장치
│   └── httpx            openapi.yaml 공통 에러 모델에 맞춘 JSON 응답 헬퍼
├── cmd/create-admin/    첫 어드민 계정을 만드는 1회성 잡
├── cmd/create-user/     초대 사용자 계정 생성 CLI
├── docs/openapi.yaml    OpenAPI 스펙 (main.go 가 //go:embed 로 바이너리에 넣는다)
├── Dockerfile           Cloud Run 이미지 (2단계 빌드)
├── cloudbuild.yaml      build → push → gcloud run deploy
├── firebase.json        로컬 개발용 에뮬레이터 설정 (firestore 8090 / ui 4090)
├── firebase.test.json   테스트 전용 에뮬레이터 설정 (firestore 8095, ui 없음)
└── .env.example         환경변수 템플릿 (정본)
```

## 로컬 개발 시작하기

**처음에는 환경 설정 → 로컬 DB → WAS → 로그인 계정 생성 순서로 진행한다.**
설정이 끝난 다음부터는 DB 에뮬레이터를 켜고 별도 터미널에서 `go run .` 하면 된다.
WAS 실행 명령이 DB까지 띄워 주지는 않는다.

사전 요구사항: Go 1.26, Node.js, JDK 21+, Firebase CLI다.
설치 여부는 `go version`, `node --version`, `java -version`, `firebase --version`으로 확인한다.
Firebase CLI가 없으면 `npm install -g firebase-tools`로 설치한다.

### 1. 최초 한 번: 환경변수 파일 설정

```bash
cd /Users/max/Projects/jayeon-was
cp .env.example .env
openssl rand -base64 32
openssl rand -base64 32
```

`.env`가 이미 있으면 복사하지 않고 기존 값을 확인한다.
위에서 출력된 두 값을 `.env`의 `JWT_SECRET`, `ADMIN_JWT_SECRET`에 각각 넣는다.
**예시의 자리표시자를 실제 값으로 교체하고, 두 시크릿은 서로 다르게 설정한다.**
`.env`는 커밋하지 않는다.

나머지 로컬 기본값은 `.env.example`에 들어 있다.

```dotenv
GOOGLE_CLOUD_PROJECT=demo-jayeon
FIRESTORE_EMULATOR_HOST=localhost:8090
PORT=8091
```

`go run .`은 리포 루트의 `.env`를 자동으로 읽는다. 다만 터미널에 이미 export된 환경변수가
있으면 그 값이 우선하므로 다른 프로젝트 설정이 남아 있지 않은지 확인한다.

### 2. 터미널 A: Firestore 에뮬레이터 실행

처음 실행할 때는 아직 저장된 데이터가 없으므로 다음 명령을 쓴다.

```bash
cd /Users/max/Projects/jayeon-was
firebase emulators:start --only firestore --project demo-jayeon \
  --export-on-exit=./emulator-data
```

- 종료할 때 **Ctrl+C**를 누르고 저장 완료까지 기다린다. 데이터는 `emulator-data/`에 저장된다.
- 다음 실행부터는 아래 [재실행 방법](#종료와-다음-실행)의 `--import` 옵션으로 복원한다.
- 강제 종료하면 마지막 저장 이후 변경분은 사라질 수 있다. `emulator-data/`는 커밋하지 않는다.
- `demo-jayeon`은 로컬 테스트용 프로젝트 ID다. 실제 GCP 프로젝트나 Firebase 로그인이
  필요 없으며 `.env`의 `GOOGLE_CLOUD_PROJECT`와 같아야 한다.

| 주소 | 역할 |
| --- | --- |
| `localhost:8090` | Firestore 에뮬레이터. WAS가 접속하는 로컬 DB |
| [localhost:4090](http://localhost:4090) | Emulator UI. 저장된 데이터 확인 |

### 3. 터미널 B: WAS 실행

에뮬레이터를 켜 둔 채 **새 터미널**에서 실행한다.

```bash
cd /Users/max/Projects/jayeon-was
go run .
```

`listening on :8091` 로그가 나오면 서버가 실행된 것이다.
포트는 `.env`의 `PORT`를 따른다. 미설정 시 기본값은 `8080`이므로 로컬은 `8091`로 설정한다.

브라우저에서 [헬스체크](http://localhost:8091/health)를 열어 `ok`가 나오는지 확인한다.
터미널에서는 다음 명령으로 확인할 수 있다.

```bash
curl http://localhost:8091/health
```

[Swagger UI](http://localhost:8091/docs)에서 API를 확인한다. 스펙 원문은 `/openapi.yaml`이다.
Swagger UI 에셋은 CDN에서 가져오므로 네트워크가 필요하다.

### 4. 터미널 C: 최초 로그인 계정 생성

앱은 초대 전용이므로 가입 화면이 없다. DB와 WAS를 켜 둔 채 **다른 터미널**에서 계정을 만든다.
이메일·이름·임시 비밀번호를 원하는 값으로 바꾼다.

```bash
cd /Users/max/Projects/jayeon-was
GOOGLE_CLOUD_PROJECT=demo-jayeon FIRESTORE_EMULATOR_HOST=localhost:8090 \
  go run ./cmd/create-user \
  --email you@example.com --name '사용자' \
  --password '<8자 이상 임시 비밀번호>' --write
```

- **계정 생성 CLI는 `.env`를 읽지 않는다.** 위처럼 환경변수를 명령 앞에 명시한다.
- `--write`가 있어야 실제로 생성한다. 빼면 DB에 연결하지 않는 dry-run이다.
- 같은 이메일로 다시 만들면 중복 오류가 난다. DB 데이터를 복원했다면 계정을 다시 만들 필요 없다.
- 생성한 이메일·임시 비밀번호로 앱에 로그인하면 비밀번호 변경 화면으로 이동한다.
  변경 후에는 새 비밀번호로 다시 로그인한다.
- 사용자 계정과 어드민 계정은 별개다. 앱 로그인에는 `create-user`로 만든 계정을 쓴다.

인증 API와 비밀번호 정책은 아래 [사용자 로그인](#사용자-로그인)에 정리되어 있다.

### 종료와 다음 실행

종료할 때는 WAS 터미널에서 `Ctrl+C`, 이어서 에뮬레이터 터미널에서 `Ctrl+C`를 누른다.
에뮬레이터의 데이터 저장과 종료가 끝날 때까지 기다린다.

다음부터는 환경 파일이나 계정을 다시 만들지 않고, 터미널 두 개에서 실행한다.

**터미널 A — 저장된 DB 복원:**

```bash
cd /Users/max/Projects/jayeon-was
firebase emulators:start --only firestore --project demo-jayeon \
  --import=./emulator-data --export-on-exit=./emulator-data
```

**터미널 B — WAS:**

```bash
cd /Users/max/Projects/jayeon-was
go run .
```

Go 코드나 `.env`를 변경한 경우 WAS를 `Ctrl+C`로 종료한 뒤 `go run .`으로 다시 실행한다.
`go run .`에는 자동 재시작 기능이 없다. DB 에뮬레이터는 그대로 켜 두어도 된다.

### 실행이 안 될 때

| 증상 | 확인할 것 |
| --- | --- |
| `GOOGLE_CLOUD_PROJECT` 또는 `JWT_SECRET` 미설정 오류 | 리포 루트에 `.env`가 있는지, 값을 채웠는지 확인 |
| 사용자·어드민 시크릿이 같다는 오류 | `.env`의 두 시크릿을 서로 다른 값으로 설정 |
| `address already in use` / 8091 포트 충돌 | 이전 WAS 터미널에서 `Ctrl+C`로 종료한 뒤 재실행 |
| `/health`가 503 | 에뮬레이터 실행 여부와 `FIRESTORE_EMULATOR_HOST=localhost:8090` 확인 |
| 계정 생성 명령이 환경변수 오류 | CLI는 `.env`를 읽지 않으므로 명령 앞의 환경변수 지정 확인 |
| 재시작 후 계정이 사라짐 | 에뮬레이터 정상 종료·저장 여부와 재실행 시 `--import` 지정 확인 |
| 앱에서 로그인 API가 404 | 변경 전 WAS가 떠 있을 수 있으므로 현재 코드로 WAS 재시작 |

포트를 사용 중인 프로세스는 아래 명령으로 확인한다. PID를 확인하지 않고 다른 프로젝트의
프로세스를 종료하지 않도록 한다.

```bash
lsof -nP -iTCP:8091 -sTCP:LISTEN
lsof -nP -iTCP:8090 -sTCP:LISTEN
```

## 로컬 포트

형제 프로젝트 birdieup 과 **같은 모양으로 한 칸 옆**에 잡았다. 새 포트를 잡을 때는 이 표에
먼저 적는다.

| 프로젝트 | admin | app(dev) | Firestore 에뮬 | 에뮬 UI | WAS | 테스트 에뮬 |
| --- | --- | --- | --- | --- | --- | --- |
| birdieup | 3000 | 3003 | 8080 | 4000 | 8081 | 8085 |
| **nature** | 3100 | **3103** | **8090** | **4090** | **8091** | **8095** |

(nature 의 admin 3100 은 어드민 SPA 가 생길 때를 위해 비워 둔 자리다.)

🔴 **birdieup 과 겹치면 안 된다.** 겹쳐서 「주소가 이미 사용 중」으로 죽으면 차라리 낫다 —
즉시 보이니까. 진짜 문제는 죽지 않는 경우다: 앱이 자기 API 대신 birdieup-was 를 부르고,
거기서 온 404 나 엉뚱한 응답을 받고도 원인이 자기 코드에 있다고 생각하며 한참을 헤맨다.
인증이 걸리면 더 고약해서, 다른 프로젝트의 토큰 규칙에 막힌 것을 자기 로그인 버그로
착각하게 된다.


## 빌드 · 검사 · 테스트

```bash
go build ./...   # 컴파일 확인 (파일을 남기지 않는다)
go vet ./...     # 정적 검사
go test ./...    # 단위 테스트
```

`go build .` (패키지 하나) 은 리포 루트에 모듈명과 같은 **`nature-was` 바이너리**를 남긴다.
`.gitignore` 와 `.dockerignore` 가 둘 다 제외하므로 커밋되지도, 이미지 빌드 컨텍스트에
들어가지도 않는다.

### 테스트

단위 테스트만 돌리려면 그냥 `go test ./...` 다. 에뮬레이터가 없으면 에뮬레이터가 필요한
테스트는 skip 하고 나머지는 전부 통과한다.

에뮬레이터가 필요한 통합 테스트까지 돌리려면 **일회용 에뮬레이터**를 띄운다.

```bash
firebase emulators:exec --only firestore --project demo-test \
  --config firebase.test.json 'go test ./... -count=1'
```

- `emulators:exec` 가 `FIRESTORE_EMULATOR_HOST` 를 자동으로 주입한다.
- `--import` / `--export-on-exit` 를 주지 않으므로 **`emulator-data/` 를 건드리지 않는다.**
  매 실행이 빈 데이터로 시작한다.
- firebase-tools 15 는 **JDK 21 이상**을 요구한다. `java -version` 이 그보다 낮으면
  `JAVA_HOME` 을 21+ JDK 로 지정하고 돌린다.

#### 설정 파일을 둘로 나눈 이유

`firebase.test.json` 은 Firestore 포트가 **8095** 라 개발용 에뮬레이터(`firebase.json`, 8090)와
같이 떠 있어도 충돌하지 않는다. Emulator UI 도 꺼져 있다.
같은 포트를 쓰면 테스트가 개발 중인 로컬 데이터를 건드리거나 포트 충돌로 실패한다.
**개발용 에뮬레이터를 켜 둔 채 테스트를 돌릴 수 있게 하려고 나눈 것이다.**

## 환경변수

전체 목록과 "왜" 는 **`.env.example` 이 정본이다.** 여기에는 기동을 막는 것과
조용히 기능만 끄는 것의 구분만 적는다.

| 변수 | 없으면 |
| --- | --- |
| `GOOGLE_CLOUD_PROJECT` | **기동 실패** |
| `JWT_SECRET` | **기동 실패** — 없으면 토큰을 위조할 수 있다 |
| `ADMIN_JWT_SECRET` | 경고 1회. **어드민 API 를 아예 등록하지 않는다** — `/auth/admin.login` 과 `/admin/*` 이 전부 404. 사용자 앱은 그대로 돈다 |
| `CORS_ALLOWED_ORIGINS` | CORS 헤더를 아예 붙이지 않는다 (운영 기본값) |
| `FIRESTORE_EMULATOR_HOST` | 에뮬레이터 대신 ADC 로 실제 Firestore 에 붙는다 (운영 동작) |
| `PORT` | `8080` (Cloud Run 규약) |

🔴 `ADMIN_JWT_SECRET` 이 `JWT_SECRET` 과 **같은 값이면 기동 실패다.** 없는 것과 다르다.

## 헬스체크

`GET /healthz` 와 `GET /health` 두 경로가 **같은 핸들러**를 쓴다. Firestore 에 가벼운 왕복을
한 번 돌려 연결이 살아있으면 `200 ok`, 아니면 `503` 을 준다.

```bash
curl localhost:8091/health
```

에뮬레이터가 떠 있지 않으면 `503` 이 정상이다.

- `/healthz` 는 Cloud Run 의 **startup probe 용**이다. 프로브는 컨테이너로 직접 오므로
  정상 동작한다.
- `/health` 는 **외부용 별칭**이다. Google Frontend 가 정확히 `/healthz` 를 가로채 자체
  404(HTML)를 돌려주기 때문에 run.app 주소로도 `/healthz` 는 컨테이너까지 오지 않는다.
  **외부 업타임 감시와 배포 후 스모크 체크는 `/health` 를 쓴다.**

### 공개 주소 앞에는 프록시가 한 겹 있다

```
nature-api.redhead.kr  ──CNAME──▶  redhead-jayeon-api.web.app  ──rewrite──▶  Cloud Run jayeon-was
                                        (Firebase Hosting)
```

Cloud Run 도메인 매핑이 `asia-northeast3` 에서 지원되지 않아 Firebase Hosting 을 rewrite
프록시로 쓴다. External HTTPS LB 는 트래픽이 0 이어도 월 약 $18 이 나간다.

🔴 그래서 **모든 요청에 `Via` 헤더가 붙고, 클라이언트 IP 는 `X-Forwarded-For` 로 들어온다.
`RemoteAddr` 를 그대로 믿으면 안 된다** — 거기 찍히는 것은 프록시 주소다.
위의 `/healthz` 가 컨테이너까지 오지 않는 것과 같은 뿌리(앞단이 한 겹 있다)의 이야기다.

## 사용자 로그인

초대 전용 이메일·비밀번호 방식이다. 공개 가입과 이메일 비밀번호 찾기는 제공하지 않는다.
에뮬레이터를 띄운 뒤 아래 CLI로 계정을 만든다. 이 CLI는 `.env`를 읽지 않으므로
대상 환경변수를 명시한다. `--write`를 빼면 DB에 연결하지 않는 dry-run이다.

```bash
GOOGLE_CLOUD_PROJECT=demo-jayeon FIRESTORE_EMULATOR_HOST=localhost:8090 \
  go run ./cmd/create-user --email invitee@example.com --name '초대 사용자' \
  --password '<8자 이상 임시 비밀번호>' --write
```

운영 쓰기에는 `--write --yes`가 모두 필요하다. 임시 비밀번호는 CLI 인자에 들어가므로
셸 기록 관리에 유의한다. CLI 출력에는 비밀번호를 표시하지 않는다.

생성된 계정은 `mustChangePassword: true`로 시작한다. 임시 비밀번호를 초대받은 사람에게
전달하면 앱(`nature-app`, 로컬 3103)에서 로그인하고 비밀번호를 변경한다.
앱의 API 주소는 `http://localhost:8091`이며, 실기기는 개발 PC의 사설 IP를 쓴다.

| 경로 | 동작 |
| --- | --- |
| `POST /auth/login` | 이메일·비밀번호 확인 후 토큰과 `mustChangePassword` 반환 |
| `POST /auth/refresh` | 계정 상태·토큰 버전 대조 후 토큰 재발급 |
| `POST /auth/change-password` | 현재 비밀번호 확인 후 변경, 성공 시 204 |
| `GET /users/me` | 내 프로필과 `mustChangePassword` 반환 |

상세 요청·응답은 `docs/openapi.yaml`과 로컬 `/docs`에서 확인한다.
새 비밀번호는 8자 이상, UTF-8 기준 72바이트 이하여야 하며 현재 비밀번호와 달라야 한다.
비밀번호 변경은 저장된 해시·토큰 버전·변경 필요 플래그를 함께 갱신한다.
**변경 후 기존 리프레시 토큰은 무효이며 다시 로그인해야 한다.**
이미 발급된 액세스 토큰은 최대 1시간 더 유효하다. 로그아웃은 해당 기기의 토큰을 삭제한다.

로그인 실패는 계정 존재 여부를 구분하지 않는 `INVALID_CREDENTIALS`다.
`TOO_MANY_ATTEMPTS`(429)는 향후 시도 제한을 위한 예약 코드이며, 이번에는 제한을 구현하지 않았다.

## SMS Phase 1 API

로컬 실행·계정 생성은 위 순서와 같다. 앱에서 임시 비밀번호를 변경하고 다시 로그인한 뒤
수신자를 등록한다. SMS 도메인은 비활성 계정과 최초 비밀번호 미변경 계정의 접근을 차단한다.

| 기능 | API |
| --- | --- |
| 수신자 등록·검색 | `POST /recipients`, `GET /recipients?q=...&groupId=...` |
| 수신자 수정·삭제 | `PUT /recipients/{id}`, `DELETE /recipients/{id}` |
| 캠페인 생성·이력 | `POST /sms/campaigns`, `GET /sms/campaigns` |
| 상세·발송 대상 | `GET /sms/campaigns/{id}`, `GET /sms/campaigns/{id}/recipients` |
| 시작·미발송 이어 보내기 | `POST /sms/campaigns/{id}/start` |
| 발송 중지 | `POST /sms/campaigns/{id}/cancel` |
| 한 건 발송 확보·결과 보고 | `PATCH /sms/campaigns/{id}/recipients/{recipientId}` |
| 명시적 실패 재시도 준비 | `POST /sms/campaigns/{id}/recipients/{recipientId}/retry` |

- `010-1234-5678`, `01012345678`은 `01012345678`로 저장한다. 신규 입력은 010으로 시작하는 11자리만 허용하고 +82 입력은 거부한다. 기존 +8210 저장값의 중복 잠금은 호환한다.
  사용자별 중복 번호를 거부하고, 이름·번호 검색과 자유 문자열 `groupId` 필터를 지원한다.
- 캠페인은 1~50명, 본문은 최대2000 Unicode 문자다. 실제 SMS 분할 수는 단말의
  `SmsManager.divideMessage()` 결과로 결정하며 서버의 문자 수를 통신요금/분할 수로 보지 않는다.
- 캠페인 생성 시 이름·번호·본문을 snapshot으로 저장하므로 수신자를 나중에 수정/삭제해도 이력은 유지된다.
- `requestId`는 캠페인 생성 재요청용, `attemptId`는 한 건 발송 시도용이다.
  네트워크 재시도에서 새 식별자를 만들면 안 된다.
- 실제 SMS를 보내기 전에 서버에 `SENDING`을 요청한다. 최초 응답의 `dispatchAllowed:true`일 때만
  Native를 호출한다. 같은 요청을 반복해서 받은 `false`는 재발송 허가가 아니다.
- 캠페인별 `SENDING`은 한 건만 허용하며 결과 저장에 성공한 뒤 다음 건을 확보한다.
- 앱이 종료되어도 서버의 `SENDING`을 자동으로 대기로 돌리지 않는다. 앱이 Native의 영속 결과를
  복구해 서버에 동기화하고, 사용자가 명시적으로 이어 보내기를 눌러야 한다.
- `OUTCOME_UNKNOWN`/`PARTIAL_SENT` 실패는 결과 확인 필요이며 Phase1 재시도를 금지한다.
  동일 시도의 결과 불명 상태에 늦은 전체 성공 callback이 도착한 경우에만 SENT로 정정한다.
- `SENT`는 단말 발송 요청 성공이며 상대방의 수신/읽음 확인이 아니다.
- WAS는 전화번호·본문을 application log에 남기지 않는다. 원문은 인증된 사용자 DB에 저장한다.

요청·응답·오류·중단 시점의 상세 규칙은 [SMS_API_CONTRACT.md](docs/SMS_API_CONTRACT.md)와
[Swagger UI](http://localhost:8091/docs)를 확인한다. 새 컬렉션은 자동 생성되며 SQL migration이나
Terraform 적용은 필요하지 않다.

### SMS 검증 범위

Firestore 테스트는 실제 문자를 보내지 않고 Android가 보고할 상태를 모의 입력해 서버 처리를 검증한다.
동시 claim의 발송 허가가 하나인지, 결과 저장·중단·재개·이력·사용자 격리가 유지되는지 확인한다.

```bash
firebase emulators:exec --only firestore --project demo-test \
  --config firebase.test.json 'go test -race ./... -count=1'
```

실제 SIM, SEND_SMS 권한, multipart callback, 앱 강제 종료 후 복구, 통신사 제한 및 기본 메시지 앱
표시는 **앱 담당과 사용자가 Android 실기기에서 별도로 확인**해야 한다. 앱은 SMS Provider에 직접
INSERT하지 않지만 Android OS가 발송 메시지를 자동으로 저장할 수 있다.

## 어드민

첫 어드민 계정은 아래로 만든다. (에뮬레이터 또는 대상 Firestore 가 떠 있어야 한다)

```bash
go run ./cmd/create-admin
```

`ADMIN_JWT_SECRET` 이 비어 있으면 어드민 라우트가 **통째로 404** 다
(`/auth/admin.login`, `/admin/*`). 기동은 그대로 되고 로그에 경고만 남으므로,
"로그인 화면이 404 를 준다" 면 먼저 이 변수를 확인한다.

## 브랜치와 배포

기본 브랜치는 **`staging`** 이다. 브랜치를 푸시한다고 배포되지 않는다.

```
staging 에서 작업  →  release 에 머지  →  v* 태그 푸시  →  배포
```

`v1.2.3` 같은 태그를 푸시하면 GCP **Cloud Build 트리거**(패턴 `^v.*$`)가 `cloudbuild.yaml` 을
돌려 Cloud Run 에 배포한다. `build` → `push` → `gcloud run deploy` 세 단계이고,
롤백 대상을 지목할 수 있도록 `$COMMIT_SHA` 태그와 `latest` 태그를 함께 민다
(`latest` 만 있으면 "어느 이미지로 되돌릴지" 를 지목할 수 없다).

- **GitHub Actions 는 쓰지 않는다.**
- **트리거 정의는 이 레포가 아니라 GCP 쪽에 있다.** 레포를 뒤져도 나오지 않으니
  트리거를 바꾸려면 GCP 콘솔(또는 `redhead-terraform`)을 본다.
- 배포 대상: Cloud Run 서비스 `jayeon-was`, 리전 `asia-northeast3`,
  이미지 `asia-northeast3-docker.pkg.dev/redhead-kr/redhead/jayeon-was`.
- 수동으로 돌릴 때는 `gcloud builds submit --config cloudbuild.yaml`.
- 배포가 끝나면 `/health` 로 스모크 체크한다(`/healthz` 는 밖에서 쓸 수 없다).

## 인프라 경계

> 🔴 **Cloud Run 서비스 설정(환경변수·시크릿·CPU·min-instances·`--allow-unauthenticated` 등)의
> 소유자는 `redhead-terraform` 이다. CI 는 이미지만 교체한다.**

`cloudbuild.yaml` 의 deploy 단계에 서비스 설정 플래그를 넣지 마라. CI 가 같이 넘기면 배포할
때마다 Terraform 이 만든 값을 CI 가 덮고, 다음 apply 에서 Terraform 이 되돌리는 핑퐁이 생긴다.
실제 증상은 "배포 후 환경변수가 사라져 기동 실패" 로 나타난다.
**설정을 바꿔야 하면 Terraform 쪽을 고쳐서 apply 한다.**

## Phase 1에서 제외한 기능

예약 발송, MMS, 주소록 자동 동기화, 외부 SMS 업체 API, 복잡한 메시지 Queue는 구현하지 않는다.
iOS 자동 일괄 발송도 지원하지 않는다. Android 실제 발송은 별도 `nature-app` 저장소가 담당한다.

인증의 공개 가입, 이메일 비밀번호 찾기, 사용자 프로필 수정, 로그인 시도 제한은 아직 없다.

### 수신자·템플릿·이미지 메시지 확장

[API 계약](docs/MESSAGING_EXTENSION_CONTRACT.md)에 업로드와 Excel 형식을 정리했습니다.
- 수신자 목록 `includeSent=false`는 과거 SENT가 있는 사람을 제외합니다. 기본값은 전체 조회이며 수신자 관리 화면에 사용합니다. `latestSentAt`은 SENT 성공 시각이고 수신자별 `/history`는 시도별 스냅샷입니다.
- 템플릿 CRUD, JPEG/PNG 첨부 업로드·내용 조회·논리삭제. 이미지 개별 300KiB, 최대 3개/합계 600KiB. 템플릿·캠페인에는 첨부 메타데이터만 복사하고 실제 이미지는 사용자 하위 별도 Firestore 문서에 보관합니다. Android는 SMS/LMS/MMS를 전송하며 서버는 실제 결과의 `transport`를 저장합니다.
- `.xlsx` 가져오기는 첫 시트 첫 행의 `이름, 전화번호, 그룹` 필수 헤더(영문 name/phone/groupId도 가능), 최대 2MiB/200행입니다. 두 번째 행부터 자료를 읽고 세 필수값 중 누락된 행은 제외합니다. 추가 열은 헤더 순서와 셀 문자열을 customFields로 보존합니다(최대 20개, 행별 JSON 2000bytes, 전체 미리보기 750KiB 제한은 초과 시 명시적으로 거절). 전화번호 셀을 텍스트로 작성해 맨 앞 0을 보존하세요. 사전검증 후 24시간 내 확정하고, 기존 번호·파일내 중복·잘못된 행을 제외합니다. 확정 시 번호 중복을 다시 검사하며 같은 import ID 재요청은 중복 삽입하지 않습니다.
- 기존 캠페인도 발송일시·이력에 표시하도록 레거시 스냅샷을 함께 조회합니다. 소규모 운영을 위한 방식이며 데이터가 크게 늘면 별도 백필 후 집계 필드만 조회하도록 전환할 수 있습니다.

검증: `firebase emulators:exec --only firestore --project demo-jayeon --config firebase.test.json 'go test -race -p 1 ./...'`. `-p 1`은 에뮬레이터의 동시 트랜잭션 테스트 간 자원 경합을 줄입니다. 실제 SIM 발송은 자동 테스트하지 않습니다.

수신자 목록은 이름/ID 순서로 페이지를 나누고 필터 전체 `total`, 성공 발송 횟수 `sentCount`를 반환합니다. 템플릿 첨부는 레거시 누락값을 포함해 언제나 JSON 배열입니다.

문자보내기 헤더의 전체 발송이력은 `GET /sms/history`를 사용합니다. `q`로 당시 수신자 이름을 검색하고 발송 결과 시각의 최신순으로 페이지를 조회합니다. READY는 제외하고 SENDING 및 과거 성공/실패 시도를 보존합니다.

수신자 수정의 외부 발송 등록은 `POST /recipients/{id}/external-sends`에 선택한 발송일시를 기록합니다. 실제 문자는 보내지 않고 성공 횟수·최종 발송일시·이력에 반영하며 `source: EXTERNAL`로 앱 발송과 구분합니다. requestId로 중복 등록을 방지합니다.


### Nature 리브랜딩과 유지되는 운영 식별자

Go 모듈은 `github.com/sslim7/nature-was`, 컨테이너 실행 파일은 `/app/nature-was`다. 공개 앱/API 주소는 `nature.redhead.kr` / `nature-api.redhead.kr`로 통일했다. 실제 로컬 폴더(`/Users/max/Projects/jayeon-was`)와 GitHub 저장소(`sslim7/jayeon-was`)는 별도 저장소 이전 요청이 없으므로 유지한다.

Cloud Run `jayeon-was`, Firebase Hosting `redhead-jayeon-api`, 관련 서비스 계정, 이미지 경로의 `jayeon-was` 및 Secret Manager ID는 기존 운영 리소스다. 문자열만 바꾸면 다른 서비스가 생성될 수 있어 유지하며 `cloudbuild.yaml`의 `_SERVICE`도 동일하다. Terraform 구성은 `redhead-terraform/apps/nature`지만 GCS state prefix `apps/jayeon`은 그대로다. Firestore 프로젝트·`(default)` DB·컬렉션과 JWT 키를 바꾸지 않는다. 로컬 `demo-jayeon`도 기존 에뮬레이터 자료를 보존하는 식별자다.

이번 변경은 코드와 설정 준비다. DNS/Hosting 적용, 새 도메인 인증서 확인, 웹 재빌드, API 배포는 실행하지 않았다. 기존 jayeon 도메인은 이행 호환용으로 유지하며 자세한 수동 순서는 Terraform 저장소 `docs/NATURE_MIGRATION.md`를 따른다.
