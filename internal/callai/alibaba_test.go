package callai

// alibaba_test.go 는 httptest 로 공급자 전 구간을 흉내 내 **네트워크 없이** 계약을 고정한다.
// 여기서 확인하는 것들(헤더, 폼 필드 순서, 보내지 않아야 할 파라미터)은 실호출 한 번으로
// 알아낸 사실이라, 실수로 되돌아가면 실서비스에서만 깨진다.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 테스트용 공급자 서버 ----------

type fakeStudio struct {
	t *testing.T

	mu sync.Mutex
	// 폴링이 돌려줄 task_status 를 순서대로 담는다.
	statuses []string
	pollIdx  int

	// 업로드 단계에서 서버가 실제로 받은 것들.
	uploadPartOrder []string
	uploadFields    map[string]string
	uploadFileBytes []byte
	uploadFileName  string

	// 제출 단계에서 받은 것들.
	submitHeaders http.Header
	submitBody    map[string]any

	// 분석 단계에서 받은 것들.
	chatBody map[string]any

	// 응답 조작용.
	transcriptionDoc string
	chatResponse     string
	failedCode       string
	failedMessage    string
}

func (f *fakeStudio) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/uploads", f.getPolicy)
	mux.HandleFunc("/api/v1/services/audio/asr/transcription", f.submit)
	mux.HandleFunc("/api/v1/tasks/", f.task)
	mux.HandleFunc("/transcription.json", f.transcription)
	mux.HandleFunc("/compatible-mode/v1/chat/completions", f.chat)
	// 업로드 정책이 알려 주는 upload_host 는 호스트뿐이라 루트로 들어온다.
	mux.HandleFunc("/", f.upload)
	return mux
}

func (f *fakeStudio) requireBearer(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer test-key" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"InvalidApiKey","message":"bad key","request_id":"rq-401"}`)
		return false
	}
	return true
}

func (f *fakeStudio) getPolicy(w http.ResponseWriter, r *http.Request) {
	if !f.requireBearer(w, r) {
		return
	}
	if got := r.URL.Query().Get("action"); got != "getPolicy" {
		f.t.Errorf("action=%q, getPolicy 여야 한다", got)
	}
	if r.URL.Query().Get("model") == "" {
		f.t.Error("model 쿼리가 비어 있다")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": "rq-policy",
		"data": map[string]any{
			"policy":                 "cG9saWN5",
			"signature":              "sig123",
			"upload_dir":             "call-ai/2026-09-18",
			"upload_host":            "https://" + r.Host,
			"oss_access_key_id":      "AK123",
			"x_oss_object_acl":       "private",
			"x_oss_forbid_overwrite": true,
			"expire_in_seconds":      300,
			"max_file_size_mb":       1024,
		},
	})
}

func (f *fakeStudio) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	// 🔴 업로드는 Bearer 가 아니라 정책·서명으로 인증한다. Authorization 헤더가 붙으면 안 된다.
	if r.Header.Get("Authorization") != "" {
		f.t.Error("업로드 요청에 Authorization 헤더가 붙었다")
	}
	mr, err := r.MultipartReader()
	if err != nil {
		f.t.Errorf("multipart 아님: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.uploadFields = map[string]string{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.t.Errorf("part 읽기 실패: %v", err)
			break
		}
		f.uploadPartOrder = append(f.uploadPartOrder, p.FormName())
		if p.FormName() == "file" {
			f.uploadFileName = p.FileName()
			b, _ := io.ReadAll(p)
			f.uploadFileBytes = b
			continue
		}
		b, _ := io.ReadAll(p)
		f.uploadFields[p.FormName()] = string(b)
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeStudio) submit(w http.ResponseWriter, r *http.Request) {
	if !f.requireBearer(w, r) {
		return
	}
	f.submitHeaders = r.Header.Clone()
	body, _ := io.ReadAll(r.Body)
	f.submitBody = map[string]any{}
	if err := json.Unmarshal(body, &f.submitBody); err != nil {
		f.t.Errorf("제출 본문이 JSON 이 아니다: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": "rq-submit",
		"output":     map[string]any{"task_id": "task-1", "task_status": "PENDING"},
	})
}

func (f *fakeStudio) task(w http.ResponseWriter, r *http.Request) {
	if !f.requireBearer(w, r) {
		return
	}
	f.mu.Lock()
	status := "SUCCEEDED"
	if f.pollIdx < len(f.statuses) {
		status = f.statuses[f.pollIdx]
	}
	f.pollIdx++
	f.mu.Unlock()

	out := map[string]any{"task_id": "task-1", "task_status": status}
	switch status {
	case "SUCCEEDED":
		out["results"] = []any{map[string]any{
			"file_url":          "oss://call-ai/x.m4a",
			"subtask_status":    "SUCCEEDED",
			"transcription_url": "https://" + r.Host + "/transcription.json",
		}}
	case "FAILED":
		out["code"] = f.failedCode
		out["message"] = f.failedMessage
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": "rq-task",
		"output":     out,
		"usage":      map[string]any{"duration": 123.5},
	})
}

func (f *fakeStudio) transcription(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		f.t.Error("서명 URL 요청에 Authorization 헤더가 붙었다")
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, f.transcriptionDoc)
}

func (f *fakeStudio) chat(w http.ResponseWriter, r *http.Request) {
	if !f.requireBearer(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.chatBody = map[string]any{}
	if err := json.Unmarshal(body, &f.chatBody); err != nil {
		f.t.Errorf("채팅 본문이 JSON 이 아니다: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, f.chatResponse)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// startFake 는 TLS 서버를 띄운다.
// normalizeBaseURL 이 http:// 를 https:// 로 올리기 때문에 평문 서버로는 이 경로를 못 탄다.
func startFake(t *testing.T, f *fakeStudio) (*httptest.Server, *alibabaClient) {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := newAlibabaClient(Config{BaseURL: srv.URL, APIKey: "test-key", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("클라이언트 생성 실패: %v", err)
	}
	// 테스트 인증서를 신뢰하는 클라이언트로 갈아 끼운다.
	c.http = srv.Client()
	c.upload = srv.Client()
	c.http.Timeout = 10 * time.Second
	return srv, c
}

const sampleTranscriptionDoc = `{
  "file_url": "oss://call-ai/x.m4a",
  "transcripts": [{
    "channel_id": 0,
    "text": "전체 전사문",
    "sentences": [
      {"begin_time": 5000, "end_time": 6000, "text": "다섯", "sentence_id": 3, "speaker_id": 0},
      {"begin_time": 1000, "end_time": 2000, "text": "하나", "sentence_id": 1, "speaker_id": "spk_1"},
      {"begin_time": 3000, "end_time": 3500, "text": "셋", "sentence_id": 2},
      {"begin_time": 7000, "end_time": 6000, "text": "일곱", "sentence_id": 4, "speaker_id": 1},
      {"begin_time": 9000, "end_time": 9500, "text": "   ", "sentence_id": 5, "speaker_id": 1}
    ]
  }]
}`

func testAudio(data string) Audio {
	return Audio{
		Open:         func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil },
		Size:         int64(len(data)),
		FileName:     "recording.m4a",
		ContentType:  "audio/mp4",
		SpeakerCount: 2,
	}
}

// ---------- ASR 전 구간 ----------

func TestAlibabaTranscribeFlow(t *testing.T) {
	f := &fakeStudio{t: t, statuses: []string{"PENDING", "RUNNING", "SUCCEEDED"}, transcriptionDoc: sampleTranscriptionDoc}
	_, c := startFake(t, f)
	tr := &alibabaTranscriber{c: c, model: defaultASRModel}

	const audio = "가짜 오디오 바이트"
	st, err := tr.Start(context.Background(), testAudio(audio))
	if err != nil {
		t.Fatalf("Start 실패: %v", err)
	}
	if st.Status != StatusRunning || st.Token != "task-1" {
		t.Fatalf("Start 결과가 이상하다: %+v", st)
	}
	if st.Model != defaultASRModel {
		t.Errorf("Model=%q", st.Model)
	}
	if st.RequestID != "rq-submit" {
		t.Errorf("RequestID=%q", st.RequestID)
	}

	// --- 업로드 폼 검증 ---
	for _, k := range []string{"OSSAccessKeyId", "policy", "signature", "key", "success_action_status"} {
		if f.uploadFields[k] == "" {
			t.Errorf("업로드 폼에 %s 가 없다 (받은 것: %v)", k, f.uploadFields)
		}
	}
	if f.uploadFields["policy"] != "cG9saWN5" || f.uploadFields["signature"] != "sig123" {
		t.Errorf("정책/서명이 그대로 전달되지 않았다: %v", f.uploadFields)
	}
	if f.uploadFields["success_action_status"] != "200" {
		t.Errorf("success_action_status=%q", f.uploadFields["success_action_status"])
	}
	if f.uploadFields["x-oss-forbid-overwrite"] != "true" {
		t.Errorf("x-oss-forbid-overwrite=%q (bool 을 문자열로 옮겨야 한다)", f.uploadFields["x-oss-forbid-overwrite"])
	}
	key := f.uploadFields["key"]
	if !strings.HasPrefix(key, "call-ai/2026-09-18/") || !strings.HasSuffix(key, ".m4a") {
		t.Errorf("key=%q — upload_dir 접두사와 확장자가 살아 있어야 한다", key)
	}
	if strings.Contains(key, "recording") {
		t.Errorf("key=%q — 사용자 파일명을 그대로 쓰면 안 된다", key)
	}
	if n := len(f.uploadPartOrder); n == 0 || f.uploadPartOrder[n-1] != "file" {
		t.Errorf("파트 순서=%v — file 이 마지막이어야 한다", f.uploadPartOrder)
	}
	if string(f.uploadFileBytes) != audio {
		t.Errorf("업로드된 바이트가 다르다: %q", string(f.uploadFileBytes))
	}

	// --- 제출 요청 검증 ---
	if f.submitHeaders.Get("X-DashScope-Async") != "enable" {
		t.Error("X-DashScope-Async 헤더가 없다")
	}
	if f.submitHeaders.Get("X-DashScope-OssResourceResolve") != "enable" {
		t.Error("🔴 X-DashScope-OssResourceResolve 헤더가 없다 — oss:// 참조가 풀리지 않는다")
	}
	if f.submitHeaders.Get("X-DashScope-WorkSpace") != "" {
		t.Error("워크스페이스 헤더는 보내지 않아야 한다")
	}
	rawSubmit, _ := json.Marshal(f.submitBody)
	for _, banned := range []string{"enable_words", "enable_itn"} {
		if bytes.Contains(rawSubmit, []byte(banned)) {
			t.Errorf("🔴 %s 를 보냈다 — 공급자가 조용히 무시하는 파라미터다", banned)
		}
	}
	params, _ := f.submitBody["parameters"].(map[string]any)
	if hints, _ := params["language_hints"].([]any); len(hints) != 1 || hints[0] != "ko" {
		t.Errorf("language_hints=%v, [ko] 여야 한다", params["language_hints"])
	}
	if params["diarization_enabled"] != true {
		t.Errorf("diarization_enabled=%v", params["diarization_enabled"])
	}
	if params["speaker_count"] != float64(2) {
		t.Errorf("speaker_count=%v", params["speaker_count"])
	}
	input, _ := f.submitBody["input"].(map[string]any)
	urls, _ := input["file_urls"].([]any)
	if len(urls) != 1 || !strings.HasPrefix(urls[0].(string), "oss://call-ai/2026-09-18/") {
		t.Errorf("file_urls=%v — oss:// 참조여야 한다", input["file_urls"])
	}

	// --- 폴링 ---
	for i, want := range []Status{StatusRunning, StatusRunning, StatusDone} {
		st, err = tr.Poll(context.Background(), st.Token)
		if err != nil {
			t.Fatalf("폴링 %d 실패: %v", i, err)
		}
		if st.Status != want {
			t.Fatalf("폴링 %d: status=%q, 기대 %q", i, st.Status, want)
		}
	}
	if st.Usage.AudioSeconds != 123.5 {
		t.Errorf("AudioSeconds=%v", st.Usage.AudioSeconds)
	}
	if st.Transcript == nil {
		t.Fatal("Transcript 가 nil 이다")
	}
	if st.Transcript.Text != "전체 전사문" {
		t.Errorf("Text=%q", st.Transcript.Text)
	}
	want := []Segment{
		{Start: 1, End: 2, Text: "하나", Speaker: "spk_1"},
		{Start: 3, End: 3.5, Text: "셋", Speaker: ""},
		{Start: 5, End: 6, Text: "다섯", Speaker: "0"},
		{Start: 7, End: 7, Text: "일곱", Speaker: "1"}, // End < Start 보정
	}
	if len(st.Transcript.Segments) != len(want) {
		t.Fatalf("세그먼트 %d개, 기대 %d개: %+v", len(st.Transcript.Segments), len(want), st.Transcript.Segments)
	}
	for i, w := range want {
		if st.Transcript.Segments[i] != w {
			t.Errorf("세그먼트[%d]=%+v, 기대 %+v", i, st.Transcript.Segments[i], w)
		}
	}
}

func TestPollUnknownStatusIsRunning(t *testing.T) {
	// 🔴 공급자가 새 상태를 추가해도 멀쩡한 작업을 실패로 확정하면 안 된다.
	f := &fakeStudio{t: t, statuses: []string{"QUEUING_IN_SOME_NEW_WAY"}}
	_, c := startFake(t, f)
	tr := &alibabaTranscriber{c: c, model: defaultASRModel}
	st, err := tr.Poll(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("모르는 상태에서 에러가 났다: %v", err)
	}
	if st.Status != StatusRunning {
		t.Errorf("status=%q, RUNNING 이어야 한다", st.Status)
	}
	if st.Transcript != nil {
		t.Error("진행 중인데 Transcript 가 채워졌다")
	}
}

func TestPollFailedClassification(t *testing.T) {
	cases := []struct {
		code string
		want Kind
	}{
		{"DataInspectionFailed", KindContentFiltered},
		{"BadRequest.InputDownloadFailed", KindInputUnavailable},
		{"Throttling.RateQuota", KindRetryable},
		// 비동기 작업 실패는 HTTP status 가 200 이라 코드 문자열만으로 판단한다.
		// 모르는 코드는 재시도로 남는다 — 확정 실패로 못 박아 통화를 버리는 것보다,
		// 호출부의 시도 횟수 상한에 걸려 멈추는 쪽이 안전하다.
		{"InvalidParameter", KindRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			f := &fakeStudio{t: t, statuses: []string{"FAILED"}, failedCode: tc.code, failedMessage: "공급자 메시지"}
			_, c := startFake(t, f)
			tr := &alibabaTranscriber{c: c, model: defaultASRModel}
			_, err := tr.Poll(context.Background(), "task-1")
			if err == nil {
				t.Fatal("FAILED 인데 에러가 없다")
			}
			if KindOf(err) != tc.want {
				t.Errorf("kind=%v, 기대 %v", KindOf(err), tc.want)
			}
			if CodeOf(err) != tc.code {
				t.Errorf("code=%q", CodeOf(err))
			}
			if RequestIDOf(err) != "rq-task" {
				t.Errorf("request_id=%q", RequestIDOf(err))
			}
		})
	}
}

func TestParseTranscriptionEmpty(t *testing.T) {
	// 🔴 Segments 가 nil 이면 저장 쪽이 거부한다.
	tr, err := parseTranscription([]byte(`{"transcripts":[]}`), "rq")
	if err != nil {
		t.Fatalf("파싱 실패: %v", err)
	}
	if tr.Text != "" {
		t.Errorf("Text=%q", tr.Text)
	}
	if tr.Segments == nil {
		t.Fatal("Segments 가 nil 이다 — 빈 슬라이스여야 한다")
	}
	if len(tr.Segments) != 0 {
		t.Errorf("Segments=%+v", tr.Segments)
	}
}

// ---------- 에러 분류표 ----------

func TestErrorClassificationTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		want   Kind
	}{
		{"429 스로틀링", 429, "Throttling.RateQuota", KindRetryable},
		{"500 서버", 500, "InternalError", KindRetryable},
		{"400 콘텐츠 필터", 400, "DataInspectionFailed", KindContentFiltered},
		{"400 오디오 못 받음", 400, "BadRequest.InputDownloadFailed", KindInputUnavailable},
		{"401 키 오류", 401, "InvalidApiKey", KindPermanent},
		{"404 모델 없음", 404, "ModelNotFound", KindPermanent},
		{"403 미구매", 403, "AccessDenied.Unpurchased", KindPermanent},
		{"400 형식 오류", 400, "InvalidParameter", KindPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"code":"` + tc.code + `","message":"공급자 메시지","request_id":"rq-1"}`)
			e := newAPIError(tc.status, "hdr-req", body)
			if e.Kind != tc.want {
				t.Errorf("kind=%v, 기대 %v", e.Kind, tc.want)
			}
			if e.Code != tc.code || e.Status != tc.status || e.RequestID != "rq-1" {
				t.Errorf("에러 메타가 어긋났다: %+v", e)
			}
			if e.Message != "공급자 메시지" {
				t.Errorf("message=%q", e.Message)
			}
		})
	}
}

func TestErrorRequestIDFallsBackToHeader(t *testing.T) {
	e := newAPIError(500, "hdr-req", []byte(`{"code":"InternalError","message":"boom"}`))
	if e.RequestID != "hdr-req" {
		t.Errorf("request_id=%q, 헤더 값을 써야 한다", e.RequestID)
	}
}

func TestErrorFromOpenAIShapeAndOSSXML(t *testing.T) {
	e := newAPIError(401, "", []byte(`{"error":{"code":"InvalidApiKey","message":"invalid key"},"request_id":"rq-2"}`))
	if e.Kind != KindPermanent || e.Code != "InvalidApiKey" || e.RequestID != "rq-2" {
		t.Errorf("호환 모드 에러 모양을 못 읽었다: %+v", e)
	}
	x := newAPIError(403, "", []byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>denied</Message><RequestId>oss-1</RequestId></Error>`))
	if x.Code != "AccessDenied" || x.RequestID != "oss-1" {
		t.Errorf("OSS XML 에러를 못 읽었다: %+v", x)
	}
}

func TestUnparseableErrorBodyIsNotEchoed(t *testing.T) {
	// 🔴 해석 못 한 본문을 Message 에 그대로 넣으면 통화 내용이 로그로 샐 수 있다.
	body := []byte("고객 이름은 홍길동이고 계좌번호는 123-456 입니다")
	e := newAPIError(500, "", body)
	if strings.Contains(e.Message, "홍길동") {
		t.Errorf("에러 메시지에 본문이 그대로 들어갔다: %q", e.Message)
	}
}

func TestDoJSONTreats200WithCodeAsFailure(t *testing.T) {
	// DashScope 는 200 에 top-level code 를 실어 실패를 알리는 경로가 있다.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"code": "Throttling.RateQuota", "message": "slow down", "request_id": "rq-3"})
	}))
	defer srv.Close()
	c, err := newAlibabaClient(Config{BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c.http = srv.Client()
	req, _ := c.newRequest(context.Background(), http.MethodGet, "/x", nil)
	err = c.doJSON(req, &struct{}{})
	if err == nil {
		t.Fatal("200 + code 인데 성공으로 봤다")
	}
	if KindOf(err) != KindRetryable || CodeOf(err) != "Throttling.RateQuota" {
		t.Errorf("분류가 틀렸다: %v", err)
	}
}

// ---------- LLM ----------

const sampleChatResponse = `{
  "id": "chatcmpl-abc",
  "request_id": "rq-chat",
  "model": "qwen3.7-plus",
  "choices": [{
    "finish_reason": "stop",
    "message": {"content": "{\"summary\":\"요약문\",\"details\":[{\"title\":\"배송\",\"content\":\"다음 주 발송\"}],\"todos\":[{\"content\":\"견적 보내기\",\"owner\":null,\"due_date\":null,\"source\":\"견적 좀 보내주세요\"},{\"content\":\"전화\",\"owner\":\"  \",\"due_date\":\"2026-10-01\",\"source\":\"화요일에 전화드릴게요\"}]}"}
  }],
  "usage": {"prompt_tokens": 1200, "completion_tokens": 300, "completion_tokens_details": {"reasoning_tokens": 64}}
}`

func testTranscript() Transcript {
	return Transcript{
		Text: "안녕하세요 견적 좀 보내주세요",
		Segments: []Segment{
			{Start: 1.2, End: 3, Text: "안녕하세요", Speaker: "0"},
			{Start: 3.5, End: 6, Text: "견적 좀 보내주세요", Speaker: "1"},
		},
	}
}

func TestAlibabaAnalyze(t *testing.T) {
	f := &fakeStudio{t: t, chatResponse: sampleChatResponse}
	_, c := startFake(t, f)
	a := &alibabaAnalyzer{c: c, model: defaultLLMModel, thinkingBudget: 512, maxTokens: 4096}

	got, err := a.Analyze(context.Background(), testTranscript())
	if err != nil {
		t.Fatalf("Analyze 실패: %v", err)
	}

	// --- 요청 본문 ---
	rf, _ := f.chatBody["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type=%v", rf["type"])
	}
	if js["strict"] != true {
		t.Errorf("🔴 json_schema.strict 가 true 가 아니다: %v", js["strict"])
	}
	if js["name"] != "call_analysis" {
		t.Errorf("json_schema.name=%v", js["name"])
	}
	schema, _ := js["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Error("🔴 strict 모드에서는 최상위에 additionalProperties:false 가 있어야 한다")
	}
	if req, _ := schema["required"].([]any); len(req) != 5 {
		t.Errorf("최상위 required=%v — 모든 프로퍼티가 들어가야 한다", schema["required"])
	}
	// 🔴 thinking 제어와 max_tokens 는 **둘 다** 있어야 한다.
	if f.chatBody["thinking_budget"] != float64(512) {
		t.Errorf("thinking_budget=%v", f.chatBody["thinking_budget"])
	}
	if f.chatBody["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens=%v — thinking 예산만으로는 답변 토큰이 막히지 않는다", f.chatBody["max_tokens"])
	}
	msgs, _ := f.chatBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "연도를 지어내지 마라") {
		t.Error("🔴 시스템 프롬프트가 빠졌다 — 모델이 연도를 지어낸다")
	}
	user, _ := msgs[1].(map[string]any)
	if uc := user["content"].(string); !strings.Contains(uc, "견적 좀 보내주세요") || !strings.Contains(uc, "화자 1") {
		t.Errorf("사용자 메시지에 전사문/화자 구분이 없다: %q", uc)
	}

	// --- 응답 매핑 ---
	if got.Content.Summary != "요약문" {
		t.Errorf("Summary=%q", got.Content.Summary)
	}
	if got.Model != "qwen3.7-plus" || got.RequestID != "rq-chat" || got.PromptVersion != callAnalysisPromptVersion {
		t.Errorf("메타가 어긋났다: %+v", got)
	}
	wantUsage := Usage{PromptTokens: 1200, CompletionTokens: 300, ReasoningTokens: 64}
	if got.Usage != wantUsage {
		t.Errorf("usage=%+v, 기대 %+v", got.Usage, wantUsage)
	}
	if len(got.Content.Todos) != 2 {
		t.Fatalf("todos=%+v", got.Content.Todos)
	}
	if got.Content.Todos[0].Owner != nil || got.Content.Todos[0].DueDate != nil {
		t.Error("null owner/due_date 가 nil 로 오지 않았다")
	}
	if got.Content.Todos[1].Owner != nil {
		t.Error("공백만 있는 owner 는 없음으로 통일해야 한다")
	}
	if got.Content.Todos[1].DueDate == nil || *got.Content.Todos[1].DueDate != "2026-10-01" {
		t.Errorf("due_date=%v — 밑줄 필드가 붙지 않으면 화면에 기한이 안 뜬다", got.Content.Todos[1].DueDate)
	}
	// 🔴 nil 슬라이스 정규화: 모델이 통째로 빼먹은 배열들.
	if got.Content.Decisions == nil {
		t.Error("Decisions 가 nil 이다")
	}
	cs := got.Content.Consulting
	for name, v := range map[string][]string{
		"customer_needs": cs.CustomerNeeds, "questions": cs.Questions, "concerns": cs.Concerns,
		"objections": cs.Objections, "important_points": cs.ImportantPoints, "followups": cs.Followups,
	} {
		if v == nil {
			t.Errorf("consulting.%s 가 nil 이다", name)
		}
	}
}

func TestAnalyzeThinkingDisabledWhenNoBudget(t *testing.T) {
	f := &fakeStudio{t: t, chatResponse: sampleChatResponse}
	_, c := startFake(t, f)
	a := &alibabaAnalyzer{c: c, model: defaultLLMModel, thinkingBudget: 0, maxTokens: 2048}
	if _, err := a.Analyze(context.Background(), testTranscript()); err != nil {
		t.Fatalf("Analyze 실패: %v", err)
	}
	if f.chatBody["enable_thinking"] != false {
		t.Errorf("enable_thinking=%v — 예산이 없으면 꺼야 한다", f.chatBody["enable_thinking"])
	}
	if _, ok := f.chatBody["thinking_budget"]; ok {
		t.Error("thinking_budget 과 enable_thinking 을 같이 보내면 안 된다")
	}
	if f.chatBody["max_tokens"] != float64(2048) {
		t.Errorf("max_tokens=%v", f.chatBody["max_tokens"])
	}
}

func TestAnalyzeFinishReasonLengthIsError(t *testing.T) {
	// 🔴 JSON 이 우연히 파싱되더라도 잘린 분석을 저장하면 안 된다.
	resp := `{"id":"chatcmpl-x","model":"m","choices":[{"finish_reason":"length","message":{"content":"{\"summary\":\"반쪽\"}"}}],"usage":{}}`
	f := &fakeStudio{t: t, chatResponse: resp}
	_, c := startFake(t, f)
	a := &alibabaAnalyzer{c: c, model: defaultLLMModel, maxTokens: 10}
	_, err := a.Analyze(context.Background(), testTranscript())
	if err == nil {
		t.Fatal("잘린 출력인데 성공했다")
	}
	if CodeOf(err) != "OutputTruncated" || KindOf(err) != KindRetryable {
		t.Errorf("분류가 틀렸다: %v", err)
	}
}

func TestBuildUserMessageTruncates(t *testing.T) {
	long := strings.Repeat("한", 5000) // 15000 바이트
	msg := buildUserMessage(Transcript{Text: long}, 1000)
	if len(msg) > 1000 {
		t.Errorf("상한을 넘었다: %d바이트", len(msg))
	}
	if !strings.HasSuffix(msg, promptTruncNotice) {
		t.Error("🔴 잘렸다는 안내가 붙지 않았다 — 모델이 뒷부분을 상상한다")
	}
	for _, r := range msg {
		if r == '�' {
			t.Fatal("rune 중간에서 잘렸다")
		}
	}
}

// ---------- 설정 ----------

func TestNormalizeBaseURL(t *testing.T) {
	// 🔴 인프라에 경로 suffix 가 붙은 값이 준비돼 있었다. 호스트만 남아야 한다.
	for _, in := range []string{
		"example.com",
		"  example.com  ",
		"https://example.com",
		"https://example.com/",
		"https://example.com/compatible-mode/v1",
		"http://example.com",
		"https://example.com/api/v1?x=1#f",
	} {
		got, err := normalizeBaseURL(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != "https://example.com" {
			t.Errorf("%q → %q, 기대 https://example.com", in, got)
		}
	}
	for _, in := range []string{"", "   ", "https://", "https:///path"} {
		if _, err := normalizeBaseURL(in); err == nil {
			t.Errorf("%q: 에러가 나야 한다", in)
		}
	}
}

func TestConstructorsValidateConfig(t *testing.T) {
	if _, err := newAlibabaTranscriber(Config{BaseURL: "example.com"}); err == nil {
		t.Error("API 키가 없는데 생성됐다")
	}
	if _, err := newAlibabaAnalyzer(Config{APIKey: "k"}); err == nil {
		t.Error("BaseURL 이 없는데 생성됐다")
	}
	// 스킴 없는 호스트가 기본 형태다.
	tr, err := newAlibabaTranscriber(Config{APIKey: "k", BaseURL: "ws-abc.ap-southeast-1.maas.aliyuncs.com"})
	if err != nil {
		t.Fatalf("스킴 없는 호스트가 거부됐다: %v", err)
	}
	at := tr.(*alibabaTranscriber)
	if at.c.baseURL != "https://ws-abc.ap-southeast-1.maas.aliyuncs.com" {
		t.Errorf("baseURL=%q", at.c.baseURL)
	}
	if at.model != defaultASRModel {
		t.Errorf("기본 ASR 모델=%q", at.model)
	}
	an, err := newAlibabaAnalyzer(Config{APIKey: "k", BaseURL: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if m := an.(*alibabaAnalyzer).model; m != defaultLLMModel {
		t.Errorf("기본 LLM 모델=%q", m)
	}
	// 🔴 날짜 스냅샷 모델명을 기본값으로 쓰면 RPM 이 60 으로 떨어진다.
	if strings.Count(defaultLLMModel, "-") > 1 {
		t.Errorf("기본 LLM 모델 %q 이 스냅샷처럼 보인다", defaultLLMModel)
	}
}

func TestAudioExt(t *testing.T) {
	cases := []struct {
		in   Audio
		want string
	}{
		{Audio{FileName: "call.m4a"}, ".m4a"},
		{Audio{FileName: "call.WAV"}, ".wav"},
		{Audio{FileName: "통화 녹음", ContentType: "audio/mpeg"}, ".mp3"},
		{Audio{FileName: "no-ext", ContentType: "audio/wav; codecs=1"}, ".wav"},
		{Audio{}, ".m4a"},
		{Audio{FileName: "weird.verylongext"}, ".m4a"},
	}
	for _, tc := range cases {
		if got := audioExt(tc.in); got != tc.want {
			t.Errorf("%+v → %q, 기대 %q", tc.in, got, tc.want)
		}
	}
}

func TestEmbeddedSchemaIsStrictSafe(t *testing.T) {
	// 🔴 strict 모드는 모든 object 에 additionalProperties:false 와, 모든 프로퍼티가
	// required 에 들어 있을 것을 요구한다. 하나라도 빠지면 400 이다.
	var schema map[string]any
	if err := json.Unmarshal(callAnalysisSchema, &schema); err != nil {
		t.Fatalf("스키마가 JSON 이 아니다: %v", err)
	}
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		if node["type"] == "object" {
			if node["additionalProperties"] != false {
				t.Errorf("%s: additionalProperties:false 가 없다", path)
			}
			props, _ := node["properties"].(map[string]any)
			req, _ := node["required"].([]any)
			if len(props) != len(req) {
				t.Errorf("%s: properties %d개, required %d개", path, len(props), len(req))
			}
			for k, v := range props {
				child, _ := v.(map[string]any)
				if child == nil {
					continue
				}
				if _, ok := child["description"]; !ok && child["type"] != "array" {
					t.Errorf("%s.%s: description 이 없다", path, k)
				}
				walk(path+"."+k, child)
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(path+"[]", items)
		}
	}
	walk("$", schema)
	if !strings.Contains(string(callAnalysisSchema), "YYYY-MM-DD") {
		t.Error("due_date 설명에 형식이 없다")
	}
}

// ── 데드라인 분류 회귀 테스트 ──────────────────────────────────────────────────
//
// 2026-09-18, 25분짜리 통화가 여기서 깨졌다. http.Client.Timeout(30초)이 끊은 에러가
// context.DeadlineExceeded 를 만족하는 바람에 「호출부가 취소했다」로 읽혔고, 호출부는
// 그것을 재시도 불가로 보아 통화를 ANALYSIS_FAILED 로 확정했다. 종료 상태는 스윕 대상이
// 아니라 그 통화는 사람이 손대기 전까지 영원히 멈춰 있었다.

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "fake timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestTransportErrorSeparatesWhoseDeadlineExpired(t *testing.T) {
	alive := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("호출부 ctx 가 죽었으면 공급자 실패가 아니다", func(t *testing.T) {
		err := transportError(dead, context.Canceled, time.Second)
		var e *Error
		if errors.As(err, &e) {
			// 감싸면 호출부가 「예산 소진」과 「공급자 실패」를 구분할 수 없게 된다.
			t.Fatalf("호출부 취소를 공급자 에러로 감쌌다: %v", e)
		}
	})

	t.Run("ctx 는 살아 있는데 타임아웃이면 진짜 공급자 실패다", func(t *testing.T) {
		for name, in := range map[string]error{
			"deadline": context.DeadlineExceeded,
			"net":      fakeTimeoutErr{},
		} {
			err := transportError(alive, in, 30*time.Second)
			if !Retryable(err) {
				t.Fatalf("%s: 공급자 타임아웃이 재시도 불가로 분류됐다 — 통화가 버려진다: %v", name, err)
			}
			if CodeOf(err) != CodeProviderTimeout {
				t.Fatalf("%s: code=%s (기대 %s)", name, CodeOf(err), CodeProviderTimeout)
			}
			if KindOf(err) != KindRetryable {
				t.Fatalf("%s: kind=%v", name, KindOf(err))
			}
		}
	})
}

// 🔴 분류(*Error)가 ctx 검사보다 먼저다. 순서가 뒤집히면 KindRetryable 로 감싼 공급자
// 타임아웃이 안에 든 context.DeadlineExceeded 때문에 재시도 불가로 뒤집힌다.
func TestRetryableRespectsWrappedKindOverContextError(t *testing.T) {
	err := &Error{Kind: KindRetryable, Code: CodeProviderTimeout, Message: "느리다", Err: context.DeadlineExceeded}
	if !Retryable(err) {
		t.Fatal("감싼 Kind 가 무시되고 ctx 에러가 이겼다")
	}
	perm := &Error{Kind: KindPermanent, Code: "InvalidApiKey"}
	if Retryable(perm) {
		t.Fatal("영구 실패를 재시도 대상으로 봤다")
	}
	// 맨 ctx 에러는 여전히 「공급자 실패가 아니다」 — 호출부가 판단할 몫이다.
	if Retryable(context.DeadlineExceeded) {
		t.Fatal("맨 ctx 에러를 공급자 실패로 봤다")
	}
}

// 🔴 진단 정보가 남아야 한다. 사고 당일 로그에는 code=RETRYABLE kind=RETRYABLE 뿐이라
// 원인을 알아내려면 소스를 거꾸로 읽어야 했다.
func TestErrorAccessorsExposeDiagnostics(t *testing.T) {
	err := &Error{Kind: KindRetryable, Code: "Throttling.RateQuota", Status: 429, RequestID: "req-1", Message: "쿼터 초과"}
	if StatusOf(err) != 429 || MessageOf(err) != "쿼터 초과" || RequestIDOf(err) != "req-1" {
		t.Fatalf("진단 정보가 꺼내지지 않는다: status=%d msg=%q req=%q", StatusOf(err), MessageOf(err), RequestIDOf(err))
	}
	if StatusOf(context.DeadlineExceeded) != 0 || MessageOf(context.DeadlineExceeded) != "" {
		t.Fatal("callai.Error 가 아닌 에러에서 없는 값을 지어냈다")
	}
}
