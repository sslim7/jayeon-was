package main

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/joho/godotenv"
	"github.com/sslim7/nature-was/internal/admin"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/calls"
	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/sslim7/nature-was/internal/recipients"
	"github.com/sslim7/nature-was/internal/sms"
	"github.com/sslim7/nature-was/internal/userguard"
	"github.com/sslim7/nature-was/internal/users"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed docs/openapi.yaml
var openapiSpec []byte

// Swagger UI 페이지. 스펙은 /openapi.yaml 에서 읽고, UI 에셋은 CDN 에서 가져온다(로컬 개발용).
const swaggerHTML = `<!doctype html>
<html lang="ko">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Nature API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({ url: "/openapi.yaml", dom_id: "#swagger-ui" });
</script>
</body>
</html>`

func main() {
	// .env 는 로컬 개발용이다. Cloud Run 에는 파일이 없으므로 조용히 넘어간다.
	// 이미 설정된 OS 환경변수는 godotenv 가 덮어쓰지 않는다.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf(".env 로딩 실패(무시): %v", err)
	}

	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		log.Fatal("GOOGLE_CLOUD_PROJECT 환경변수가 필요하다")
	}

	// 인증에 필요한 시크릿. 없으면 토큰을 위조할 수 있으므로 기동을 막는다.
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		log.Fatal("JWT_SECRET 환경변수가 필요하다")
	}

	ctx := context.Background()

	// FIRESTORE_EMULATOR_HOST 가 설정되어 있으면 SDK 가 알아서 에뮬레이터에 붙는다.
	client, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("Firestore 클라이언트 생성 실패: %v", err)
	}
	defer client.Close()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Cloud Run 규약
	}

	// 미설정이면 CORS 헤더를 아예 붙이지 않는다. (운영 기본값)
	allowedOrigins := parseAllowedOrigins(os.Getenv("CORS_ALLOWED_ORIGINS"))

	mux := http.NewServeMux()

	// API 스펙과 Swagger UI (로컬 개발용 문서)
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Write(openapiSpec)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(swaggerHTML))
	})

	health := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		// 존재하지 않는 문서를 읽어 왕복만 확인한다. NotFound 면 연결은 정상이다.
		_, err := client.Collection("_health").Doc("_probe").Get(ctx)
		if err != nil && status.Code(err) != codes.NotFound {
			log.Printf("healthz: Firestore 연결 실패: %v", err)
			http.Error(w, "firestore unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}

	// Cloud Run 의 startup probe 가 쓰는 경로. 프로브는 컨테이너로 직접 오므로 정상 동작한다.
	mux.HandleFunc("GET /healthz", health)

	// 같은 핸들러의 외부용 별칭.
	// Google Frontend 가 정확히 `/healthz` 를 가로채 자체 404(HTML)를 돌려주기 때문에,
	// run.app 주소로도 앞단 호스팅 경유로도 이 경로는 컨테이너까지 오지 않는다.
	// 외부 업타임 감시와 배포 후 스모크 체크는 이쪽을 쓴다.
	mux.HandleFunc("GET /health", health)

	// Bearer 액세스 토큰을 파싱해 사용자 ID 를 주입하는 미들웨어의 발급자.
	// 401 판정은 미들웨어가 아니라 핸들러 몫이다 — 인증 없이 열리는 경로가 섞여 있다.
	tokens := auth.NewTokenIssuer(jwtSecret)
	auth.Register(mux, users.NewStore(client), tokens)
	users.Register(mux, client)

	// 개인정보 도메인은 현재 계정 상태와 임시 비밀번호 변경 여부도 확인한다.
	domainGuard := userguard.New(users.NewStore(client))
	recipients.Register(mux, client, domainGuard)
	calls.Register(mux, client, domainGuard)
	sms.Register(mux, client, domainGuard)
	messaging.Register(mux, client, domainGuard)

	// 어드민 API. ADMIN_JWT_SECRET 이 없으면 라우트를 하나도 걸지 않고 (nil, nil) 이다 —
	// 어드민 키 하나 때문에 사용자 앱까지 죽을 이유가 없다(internal/admin.Register).
	// 에러가 나는 경우는 ADMIN_JWT_SECRET 이 JWT_SECRET 과 같을 때뿐이고, 그때는
	// 어드민 토큰이 사용자 미들웨어에서도 파싱되는 위험한 설정이라 기동을 멈춘다.
	//
	// 반환되는 *Handler 가 도메인 어드민 라우트를 감쌀 가드다 — 인증·권한·감사를 한자리에서
	// 한다. **nil 이면 어드민이 꺼진 상태이므로 도메인 라우트도 걸지 않는다.** 걸어 두면
	// 첫 요청에서 nil 참조로 죽는데, 기동 로그에는 아무것도 남지 않아 원인을 찾기 어렵다.
	adminH, err := admin.Register(mux, client)
	if err != nil {
		log.Fatalf("어드민 API 초기화 실패: %v", err)
	}
	if adminH == nil {
		log.Println("ADMIN_JWT_SECRET 이 없다 — 어드민 API 를 켜지 않는다")
	}
	// adminH 가 nil 이 아닐 때 **여기에 도메인 어드민 라우트를 건다**
	// (예: posts.RegisterAdmin(mux, client, adminH)). 이 프로젝트에는 아직 도메인이 없어 비어 있다.

	// Bearer 액세스 토큰이 유효하면 사용자 ID 를 주입한다. 401 판정은 핸들러 몫이다.
	handler := withCORS(tokens.Middleware(mux), allowedOrigins)
	srv := &http.Server{Addr: ":" + port, Handler: handler}

	go func() {
		log.Printf("listening on :%s (project=%s)", port, projectID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("서버 실행 실패: %v", err)
		}
	}()

	// Cloud Run 은 종료 시 SIGTERM 을 보낸다.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown 실패: %v", err)
	}
	log.Println("서버 종료")
}

// corsPolicy 는 정확 일치 오리진 집합과 사설망 와일드카드 패턴을 담는다.
type corsPolicy struct {
	exact map[string]bool
	// `http://*:3000` 처럼 호스트 자리에 `*` 를 쓴 항목. 스킴·포트가 일치하고
	// 호스트가 사설 IP(또는 루프백)일 때만 허용한다 — DHCP 로 바뀌는 개발 머신
	// IP 를 매번 등록하지 않기 위한 로컬 개발용이며 운영에서는 쓰지 않는다.
	wildcard []*url.URL
}

func (p corsPolicy) allows(origin string) bool {
	if p.exact[origin] {
		return true
	}
	if len(p.wildcard) == 0 {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !(ip.IsPrivate() || ip.IsLoopback()) {
		return false
	}
	for _, w := range p.wildcard {
		if w.Scheme == u.Scheme && w.Port() == u.Port() {
			return true
		}
	}
	return false
}

// parseAllowedOrigins 는 콤마로 구분된 오리진 목록을 정책으로 만든다.
// 빈 문자열이면 빈 정책 — 즉 CORS 비활성.
func parseAllowedOrigins(raw string) corsPolicy {
	policy := corsPolicy{exact: make(map[string]bool)}
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o == "" {
			continue
		}
		if u, err := url.Parse(o); err == nil && u.Hostname() == "*" {
			policy.wildcard = append(policy.wildcard, u)
			continue
		}
		policy.exact[o] = true
	}
	return policy
}

// withCORS 는 정책이 허용하는 Origin 에만 CORS 헤더를 붙인다.
// 정책이 비었거나 일치하지 않으면 헤더 없이 그대로 통과시킨다.
func withCORS(next http.Handler, allowed corsPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		/*
		 * **Vary 는 Origin 이 없을 때도 붙인다.** 이게 없으면 앞단 캐시(호스팅·CDN)가
		 * 오리진을 구별하지 않는다 — 브라우저가 아닌 무언가(우리 curl, 헬스체크, 크롤러)가
		 * 먼저 부른 응답이 CORS 헤더 없이 캐시되고, 그 사본이 브라우저에게도 그대로 나간다.
		 * 그러면 브라우저는 본문을 못 읽고 우리가 보낸 안내가 「네트워크 오류」로 바뀐다 —
		 * 링크를 받은 사람이 와이파이를 의심하다 포기하던 것이 이것이었다.
		 *
		 * 헤더를 붙이는 조건(`allows`) 안에 두면 정작 그 사고가 나는 경우에만 빠진다.
		 */
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" && allowed.allows(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}

		// preflight 는 핸들러로 넘기지 않는다. 허용되지 않은 오리진이면
		// 위에서 헤더가 안 붙었으므로 브라우저가 알아서 막는다.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
