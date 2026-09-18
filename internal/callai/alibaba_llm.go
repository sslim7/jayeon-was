package callai

// alibaba_llm.go 는 Model Studio 의 **OpenAI 호환 채팅 완성**으로 전사문을 구조화 분석으로 바꾼다.
// 동기 호출 한 번이라 상태가 없다.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// 🔴 시스템 프롬프트를 Go 소스에 거대한 문자열로 박지 않는다.
// 프롬프트는 코드보다 훨씬 자주 바뀌고, 바뀔 때마다 diff 가 읽히지 않으면 무엇을 고쳤는지
// 아무도 모르게 된다. 파일로 두면 diff 가 문장 단위로 보이고 버전도 파일명에 남는다.
//
//go:embed prompts/call_analysis_v1.txt
var callAnalysisPrompt string

//go:embed prompts/call_analysis_v1.schema.json
var callAnalysisSchema []byte

// callAnalysisPromptVersion 은 저장되는 분석에 남는다.
// 프롬프트를 고칠 때는 파일을 새로 만들고 이 값을 같이 올려라 — 그래야 나중에
// 「이 요약은 어느 프롬프트가 만든 것인가」를 답할 수 있다.
const callAnalysisPromptVersion = "call_analysis_v1"

const (
	// 🔴 전사문 바이트 상한. 1시간짜리 통화 전사문은 모델 컨텍스트를 넘길 수 있고,
	// 넘기면 400 이 아니라 비싼 요청이 통째로 버려진다. 넘치면 앞부분만 보내되
	// **잘렸다는 사실을 프롬프트에 명시해** 모델이 뒷부분을 상상하지 않게 한다.
	maxPromptBytes    = 200_000
	promptTruncNotice = "\n\n(안내: 전사문이 길어 앞부분만 전달됐다. 뒤쪽에 무슨 이야기가 있었을지 상상해서 채우지 마라.)"
)

type alibabaAnalyzer struct {
	c              *alibabaClient
	model          string
	thinkingBudget int
	maxTokens      int
}

// wireAnalysis 는 모델이 내놓는 JSON 모양이다.
//
// 🔴 DTO(AnalysisContent)에 json 태그를 달아 대신 쓰지 않는다. DTO 는 공급자를 몰라야 하고,
// 무엇보다 Go 의 기본 필드 매칭은 **밑줄을 무시하지 않는다** — `due_date` 는 DueDate 에,
// `customer_needs` 는 CustomerNeeds 에 절대 붙지 않는다. 태그 없이 DTO 로 바로 언마샬하면
// 그 필드들이 조용히 비어서, 화면에 담당자와 기한이 영원히 안 뜬다.
type wireAnalysis struct {
	Summary string `json:"summary"`
	Details []struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	} `json:"details"`
	Todos []struct {
		Content string  `json:"content"`
		Owner   *string `json:"owner"`
		DueDate *string `json:"due_date"`
		Source  string  `json:"source"`
	} `json:"todos"`
	Decisions  []string `json:"decisions"`
	Consulting struct {
		CustomerNeeds   []string `json:"customer_needs"`
		Questions       []string `json:"questions"`
		Concerns        []string `json:"concerns"`
		Objections      []string `json:"objections"`
		ImportantPoints []string `json:"important_points"`
		Followups       []string `json:"followups"`
	} `json:"consulting"`
}

type chatResponse struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	Model     string `json:"model"`
	Choices   []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func (a *alibabaAnalyzer) Analyze(ctx context.Context, t Transcript) (Analysis, error) {
	body, err := a.requestBody(t)
	if err != nil {
		return Analysis{}, err
	}
	req, err := a.c.newRequest(ctx, http.MethodPost, "/compatible-mode/v1/chat/completions", body)
	if err != nil {
		return Analysis{}, err
	}
	var resp chatResponse
	if err := a.c.doJSON(req, &resp); err != nil {
		return Analysis{}, err
	}

	reqID := resp.RequestID
	if reqID == "" {
		// 호환 모드는 request_id 를 안 줄 때가 있다. 그때는 chatcmpl-... id 가 문의 단서다.
		reqID = resp.ID
	}
	model := resp.Model
	if model == "" {
		model = a.model
	}
	usage := Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		// 🔴 thinking 이 실제로 억제됐는지는 이 값으로만 확인할 수 있다. 아래 requestBody 주석 참고.
		ReasoningTokens: resp.Usage.CompletionTokensDetails.ReasoningTokens,
	}

	if len(resp.Choices) == 0 {
		return Analysis{}, &Error{Kind: KindRetryable, Code: "NoChoices", RequestID: reqID, Message: "모델이 응답을 돌려주지 않았다"}
	}
	choice := resp.Choices[0]
	if strings.EqualFold(choice.FinishReason, "length") {
		// 🔴 출력 토큰 상한에 걸려 JSON 이 중간에서 끊겼다는 뜻이다.
		// 운이 좋으면 파싱까지 성공할 수도 있지만 그건 「앞부분만 담긴 분석」이고,
		// 그게 저장되면 사용자는 요약이 왜 반쪽인지 영영 모른다. 무조건 실패로 본다.
		return Analysis{}, &Error{Kind: KindRetryable, Code: "OutputTruncated", RequestID: reqID, Message: "모델 출력이 토큰 상한에서 잘렸다"}
	}
	if strings.EqualFold(choice.FinishReason, "content_filter") {
		return Analysis{}, &Error{Kind: KindContentFiltered, Code: "DataInspectionFailed", RequestID: reqID, Message: "콘텐츠 필터가 응답을 막았다"}
	}

	content := stripJSONFence(choice.Message.Content)
	if content == "" {
		return Analysis{}, &Error{Kind: KindRetryable, Code: "EmptyContent", RequestID: reqID, Message: "모델이 빈 응답을 돌려줬다"}
	}
	var w wireAnalysis
	if err := json.Unmarshal([]byte(content), &w); err != nil {
		// 🔴 err 만 붙인다. 파싱에 실패한 본문에는 통화 내용이 들어 있다 — 로그에 남기면 안 된다.
		return Analysis{}, &Error{Kind: KindRetryable, Code: "BadAnalysisJSON", RequestID: reqID, Message: "모델 출력이 JSON 이 아니다"}
	}

	return Analysis{
		Content:       w.toContent(),
		Usage:         usage,
		Model:         model,
		PromptVersion: callAnalysisPromptVersion,
		RequestID:     reqID,
	}, nil
}

// toContent 는 wire 모양을 DTO 로 옮기면서 **nil 슬라이스를 빈 슬라이스로 정규화한다.**
// 🔴 저장 쪽 검증이 nil 을 거부한다. 모델이 `"todos": []` 를 빼먹는 것만으로
// 멀쩡한 분석이 저장 단계에서 버려지는 일을 여기서 끝낸다.
func (w wireAnalysis) toContent() AnalysisContent {
	out := AnalysisContent{
		Summary:   w.Summary,
		Details:   make([]Detail, 0, len(w.Details)),
		Todos:     make([]Todo, 0, len(w.Todos)),
		Decisions: emptyIfNil(w.Decisions),
		Consulting: Consulting{
			CustomerNeeds:   emptyIfNil(w.Consulting.CustomerNeeds),
			Questions:       emptyIfNil(w.Consulting.Questions),
			Concerns:        emptyIfNil(w.Consulting.Concerns),
			Objections:      emptyIfNil(w.Consulting.Objections),
			ImportantPoints: emptyIfNil(w.Consulting.ImportantPoints),
			Followups:       emptyIfNil(w.Consulting.Followups),
		},
	}
	for _, d := range w.Details {
		out.Details = append(out.Details, Detail{Title: d.Title, Content: d.Content})
	}
	for _, td := range w.Todos {
		// 모델이 null 대신 빈 문자열을 넣는 경우가 있다. 「담당자가 빈칸」은 화면에서
		// 「담당자 미상」과 다르게 보이므로 여기서 없음으로 통일한다.
		out.Todos = append(out.Todos, Todo{
			Content: td.Content,
			Owner:   nilIfBlank(td.Owner),
			DueDate: nilIfBlank(td.DueDate),
			Source:  td.Source,
		})
	}
	return out
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nilIfBlank(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return p
}

// stripJSONFence 는 모델이 씌운 마크다운 코드펜스를 벗긴다.
// 구조화 출력을 쓰면 나오지 않아야 정상이지만, 한 번 나오면 통화 전체가 실패하는데
// 대비 비용이 여섯 줄이라 걸어 둔다.
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

// requestBody 는 채팅 완성 요청을 만든다.
//
// 🔴 **모르는 파라미터를 이 공급자는 조용히 무시한다. 200 이 떴다고 먹힌 게 아니다.**
// 오타 하나로 thinking 이 계속 켜져 있어도 응답은 멀쩡해 보이고 요금만 는다.
// 그래서 끄는 쪽 설정을 「보냈다」로 믿지 않고, 응답의
// usage.completion_tokens_details.reasoning_tokens 를 Usage.ReasoningTokens 에 담아
// 나중에 데이터로 확인한다 — 그 값이 0 이 아니면 thinking 이 살아 있다는 뜻이다.
func (a *alibabaAnalyzer) requestBody(t Transcript) ([]byte, error) {
	req := map[string]any{
		"model": a.model,
		"messages": []map[string]string{
			// 🔴 시스템 프롬프트가 없으면 모델이 연도를 지어내고(2024/2023 같은 값을 due_date 에
			// 박는다) 요약을 영어로 쓴다. 실제로 겪은 일이라 선택이 아니라 필수다.
			{"role": "system", "content": callAnalysisPrompt},
			{"role": "user", "content": buildUserMessage(t, maxPromptBytes)},
		},
		// 🔴 max_tokens 는 **답변 토큰만 막는다 — thinking 토큰은 막지 못한다.**
		// 그래서 둘 다 건다. 하나만 걸면 새는 쪽이 반드시 생긴다.
		"max_tokens": a.maxTokens,
		// 🔴 구조화 출력은 공식 문서가 Singapore 리전 미지원이라고 적어 뒀지만 **실제로 동작한다**
		// (실호출로 확인). strict 모드라 모든 프로퍼티가 required 이고 모든 object 에
		// additionalProperties:false 가 들어가 있어야 한다 — prompts/*.schema.json 참고.
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "call_analysis",
				"strict": true,
				"schema": json.RawMessage(callAnalysisSchema),
			},
		},
	}
	// 🔴 이 공급자는 thinking 이 **기본 ON** 이다. 끄지 않으면 비용과 지연이 조용히 샌다.
	if a.thinkingBudget > 0 {
		// 예산을 주면 정확히 그만큼만 쓴다.
		req["thinking_budget"] = a.thinkingBudget
	} else {
		req["enable_thinking"] = false
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &Error{Kind: KindPermanent, Code: "BadRequestBody", Message: "분석 요청을 만들지 못했다", Err: err}
	}
	return body, nil
}

// buildUserMessage 는 전사문 본문과 화자별 발화를 한 메시지로 만든다.
func buildUserMessage(t Transcript, max int) string {
	var b strings.Builder
	b.WriteString("[전사문]\n")
	b.WriteString(strings.TrimSpace(t.Text))
	if len(t.Segments) > 0 {
		b.WriteString("\n\n[화자별 발화]\n")
		for _, s := range t.Segments {
			speaker := strings.TrimSpace(s.Speaker)
			if speaker == "" {
				speaker = "미상"
			}
			fmt.Fprintf(&b, "[%s] 화자 %s: %s\n", formatClock(s.Start), speaker, s.Text)
		}
	}
	s := b.String()
	if len(s) <= max || max <= len(promptTruncNotice) {
		return s
	}
	return truncateUTF8(s, max-len(promptTruncNotice)) + promptTruncNotice
}

// truncateUTF8 은 바이트 상한에 맞춰 자르되 rune 중간에서 끊지 않는다.
// rune 중간에서 자르면 깨진 바이트가 그대로 프롬프트에 들어가고, 그 한 글자 때문에
// 모델이 엉뚱한 언어로 답하는 일이 생긴다.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := s[:max]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}

// formatClock 은 초를 [h:]mm:ss 로 옮긴다. 모델이 시각을 근거로 인용할 수 있게 넣는다.
func formatClock(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	h, m, s := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
