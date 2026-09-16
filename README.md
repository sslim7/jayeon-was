# jayeon-was

jayeon 서비스의 백엔드(WAS)다. 외부에 공개하는 상용 서비스가 아니라 내부에서 쓰는 서비스다.

**Go 1.26 · Google Cloud Firestore (SQL 아님) · 표준 `net/http` · Cloud Run**

웹 프레임워크도 ORM 도 두지 않는다. 라우팅은 Go 1.22+ `http.ServeMux` 의 메서드+경로 패턴을
그대로 쓴다. 개발 단계에서는 GCP 대신 **로컬 Firestore 에뮬레이터**를 쓴다.

- 운영 API 도메인: `https://jayeon-api.redhead.kr`
- 운영 인프라(GCP 프로젝트 `redhead`, DNS 포함)는 별도 레포 `redhead-terraform` 에서 관리한다.

> **아직 도메인 기능이 없다.** 지금 이 레포에 있는 것은 헬스체크, Swagger UI, CORS,
> 사용자 JWT 골격(`internal/auth`), 어드민 인증·계정·활동로그(`internal/admin`),
> 공통 HTTP 응답 층(`internal/httpx`), 고루틴 panic recover(`internal/bg`) 뿐이다.
> 아래 [아직 없는 것](#아직-없는-것) 참고.

## 디렉토리 구조

```
.
├── main.go              엔트리포인트. 환경변수 검사 → 라우터 조립 → CORS → graceful shutdown
├── internal/            도메인 패키지
│   ├── admin            어드민 로그인·계정·활동로그, 어드민 라우트 가드
│   ├── auth             사용자 JWT 발급·검증 미들웨어
│   ├── bg               응답을 보낸 뒤 도는 백그라운드 고루틴의 panic 안전장치
│   └── httpx            openapi.yaml 공통 에러 모델에 맞춘 JSON 응답 헬퍼
├── cmd/create-admin/    첫 어드민 계정을 만드는 1회성 잡
├── docs/openapi.yaml    OpenAPI 스펙 (main.go 가 //go:embed 로 바이너리에 넣는다)
├── Dockerfile           Cloud Run 이미지 (2단계 빌드)
├── cloudbuild.yaml      build → push → gcloud run deploy
├── firebase.json        로컬 개발용 에뮬레이터 설정 (firestore 8090 / ui 4090)
├── firebase.test.json   테스트 전용 에뮬레이터 설정 (firestore 8095, ui 없음)
└── .env.example         환경변수 템플릿 (정본)
```

## 로컬 개발 시작하기

사전 요구사항: Node.js, JDK 21+ (firebase-tools 15 요구사항),
firebase-tools (`npm install -g firebase-tools`).

### 1. 환경변수 파일을 만든다

```bash
cp .env.example .env
```

시크릿 두 개를 만들어 채운다. **서로 다른 값이어야 한다** — 같으면 서버가 기동하지 않는다
(이유는 `.env.example` 의 `ADMIN_JWT_SECRET` 설명에 있다).

```bash
openssl rand -base64 32   # JWT_SECRET 에 넣는다
openssl rand -base64 32   # ADMIN_JWT_SECRET 에 넣는다 (위와 다른 값)
```

### 2. Firestore 에뮬레이터를 띄운다

이 리포 루트(`firebase.json` 이 있는 곳)에서 실행한다.

```bash
firebase emulators:start --only firestore --project demo-jayeon \
  --import=./emulator-data --export-on-exit=./emulator-data
```

- 종료는 **Ctrl+C**. 종료 시 데이터를 `emulator-data/` 에 저장하고 다음 실행 때 복원한다.
  export 는 정상 종료일 때만 된다 — 강제 종료하면 마지막 export 이후 변경분은 사라진다.
  (`emulator-data/` 는 커밋하지 않는다)
- 프로젝트 ID 가 `demo-` 로 시작하면 완전 오프라인 모드라 Firebase 로그인이나
  실제 GCP 프로젝트가 필요 없다. `.env` 의 `GOOGLE_CLOUD_PROJECT` 와 같은 값을 쓴다.

| 주소 | 역할 |
| --- | --- |
| `localhost:8090` | Firestore 에뮬레이터 본체. SDK 가 붙는 곳 |
| `localhost:4090` | [Emulator UI](http://localhost:4090). 브라우저에서 데이터 확인 |

### 3. 서버를 띄운다

에뮬레이터를 띄운 채로 **별도 터미널**에서 실행한다.

```bash
go run .
```

- 포트는 `PORT` 를 따른다. 미설정 시 `8080`(Cloud Run 규약)이므로 **로컬은 `PORT=8091`** 이다
  — 아래 [로컬 포트](#로컬-포트) 참고.
- 종료는 **Ctrl+C**. SIGTERM/SIGINT 를 받으면 진행 중인 요청을 정리하고 내려간다
  (Cloud Run 도 종료 시 SIGTERM 을 보낸다).

### 4. Swagger UI 로 확인한다

<http://localhost:8091/docs> 를 연다. 스펙 원문은 `/openapi.yaml` 이다.
UI 에셋은 CDN(unpkg)에서 가져오므로 네트워크가 필요하다.

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

도메인 기능이 하나도 없다. 지금 있는 것은 골격뿐이다.

- 헬스체크(`/health`, `/healthz`), OpenAPI 스펙과 Swagger UI(`/openapi.yaml`, `/docs`)
- CORS 처리
- 사용자 JWT 발급·검증 골격(`internal/auth`) — 발급 경로를 무엇으로 열지는 아래 참고
- 어드민 인증·계정·활동로그(`internal/admin`), 첫 계정 생성 잡(`cmd/create-admin`)
- 공통 HTTP 응답 층(`internal/httpx`), 고루틴 panic recover(`internal/bg`)

**로그인 방식은 아직 정하지 않았다** (문자 인증 / 이메일 / 소셜 중 미정).
`internal/auth` 는 토큰 발급·검증만 담당하고, 신원을 어떻게 확인할지는 비어 있다.
