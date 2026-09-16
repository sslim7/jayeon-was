# jayeon-was

jayeon 서비스의 백엔드(WAS)다. 외부에 공개하는 상용 서비스가 아니라 내부에서 쓰는 서비스다.

**Go 1.26 · Google Cloud Firestore (SQL 아님) · 표준 `net/http` · Cloud Run**

웹 프레임워크도 ORM 도 두지 않는다. 라우팅은 Go 1.22+ `http.ServeMux` 의 메서드+경로 패턴을
그대로 쓴다. 개발 단계에서는 GCP 대신 **로컬 Firestore 에뮬레이터**를 쓴다.

- 운영 API 도메인: `https://jayeon-api.redhead.kr`
- 운영 인프라(GCP 프로젝트 `redhead`, DNS 포함)는 별도 레포 `redhead-terraform` 에서 관리한다.

> **아직 도메인 기능이 없다.** 지금 이 레포에 있는 것은 헬스체크, Swagger UI, CORS,
> 초대 전용 사용자 인증(`internal/auth`, `internal/users`), 어드민 인증·계정·활동로그(`internal/admin`),
> 공통 HTTP 응답 층(`internal/httpx`), 고루틴 panic recover(`internal/bg`) 뿐이다.
> 아래 [아직 없는 것](#아직-없는-것) 참고.

## 디렉토리 구조

```
.
├── main.go              엔트리포인트. 환경변수 검사 → 라우터 조립 → CORS → graceful shutdown
├── internal/            도메인 패키지
│   ├── admin            어드민 로그인·계정·활동로그, 어드민 라우트 가드
│   ├── auth             사용자 로그인·비밀번호 변경·JWT 발급 및 검증
│   ├── credentials      사용자와 어드민이 공유하는 비밀번호 해시·이메일 정규화
│   ├── users            Firestore 사용자 저장소와 내 프로필
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
| **jayeon** | 3100 | **3103** | **8090** | **4090** | **8091** | **8095** |

(jayeon 의 admin 3100 은 어드민 SPA 가 생길 때를 위해 비워 둔 자리다.)

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

`go build .` (패키지 하나) 은 리포 루트에 모듈명과 같은 **`jayeon-was` 바이너리**를 남긴다.
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
jayeon-api.redhead.kr  ──CNAME──▶  redhead-jayeon-api.web.app  ──rewrite──▶  Cloud Run jayeon-was
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
전달하면 앱(`jayeon-app`, 로컬 3103)에서 로그인하고 비밀번호를 변경한다.
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
  이미지 `asia-northeast3-docker.pkg.dev/redhead/redhead/jayeon-was`.
- 수동으로 돌릴 때는 `gcloud builds submit --config cloudbuild.yaml`.
- 배포가 끝나면 `/health` 로 스모크 체크한다(`/healthz` 는 밖에서 쓸 수 없다).

## 인프라 경계

> 🔴 **Cloud Run 서비스 설정(환경변수·시크릿·CPU·min-instances·`--allow-unauthenticated` 등)의
> 소유자는 `redhead-terraform` 이다. CI 는 이미지만 교체한다.**

`cloudbuild.yaml` 의 deploy 단계에 서비스 설정 플래그를 넣지 마라. CI 가 같이 넘기면 배포할
때마다 Terraform 이 만든 값을 CI 가 덮고, 다음 apply 에서 Terraform 이 되돌리는 핑퐁이 생긴다.
실제 증상은 "배포 후 환경변수가 사라져 기동 실패" 로 나타난다.
**설정을 바꿔야 하면 Terraform 쪽을 고쳐서 apply 한다.**

## 아직 없는 것

현재 인증과 공통 서버 기능까지 구현되어 있다. 서비스 도메인 기능은 아직 없다.

- 헬스체크(`/health`, `/healthz`), OpenAPI 스펙과 Swagger UI(`/openapi.yaml`, `/docs`)
- CORS 처리
- 초대 전용 사용자 로그인·토큰 재발급·비밀번호 변경·내 프로필과 `cmd/create-user`
- 어드민 인증·계정·활동로그(`internal/admin`), 첫 계정 생성 잡(`cmd/create-admin`)
- 공통 HTTP 응답 층(`internal/httpx`), 고루틴 panic recover(`internal/bg`)

공개 가입, 이메일 비밀번호 찾기, 사용자 프로필 수정, 로그인 시도 제한과 서비스 도메인 기능은 아직 없다.
