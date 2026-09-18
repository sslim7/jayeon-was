package callai

// alibaba.go 는 Alibaba Cloud Model Studio(DashScope, Singapore 리전) 공급자의 공통부다.
// ASR 은 alibaba_asr.go, LLM 은 alibaba_llm.go 에 있다.
//
// **왜 공식 SDK 를 쓰지 않는가** — 우리가 부르는 엔드포인트는 다섯 개뿐이다(업로드 정책,
// OSS 업로드, 전사 제출, 작업 조회, 채팅 완성). 반면 이 공급자는 `thinking_budget`,
// `X-DashScope-OssResourceResolve` 처럼 OpenAI 스펙에 없는 파라미터·헤더를 요구하는데,
// OpenAI Go SDK 는 그런 값을 ExtraFields 로 우회해야만 보낼 수 있다. 결국 SDK 를 써도
// 비표준 부분은 손으로 쓰게 되고, Cloud Run 이미지와 의존 표면만 커진다.
// 그래서 net/http 만 쓴다 — 이 패키지는 새 Go 모듈 의존성을 하나도 들이지 않는다.
//
// 🔴 **이 파일들은 통화 원문·전사문·분석 내용을 절대 로그나 에러 메시지에 담지 않는다.**
// 남길 수 있는 것은 request_id, 공급자 에러 코드, HTTP status 뿐이다.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// 🔴 날짜 스냅샷 모델명(qwen3.7-plus-2026-xx-xx 같은 것)을 기본값으로 쓰지 마라.
	// 스냅샷 모델은 RPM 이 60 으로 급락해서, 통화가 몇 건만 겹쳐도 429 가 쏟아진다.
	// 별칭(alias) 모델명이 상위 쿼터를 쓴다.
	defaultLLMModel = "qwen3.7-plus"
	defaultASRModel = "qwen-audio-3.0-asr-flash-filetrans"

	// JSON 제어 응답 상한. 작업 조회·채팅 응답은 이보다 훨씬 작다.
	maxAPIBodyBytes = 4 << 20
	// 에러 본문은 분류에 필요한 만큼만 읽는다.
	maxErrorBodyBytes = 64 << 10
	// 에러 메시지로 남길 최대 길이(rune). 공급자 메시지가 입력 일부를 되비추는 경우가 있어 자른다.
	maxErrorMessageRunes = 300
)

var errBodyTooLarge = errors.New("응답 본문이 상한을 넘었다")

// alibabaClient 는 ASR/LLM 이 공유하는 HTTP 껍데기다.
type alibabaClient struct {
	baseURL string // 스킴 포함, 끝 슬래시 없음
	apiKey  string
	// http 는 제어 API(정책 조회, 제출, 폴링, 채팅)용이다. Config.Timeout 이 통째로 걸린다.
	http *http.Client
	// upload 는 오디오 업로드 전용이다.
	//
	// 🔴 http.Client.Timeout 은 **요청 본문을 다 밀어 넣는 시간까지 포함**한다. 30초짜리
	// 타임아웃을 그대로 쓰면 1시간짜리 통화 업로드가 항상 그 자리에서 끊긴다.
	// 그래서 전체 타임아웃은 걸지 않고 ctx 로만 제어하되, 본문을 다 보낸 뒤
	// 응답 헤더를 기다리는 구간(ResponseHeaderTimeout)만 따로 묶어 무한 대기를 막는다.
	upload *http.Client
}

func newAlibabaClient(c Config) (*alibabaClient, error) {
	key := strings.TrimSpace(c.APIKey)
	if key == "" {
		// 기동 시점에 잡히게 한다. 첫 통화가 들어온 뒤에 알게 되면 그 통화를 잃는다.
		return nil, errors.New("callai: alibaba: CALL_AI_API_KEY 가 비어 있다")
	}
	base, err := normalizeBaseURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// 🔴 X-DashScope-WorkSpace 헤더는 보내지 않는다. Singapore 리전의 전용 호스트
	// (ws-xxxxx.ap-southeast-1.maas.aliyuncs.com)에 워크스페이스가 이미 박혀 있어서,
	// 헤더를 덧붙이면 중복 지정이 된다. Config.WorkspaceID 는 설정 확인용으로만 남겨 둔다.
	up, _ := http.DefaultTransport.(*http.Transport)
	var upTr http.RoundTripper
	if up != nil {
		t := up.Clone()
		t.ResponseHeaderTimeout = timeout
		upTr = t
	}
	return &alibabaClient{
		baseURL: base,
		apiKey:  key,
		http:    &http.Client{Timeout: timeout},
		upload:  &http.Client{Transport: upTr},
	}, nil
}

// normalizeBaseURL 은 설정값에서 **호스트만** 뽑아 `https://{host}` 를 만든다.
//
// 콘솔이 알려 주는 값이 `ws-xxxxx.ap-southeast-1.maas.aliyuncs.com` 처럼 스킴 없는
// 호스트명이라, 운영자가 옮겨 적은 값을 그대로 쓸 수 있어야 한다.
//
// 🔴 **엔드포인트 경로는 이 패키지가 소유한다. 설정에서는 호스트만 받는다.**
// 설정값에 경로가 섞여 들어오면 경로가 두 번 붙어 전부 404 가 된다 — 실제로
// `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` 처럼 경로 suffix 가 붙은 값이
// 인프라 쪽에 준비돼 있었고, 그대로 배포됐다면
// `.../compatible-mode/v1/api/v1/services/audio/asr/transcription` 을 부르다 전부 깨졌을 것이다.
// 그래서 파싱한 뒤 path·query·fragment 를 전부 버린다.
//
// http:// 로 들어와도 https:// 로 올린다 — 이 경로로 API 키와 통화 오디오가 지나간다.
// 업로드 정책이 알려 주는 upload_host 도 같은 함수를 지나간다(그쪽도 호스트만 필요하다).
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("callai: alibaba: CALL_AI_BASE_URL 이 비어 있다")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("callai: alibaba: CALL_AI_BASE_URL 을 해석하지 못했다: %q", raw)
	}
	return "https://" + u.Host, nil
}

func newAlibabaTranscriber(c Config) (Transcriber, error) {
	cl, err := newAlibabaClient(c)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(c.ASRModel)
	if model == "" {
		model = defaultASRModel
	}
	return &alibabaTranscriber{c: cl, model: model}, nil
}

func newAlibabaAnalyzer(c Config) (Analyzer, error) {
	cl, err := newAlibabaClient(c)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(c.LLMModel)
	if model == "" {
		model = defaultLLMModel
	}
	// 스키마가 깨져 있으면 첫 통화가 아니라 기동 때 알아야 한다.
	if !json.Valid(callAnalysisSchema) {
		return nil, errors.New("callai: alibaba: 내장 분석 스키마가 올바른 JSON 이 아니다")
	}
	return &alibabaAnalyzer{
		c:              cl,
		model:          model,
		thinkingBudget: c.ThinkingBudget,
		maxTokens:      c.MaxTokens,
	}, nil
}

// ---------- 요청/응답 공통 ----------

// apiEnvelope 는 DashScope 와 OpenAI 호환 모드의 에러 모양을 한꺼번에 받는다.
// DashScope 는 {"code","message","request_id"}, 호환 모드는 {"error":{"code","message"}} 로 준다.
type apiEnvelope struct {
	Code      json.RawMessage `json:"code"`
	Message   string          `json:"message"`
	RequestID string          `json:"request_id"`
	Error     *struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
}

// ossErrorDoc 는 OSS 가 돌려주는 XML 에러다(업로드 단계에서만 나온다).
type ossErrorDoc struct {
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
	RequestID string `xml:"RequestId"`
}

func (c *alibabaClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, &Error{Kind: KindPermanent, Code: "BadRequestURL", Message: "요청 URL 을 만들지 못했다", Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// doJSON 은 요청을 보내고 성공 본문을 out 에 담는다.
//
// 🔴 2xx 라고 성공이 아니다. DashScope 는 200 에 top-level `code` 를 실어 실패를 알리는
// 경로가 있어서, status 와 code 를 둘 다 본 뒤에야 성공으로 친다.
func (c *alibabaClient) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()

	limit := int64(maxAPIBodyBytes)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		limit = maxErrorBodyBytes
	}
	body, rerr := readLimited(resp.Body, limit)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp.StatusCode, headerRequestID(resp), body)
	}
	if rerr != nil {
		if errors.Is(rerr, errBodyTooLarge) {
			return &Error{Kind: KindPermanent, Code: "ResponseTooLarge", Status: resp.StatusCode, RequestID: headerRequestID(resp), Message: "공급자 응답이 상한을 넘었다"}
		}
		return transportError(rerr)
	}
	var env apiEnvelope
	_ = json.Unmarshal(body, &env)
	if code := envelopeCode(env); code != "" {
		return newAPIError(0, headerRequestID(resp), body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return &Error{Kind: KindPermanent, Code: "BadResponseJSON", Status: resp.StatusCode, RequestID: headerRequestID(resp), Message: "공급자 응답을 해석하지 못했다", Err: err}
		}
	}
	return nil
}

func envelopeCode(env apiEnvelope) string {
	if env.Error != nil {
		if c := rawToString(env.Error.Code); c != "" {
			return c
		}
	}
	return rawToString(env.Code)
}

// newAPIError 는 공급자 응답을 *Error 로 옮긴다. status 0 은 「HTTP 는 성공했는데
// 본문이 실패를 말한다」는 뜻이고, 이때 분류는 코드 문자열만으로 정해진다.
func newAPIError(status int, headerReqID string, body []byte) *Error {
	var env apiEnvelope
	_ = json.Unmarshal(body, &env)
	code := envelopeCode(env)
	msg := env.Message
	if env.Error != nil && env.Error.Message != "" {
		msg = env.Error.Message
	}
	reqID := env.RequestID

	if code == "" && msg == "" {
		// OSS 업로드 실패는 JSON 이 아니라 XML 로 온다.
		var oss ossErrorDoc
		if xml.Unmarshal(body, &oss) == nil {
			code, msg = oss.Code, oss.Message
			if reqID == "" {
				reqID = oss.RequestID
			}
		}
	}
	if reqID == "" {
		reqID = headerReqID
	}
	if msg == "" {
		// 🔴 해석 못 한 본문을 그대로 넣지 않는다. 무엇이 들었는지 보장할 수 없다.
		msg = "공급자가 해석할 수 없는 에러 응답을 보냈다"
	}
	return &Error{
		Kind:      classifyHTTP(status, code),
		Code:      code,
		Status:    status,
		RequestID: reqID,
		Message:   firstRunes(strings.TrimSpace(msg), maxErrorMessageRunes),
	}
}

// transportError 는 네트워크 계층 실패를 재시도 대상으로 옮긴다.
// 호출부가 취소한 경우는 재시도가 아니라 중단이므로 ctx 에러를 그대로 올려보낸다.
func transportError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &Error{Kind: KindRetryable, Code: "Transport", Message: "공급자에 연결하지 못했다", Err: err}
}

func headerRequestID(resp *http.Response) string {
	for _, k := range []string{"X-Request-Id", "X-Dashscope-Request-Id", "X-Oss-Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// readLimited 는 상한을 넘는 본문을 조용히 자르지 않고 에러로 만든다.
// 잘린 JSON 을 해석하면 「내용이 절반뿐인 전사문」이 성공처럼 저장된다.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > max {
		return b[:max], errBodyTooLarge
	}
	return b, nil
}

// rawToString 은 문자열일 수도 숫자일 수도 있는 값을 안전하게 문자열로 만든다.
// speaker_id 와 error.code 가 공급자·모델에 따라 타입이 갈려서, 타입을 못 박으면
// 그 응답 하나 때문에 통화 전체가 실패한다.
func rawToString(r json.RawMessage) string {
	t := strings.TrimSpace(string(r))
	if t == "" || t == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	var f float64
	if json.Unmarshal(r, &f) == nil {
		if f == math.Trunc(f) && math.Abs(f) < 1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	var b bool
	if json.Unmarshal(r, &b) == nil {
		return strconv.FormatBool(b)
	}
	return ""
}
