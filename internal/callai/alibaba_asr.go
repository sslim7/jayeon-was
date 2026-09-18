package callai

// alibaba_asr.go 는 Model Studio 의 **비동기 파일 전사**를 감싼다.
//
// 흐름은 세 단계다: 업로드 정책 받기 → OSS 에 오디오 올리기 → 전사 작업 제출.
// 그 뒤로는 Poll 이 작업 상태를 확인하고, 끝났으면 결과 JSON 을 그 자리에서 회수한다.
//
// 🔴 재시도 루프는 여기 없다. 재시도는 백오프를 아는 internal/calls 상태 기계가 하고,
// 이 파일은 분류된 *Error 만 돌려준다. 여기에 루프를 넣으면 Cloud Run 요청 하나가
// 재시도 시간만큼 붙잡혀 있게 된다.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// 🔴 전사 결과 본문 상한. 공급자가 주는 결과 JSON 은 단어 타임스탬프까지 들어 있어
// 통화 길이에 비례해 커진다. 상한 없이 읽으면 1Gi Cloud Run 인스턴스가 그 요청 하나로 죽는다.
const maxTranscriptionBytes = 16 << 20

type alibabaTranscriber struct {
	c     *alibabaClient
	model string
}

// uploadPolicy 는 getPolicy 응답의 data 부분이다.
// 🔴 필드가 하나라도 비면 업로드가 조용히 403 으로 끝나므로, 여기서 먼저 확인하고 끝낸다.
type uploadPolicy struct {
	Policy          string          `json:"policy"`
	Signature       string          `json:"signature"`
	UploadDir       string          `json:"upload_dir"`
	UploadHost      string          `json:"upload_host"`
	OSSAccessKeyID  string          `json:"oss_access_key_id"`
	ObjectACL       string          `json:"x_oss_object_acl"`
	ForbidOverwrite json.RawMessage `json:"x_oss_forbid_overwrite"`
	ExpireInSeconds int64           `json:"expire_in_seconds"`
	MaxFileSizeMB   int64           `json:"max_file_size_mb"`
}

func (t *alibabaTranscriber) Start(ctx context.Context, in Audio) (TranscribeState, error) {
	if in.Open == nil {
		return TranscribeState{}, &Error{Kind: KindPermanent, Code: "NoAudio", Message: "오디오를 여는 함수가 없다"}
	}
	pol, policyReqID, err := t.c.uploadPolicy(ctx, t.model)
	if err != nil {
		return TranscribeState{}, err
	}
	if pol.MaxFileSizeMB > 0 && in.Size > pol.MaxFileSizeMB<<20 {
		// 올려 봐야 거절당한다. 업로드 대역폭을 쓰기 전에 끝낸다.
		return TranscribeState{}, &Error{
			Kind: KindPermanent, Code: "AudioTooLarge", RequestID: policyReqID,
			Message: fmt.Sprintf("오디오가 공급자 상한 %dMB 를 넘었다", pol.MaxFileSizeMB),
		}
	}
	key := strings.TrimSuffix(pol.UploadDir, "/") + "/" + uploadFileName(in)
	if err := t.c.uploadAudio(ctx, pol, key, in); err != nil {
		return TranscribeState{}, err
	}
	// 🔴 공개 URL 을 만들 필요가 없다. oss:// 참조를 그대로 넘기고,
	// 제출 요청에 X-DashScope-OssResourceResolve 헤더를 붙여 공급자가 풀게 한다.
	taskID, reqID, err := t.c.submitTranscription(ctx, t.model, "oss://"+key, in)
	if err != nil {
		return TranscribeState{}, err
	}
	return TranscribeState{Status: StatusRunning, Token: taskID, Model: t.model, RequestID: reqID}, nil
}

// ---------- 1단계: 업로드 정책 ----------

func (c *alibabaClient) uploadPolicy(ctx context.Context, model string) (*uploadPolicy, string, error) {
	p := "/api/v1/uploads?action=getPolicy&model=" + url.QueryEscape(model)
	req, err := c.newRequest(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		RequestID string       `json:"request_id"`
		Data      uploadPolicy `json:"data"`
	}
	if err := c.doJSON(req, &resp); err != nil {
		return nil, "", err
	}
	pol := resp.Data
	var missing []string
	for _, f := range []struct {
		name, val string
	}{
		{"policy", pol.Policy},
		{"signature", pol.Signature},
		{"upload_dir", pol.UploadDir},
		{"upload_host", pol.UploadHost},
		{"oss_access_key_id", pol.OSSAccessKeyID},
	} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, resp.RequestID, &Error{
			Kind: KindPermanent, Code: "UploadPolicyIncomplete", RequestID: resp.RequestID,
			Message: "업로드 정책에 " + strings.Join(missing, ",") + " 가 없다",
		}
	}
	// upload_host 도 스킴 없이 오는 경우가 있다.
	host, err := normalizeBaseURL(pol.UploadHost)
	if err != nil {
		return nil, resp.RequestID, &Error{Kind: KindPermanent, Code: "UploadPolicyIncomplete", RequestID: resp.RequestID, Message: "upload_host 를 해석하지 못했다"}
	}
	pol.UploadHost = host
	return &pol, resp.RequestID, nil
}

// contentTypeExt 는 확장자가 없는 파일명을 구제한다. 공급자가 확장자로 포맷을 판별하므로
// 확장자를 잃으면 멀쩡한 오디오가 거절당한다.
var contentTypeExt = map[string]string{
	"audio/mp4":    ".m4a",
	"audio/m4a":    ".m4a",
	"audio/x-m4a":  ".m4a",
	"audio/aac":    ".aac",
	"audio/mpeg":   ".mp3",
	"audio/mp3":    ".mp3",
	"audio/wav":    ".wav",
	"audio/x-wav":  ".wav",
	"audio/wave":   ".wav",
	"audio/ogg":    ".ogg",
	"audio/opus":   ".opus",
	"audio/webm":   ".webm",
	"audio/flac":   ".flac",
	"audio/x-flac": ".flac",
	"audio/amr":    ".amr",
	"video/mp4":    ".mp4",
}

// uploadFileName 은 충돌하지 않는 오브젝트 이름을 만든다.
//
// 🔴 사용자 파일명을 그대로 쓰지 않는다. 한글·공백·경로 구분자가 섞이면 서명 계산과
// key 가 어긋나고, 같은 이름이 겹치면 x-oss-forbid-overwrite 때문에 업로드가 실패한다.
// 확장자만 살려서 넘긴다.
func uploadFileName(in Audio) string {
	ext := audioExt(in)
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 실패는 사실상 일어나지 않는다. 여기서 통화를 버리는 대신 시간으로 물러선다.
		return fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	}
	return hex.EncodeToString(b[:]) + ext
}

func audioExt(in Audio) string {
	if e := path.Ext(in.FileName); validExt(e) {
		return strings.ToLower(e)
	}
	ct := in.ContentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if e, ok := contentTypeExt[strings.ToLower(strings.TrimSpace(ct))]; ok {
		return e
	}
	// 앱이 올리는 통화 녹음 기본 포맷.
	return ".m4a"
}

func validExt(e string) bool {
	if len(e) < 2 || len(e) > 6 || e[0] != '.' {
		return false
	}
	for _, r := range e[1:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// ---------- 2단계: OSS 업로드 ----------

// uploadAudio 는 오디오를 스트리밍으로 올린다.
//
// 🔴 io.Pipe + multipart.Writer 로 **읽는 즉시 흘려보낸다.** 1시간짜리 통화를 메모리에
// 통째로 올리면 Cloud Run 인스턴스(1Gi)가 그 요청 하나 때문에 죽는다.
// Content-Length 를 못 붙이지만 chunked 로 보내면 OSS 가 받아 준다.
func (c *alibabaClient) uploadAudio(ctx context.Context, p *uploadPolicy, key string, in Audio) error {
	rc, err := in.Open(ctx)
	if err != nil {
		// 우리 스토리지에서 오디오를 못 읽었다는 뜻이다. 폴링을 이어갈 게 아니라 처음부터 다시다.
		return &Error{Kind: KindInputUnavailable, Code: "AudioOpenFailed", Message: "오디오를 열지 못했다", Err: err}
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	// 경계 문자열은 고루틴을 띄우기 **전에** 읽는다. multipart.Writer 는 동시 사용을
	// 전제로 만들어진 타입이 아니다.
	contentType := mw.FormDataContentType()
	go func() {
		defer rc.Close()
		// CloseWithError(nil) 은 읽는 쪽에 EOF 를 준다. 에러면 그 에러가 Do 에서 그대로 올라온다.
		pw.CloseWithError(writeUploadForm(mw, p, key, in, rc))
	}()
	// Do 가 실패해도 고루틴이 남지 않도록 읽는 쪽을 닫아 쓰기를 깨운다.
	defer pr.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.UploadHost, pr)
	if err != nil {
		return &Error{Kind: KindPermanent, Code: "BadUploadURL", Message: "업로드 URL 을 만들지 못했다", Err: err}
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.upload.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := readLimited(resp.Body, maxErrorBodyBytes)
		e := newAPIError(resp.StatusCode, headerRequestID(resp), body)
		// 🔴 업로드 실패는 분류와 무관하게 재시도 대상으로 본다.
		// 이 단계의 자격 증명은 방금 받은 5분짜리 정책이고, Start 를 다시 부르면 새 정책과
		// 새 오브젝트 키로 처음부터 다시 시도된다 — 정책 만료(403), 키 충돌(409),
		// 서명 불일치 모두 다음 시도에서 사라진다. API 키 자체가 죽은 경우는 이 단계에
		// 오기 전 getPolicy 가 401 로 먼저 끝낸다.
		e.Kind = KindRetryable
		return e
	}
	// 연결 재사용을 위해 남은 본문을 비운다(success_action_status=200 이면 보통 비어 있다).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	return nil
}

// writeUploadForm 은 OSS PostObject 폼을 만든다.
// 🔴 **필드 순서가 중요하다.** 파일 파트(file)는 반드시 맨 마지막이어야 하고,
// 정책·서명 필드가 그보다 앞에 와야 OSS 가 본문을 다 받기 전에 검증할 수 있다.
func writeUploadForm(mw *multipart.Writer, p *uploadPolicy, key string, in Audio, r io.Reader) error {
	fields := [][2]string{
		{"OSSAccessKeyId", p.OSSAccessKeyID},
		{"policy", p.Policy},
		{"signature", p.Signature},
		{"key", key},
		{"x-oss-object-acl", p.ObjectACL},
		{"x-oss-forbid-overwrite", rawToString(p.ForbidOverwrite)},
		{"success_action_status", "200"},
		{"x-oss-content-type", strings.TrimSpace(in.ContentType)},
	}
	for _, f := range fields {
		if f[1] == "" {
			// 정책이 주지 않은 선택 필드를 빈 값으로 보내면 서명 검증이 어긋난다.
			continue
		}
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile("file", path.Base(key))
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, r); err != nil {
		return err
	}
	return mw.Close()
}

// ---------- 3단계: 전사 제출 ----------

func (c *alibabaClient) submitTranscription(ctx context.Context, model, ossURL string, in Audio) (taskID, requestID string, err error) {
	lang := strings.TrimSpace(in.Language)
	if lang == "" {
		lang = "ko"
	}
	params := map[string]any{"language_hints": []string{lang}}
	if in.SpeakerCount >= 2 {
		params["diarization_enabled"] = true
		params["speaker_count"] = in.SpeakerCount
	}
	// ❌ enable_words / enable_itn 은 보내지 마라.
	// 대조 실험으로 확인했다 — 이 모델은 두 파라미터를 받고도 **조용히 무시한다**.
	// 200 이 떨어지고 결과도 정상이라 먹힌 것처럼 보이지만 출력에 차이가 없다.
	// 보내 봐야 「켜 뒀다」는 잘못된 믿음만 코드에 남는다.
	body, merr := json.Marshal(map[string]any{
		"model":      model,
		"input":      map[string]any{"file_urls": []string{ossURL}},
		"parameters": params,
	})
	if merr != nil {
		return "", "", &Error{Kind: KindPermanent, Code: "BadRequestBody", Message: "전사 요청을 만들지 못했다", Err: merr}
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/services/audio/asr/transcription", body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("X-DashScope-Async", "enable")
	// 🔴 이 헤더가 빠지면 oss:// 참조를 풀지 못해 제출이 실패한다.
	req.Header.Set("X-DashScope-OssResourceResolve", "enable")

	var resp struct {
		RequestID string `json:"request_id"`
		Output    struct {
			TaskID     string `json:"task_id"`
			TaskStatus string `json:"task_status"`
		} `json:"output"`
	}
	if err := c.doJSON(req, &resp); err != nil {
		return "", "", err
	}
	if resp.Output.TaskID == "" {
		return "", resp.RequestID, &Error{Kind: KindRetryable, Code: "NoTaskID", RequestID: resp.RequestID, Message: "전사 작업 ID 를 받지 못했다"}
	}
	return resp.Output.TaskID, resp.RequestID, nil
}

// ---------- 4단계: 폴링 ----------

type asrTaskResponse struct {
	RequestID string `json:"request_id"`
	Output    struct {
		TaskID     string          `json:"task_id"`
		TaskStatus string          `json:"task_status"`
		Code       json.RawMessage `json:"code"`
		Message    string          `json:"message"`
		Results    []struct {
			FileURL          string          `json:"file_url"`
			TranscriptionURL string          `json:"transcription_url"`
			SubtaskStatus    string          `json:"subtask_status"`
			Code             json.RawMessage `json:"code"`
			Message          string          `json:"message"`
		} `json:"results"`
	} `json:"output"`
	Usage struct {
		Duration float64 `json:"duration"`
	} `json:"usage"`
}

func (t *alibabaTranscriber) Poll(ctx context.Context, token string) (TranscribeState, error) {
	if strings.TrimSpace(token) == "" {
		return TranscribeState{}, &Error{Kind: KindPermanent, Code: "NoTaskID", Message: "전사 작업 토큰이 비어 있다"}
	}
	req, err := t.c.newRequest(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(token), nil)
	if err != nil {
		return TranscribeState{}, err
	}
	var resp asrTaskResponse
	if err := t.c.doJSON(req, &resp); err != nil {
		return TranscribeState{}, err
	}
	st := TranscribeState{
		Status:    StatusRunning,
		Token:     token,
		Model:     t.model,
		RequestID: resp.RequestID,
		Usage:     Usage{AudioSeconds: resp.Usage.Duration},
	}

	switch strings.ToUpper(strings.TrimSpace(resp.Output.TaskStatus)) {
	case "FAILED", "CANCELED", "CANCELLED":
		code := rawToString(resp.Output.Code)
		msg := resp.Output.Message
		if code == "" && len(resp.Output.Results) > 0 {
			// 작업은 실패인데 사유가 서브태스크에만 있는 경우가 있다.
			code = rawToString(resp.Output.Results[0].Code)
			if msg == "" {
				msg = resp.Output.Results[0].Message
			}
		}
		if msg == "" {
			msg = "전사 작업이 실패했다"
		}
		// HTTP 는 200 이므로 status 0 으로 넘겨 코드 문자열만으로 분류되게 한다.
		return TranscribeState{}, &Error{
			Kind: classifyHTTP(0, code), Code: code, RequestID: resp.RequestID,
			Message: firstRunes(strings.TrimSpace(msg), maxErrorMessageRunes),
		}
	case "SUCCEEDED":
		tr, err := t.c.fetchTranscript(ctx, resp)
		if err != nil {
			return TranscribeState{}, err
		}
		st.Status = StatusDone
		st.Transcript = tr
		return st, nil
	default:
		// 🔴 PENDING/RUNNING 은 물론 **모르는 값도 진행 중으로 본다.**
		// 공급자가 새 상태를 추가했을 때(예: QUEUING) 멀쩡히 돌아가는 작업을
		// 우리가 실패로 확정해 버리면 통화를 잃는다. 진행 중으로 두면 최악이라도
		// 호출부의 폴링 상한에 걸려 멈출 뿐이다.
		return st, nil
	}
}

// fetchTranscript 는 결과 JSON 을 **그 자리에서** 회수한다.
//
// 🔴 transcription_url 은 24시간만 유효한 서명 URL 이다. URL 을 저장해 두고 나중에 읽는
// 구조로 만들면, 작업 문서가 하루 넘게 방치된 순간 전사 결과가 영영 사라진다.
// 폴링이 SUCCEEDED 를 본 바로 그 호출에서 본문까지 받아 우리 DTO 로 바꾼다.
func (c *alibabaClient) fetchTranscript(ctx context.Context, task asrTaskResponse) (*Transcript, error) {
	var target string
	for _, r := range task.Output.Results {
		if strings.TrimSpace(r.TranscriptionURL) != "" {
			target = r.TranscriptionURL
			break
		}
	}
	if target == "" {
		// 성공인데 결과가 없다. 서브태스크 실패 사유가 있으면 그것으로 분류한다.
		code, msg := "NoTranscriptionResult", "전사 결과 URL 이 없다"
		for _, r := range task.Output.Results {
			if c := rawToString(r.Code); c != "" {
				code, msg = c, r.Message
				break
			}
		}
		return nil, &Error{Kind: classifyHTTP(0, code), Code: code, RequestID: task.RequestID, Message: firstRunes(msg, maxErrorMessageRunes)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &Error{Kind: KindPermanent, Code: "BadTranscriptionURL", RequestID: task.RequestID, Message: "전사 결과 URL 을 해석하지 못했다", Err: err}
	}
	// 서명이 URL 에 들어 있다. Authorization 헤더를 붙이면 오히려 거절당한다.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := readLimited(resp.Body, maxErrorBodyBytes)
		e := newAPIError(resp.StatusCode, headerRequestID(resp), body)
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			// 서명이 만료됐거나 오브젝트가 사라졌다 = 이 작업 ID 로는 더 얻을 게 없다.
			// 폴링을 이어가지 말고 처음부터 다시 올리라고 알린다.
			e.Kind = KindInputUnavailable
		}
		return nil, e
	}
	body, rerr := readLimited(resp.Body, maxTranscriptionBytes)
	if rerr != nil {
		if errors.Is(rerr, errBodyTooLarge) {
			return nil, &Error{Kind: KindPermanent, Code: "TranscriptTooLarge", RequestID: task.RequestID, Message: "전사 결과가 상한을 넘었다"}
		}
		return nil, transportError(rerr)
	}
	return parseTranscription(body, task.RequestID)
}

// ---------- 5단계: 결과 스키마 → 우리 DTO ----------

type asrDocument struct {
	Transcripts []struct {
		Text      string `json:"text"`
		Language  string `json:"language"`
		Sentences []struct {
			BeginTime float64         `json:"begin_time"`
			EndTime   float64         `json:"end_time"`
			Text      string          `json:"text"`
			SpeakerID json.RawMessage `json:"speaker_id"`
		} `json:"sentences"`
	} `json:"transcripts"`
}

func parseTranscription(body []byte, requestID string) (*Transcript, error) {
	var doc asrDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, &Error{Kind: KindPermanent, Code: "BadTranscriptionJSON", RequestID: requestID, Message: "전사 결과를 해석하지 못했다", Err: err}
	}
	// 🔴 Segments 는 nil 이 아니라 빈 슬라이스여야 한다 — 저장 쪽이 nil 을 거부한다.
	out := &Transcript{Segments: []Segment{}}
	if len(doc.Transcripts) == 0 {
		return out, nil
	}
	first := doc.Transcripts[0]
	out.Text = first.Text
	out.Language = first.Language
	for _, s := range first.Sentences {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			// 저장 쪽 검증이 빈 텍스트 세그먼트를 거부한다. 정보도 없으니 버린다.
			continue
		}
		seg := Segment{
			Start:   s.BeginTime / 1000, // 공급자는 ms, 우리 DTO 는 초.
			End:     s.EndTime / 1000,
			Text:    text,
			Speaker: rawToString(s.SpeakerID),
		}
		if seg.Start < 0 {
			seg.Start = 0
		}
		if seg.End < seg.Start {
			// 저장 쪽이 End < Start 인 세그먼트를 거부한다. 통화 하나를 통째로 버리느니 보정한다.
			seg.End = seg.Start
		}
		out.Segments = append(out.Segments, seg)
	}
	// 저장 쪽 검증이 Start 오름차순을 요구한다. 화자 분리가 켜지면 채널별로 섞여 오는 일이 있다.
	sort.SliceStable(out.Segments, func(i, j int) bool { return out.Segments[i].Start < out.Segments[j].Start })
	return out, nil
}
