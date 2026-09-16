# nature WAS(nature-api.redhead.kr) — Cloud Run 컨테이너 이미지
#
# Cloud Run 은 x86-64 이미지만 받는다. Cloud Build 워커는 amd64 라 그대로 빌드하면 되지만,
# Apple Silicon 에서 만든 이미지를 직접 푸시할 거라면 `docker build --platform=linux/amd64` 로 빌드한다.

# --- build: static Go binary ---
FROM golang:1.26-alpine AS build
WORKDIR /src

# 의존성 목록만 먼저 넣어 go mod download 레이어를 캐시한다. 소스가 바뀌어도 이 레이어는 재사용된다.
COPY go.mod go.sum ./
RUN go mod download

# main.go 가 docs/openapi.yaml 을 //go:embed 로 바이너리에 넣는다.
# COPY 대상을 *.go 로 좁히면 빌드가 깨지므로 컨텍스트를 통째로 넣는다 (제외는 .dockerignore 가 담당한다).
COPY . .

# cgo 를 쓰는 의존성이 없다. static 으로 뽑아야 distroless/static 위에서 돈다.
# -trimpath 로 빌드 경로를 지우고 -s -w 로 디버그 심볼을 뺀다.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/nature-was .

# --- run: distroless (셸·패키지매니저 없음) ---
FROM gcr.io/distroless/static-debian12:nonroot

# 런타임에 디스크에서 읽는 파일이 없다. 바이너리 하나로 끝난다.
COPY --from=build /out/nature-was /app/nature-was

# PORT 는 Cloud Run 이 주입한다. 여기서 ENV 로 박으면 런타임 값을 가리게 되므로 넣지 않는다.
# (main.go — PORT 미설정 시에만 8080 으로 떨어진다.)
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/nature-was"]
