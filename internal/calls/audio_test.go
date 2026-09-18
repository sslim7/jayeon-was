package calls

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/callai"
)

// 6. 🔴 남의 통화 ID 로는 아무것도 못 한다. **403 이 아니라 404** 다 — 403 은 「그 ID 가
// 존재한다」를 알려 주는 것과 같고, 통화 ID 는 앱이 정하는 값이라 남의 것을 찍어 볼 수 있다.
func TestAudioOwnerIsolation(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/calls/call-1/audio/upload-url", uploadBody},
		{"POST", "/calls/call-1/audio/complete", ""},
		{"POST", "/calls/call-1/reanalyze", ""},
		{"GET", "/calls/call-1", ""},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			w := h.do(t, c.method, c.path, "owner-b", c.body)
			if w.Code != 404 {
				t.Fatalf("남의 통화에 %d 가 나왔다: %s", w.Code, w.Body.String())
			}
		})
	}
	// 🔴 그리고 남의 작업 문서가 덮어써지지 않았는지 확인한다. upload-url 이 「없으면 만든다」
	// 이므로, 소유권 확인을 빠뜨리면 남의 통화를 통째로 가로챌 수 있다.
	if j := h.job(t, "call-1"); j.UID != "owner-a" || j.State != stateCompleted {
		t.Fatalf("작업 문서가 탈취됐다: uid=%s state=%s", j.UID, j.State)
	}
}

// 인증 없는 요청은 저장소에 닿기 전에 막힌다.
func TestAudioRequiresAuth(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	if w := h.do(t, "POST", "/calls/call-1/audio/upload-url", "", uploadBody); w.Code != 401 {
		t.Fatalf("미인증 요청이 %d", w.Code)
	}
}

func TestUploadURLValidation(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	for _, c := range []struct {
		name, body string
	}{
		{"형식 화이트리스트 밖", `{"content_type":"application/zip","size":10,"contact":{"name":"김고객","phone":"021234567"},"call":{"file_name":"a.zip","duration":null,"recorded_at":"2026-09-18T14:00:00+09:00"}}`},
		{"크기 0", `{"content_type":"audio/m4a","size":0,"contact":{"name":"김고객","phone":"021234567"},"call":{"file_name":"a.m4a","duration":null,"recorded_at":"2026-09-18T14:00:00+09:00"}}`},
		{"크기 상한 초과", `{"content_type":"audio/m4a","size":209715200,"contact":{"name":"김고객","phone":"021234567"},"call":{"file_name":"a.m4a","duration":null,"recorded_at":"2026-09-18T14:00:00+09:00"}}`},
		{"전화번호 형식", `{"content_type":"audio/m4a","size":10,"contact":{"name":"김고객","phone":"전화"},"call":{"file_name":"a.m4a","duration":null,"recorded_at":"2026-09-18T14:00:00+09:00"}}`},
		{"먼 미래 시각", `{"content_type":"audio/m4a","size":10,"contact":{"name":"김고객","phone":"021234567"},"call":{"file_name":"a.m4a","duration":null,"recorded_at":"2099-01-01T00:00:00+09:00"}}`},
		{"경로 주입 recipient_id", `{"content_type":"audio/m4a","size":10,"contact":{"name":"김고객","phone":"021234567","recipient_id":"../other"},"call":{"file_name":"a.m4a","duration":null,"recorded_at":"2026-09-18T14:00:00+09:00"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if w := h.do(t, "POST", "/calls/call-x/audio/upload-url", "owner-a", c.body); w.Code != 400 {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// 🔴 객체 경로는 결정적이어야 한다. 랜덤 이름이면 업로드를 재시도할 때마다 고아 객체가
// 생기고 366일 내내 요금이 나간다.
func TestUploadURLIsRepeatableBeforeComplete(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	first := decodeUpload(t, h, "owner-a", "call-1")
	second := decodeUpload(t, h, "owner-a", "call-1")
	if first.Object != second.Object {
		t.Fatalf("객체 경로가 매번 달라진다: %s vs %s", first.Object, second.Object)
	}
	if !strings.HasPrefix(first.Object, "calls/owner-a/call-1/audio") {
		t.Fatalf("객체 경로: %s", first.Object)
	}
	// 앱이 서명에 쓰인 Content-Type 을 그대로 보내야 한다는 사실이 응답에 드러나야 한다.
	if first.Headers["Content-Type"] != "audio/m4a" || first.Method != "PUT" {
		t.Fatalf("업로드 계약이 응답에 없다: %+v", first)
	}
	if first.MaxBytes != maxAudioBytes || first.ExpiresAt == "" {
		t.Fatalf("한도/만료가 응답에 없다: %+v", first)
	}
	exp, err := time.Parse(time.RFC3339, first.ExpiresAt)
	if err != nil || exp.Sub(h.clock()) < 30*time.Minute {
		// 100 MB 를 느린 회선으로 올리는 동안 만료되면 업로드가 조용히 401 로 죽는다.
		t.Fatalf("만료가 너무 짧다: %v (%v)", first.ExpiresAt, err)
	}

	// 큐잉된 뒤에는 오디오를 갈아 끼울 수 없다 — 전사문과 원본이 어긋난다.
	h.audio.put(first.Object, 512)
	if w := h.do(t, "POST", "/calls/call-1/audio/complete", "owner-a", ""); w.Code != 200 {
		t.Fatalf("complete: %d", w.Code)
	}
	if w := h.do(t, "POST", "/calls/call-1/audio/upload-url", "owner-a", uploadBody); w.Code != 409 {
		t.Fatalf("큐잉 후 upload-url 이 %d", w.Code)
	}
}

// complete 는 객체가 실제로 올라왔는지 확인한다. 없는 채로 큐잉하면 ASR 단계에 가서야
// 실패하고, 그때는 이미 tick 몇 번과 공급자 호출을 낭비한 뒤다.
func TestCompleteRequiresUploadedObject(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	decodeUpload(t, h, "owner-a", "call-1")
	w := h.do(t, "POST", "/calls/call-1/audio/complete", "owner-a", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "CALL_AUDIO_MISSING") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if j := h.job(t, "call-1"); j.State != stateAwaitingUpload {
		t.Fatalf("큐잉되면 안 된다: %s", j.State)
	}
}

// 🔴 audio_url 은 **상세에만** 붙고 목록에는 붙지 않는다. 그리고 저장되지 않는다.
func TestAudioURLOnlyOnDetail(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()

	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.AudioURL == nil || !strings.Contains(*got.AudioURL, "calls/owner-a/call-1/audio") {
		t.Fatalf("상세에 재생 주소가 없다: %v", got.AudioURL)
	}
	if !got.HasAudio {
		t.Fatal("has_audio 가 false 다")
	}
	// 저장 바이트에는 절대 들어가면 안 된다 — 만료되는 값이라 굳은 URL 이 남는다.
	stored, err := h.recs.Get(context.Background(), "owner-a", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AudioURL != nil {
		t.Fatal("서명 URL 이 Firestore 에 저장됐다")
	}

	// 보관 기간이 지나 원본이 사라진 상황: audio_url 은 없지만 **실패가 아니다.**
	j := h.job(t, "call-1")
	j.Audio.Object = ""
	if err := h.jobs.Put(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	got = decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.AudioURL != nil || got.HasAudio {
		t.Fatalf("보관 만료인데 재생 주소가 있다: %v", got.AudioURL)
	}
	if got.Status != "COMPLETED" {
		t.Fatalf("보관 만료가 분석 실패로 보인다: %s", got.Status)
	}
}

// 🔴 응답 전용 필드가 PUT 요청에 들어오면 거부한다. DisallowUnknownFields 는 통과하므로
// 기기 경로 검증이 막지 않으면 계약이 조용히 넓어진다.
func TestDeviceUploadRejectsResponseOnlyFields(t *testing.T) {
	for _, field := range []string{`"stage":"받아쓰는 중"`, `"has_audio":true`, `"job_state":"QUEUED"`, `"audio_url":"https://x"`} {
		t.Run(field, func(t *testing.T) {
			b, _ := json.Marshal(fixture())
			body := strings.Replace(string(b), `"call_id"`, field+`,"call_id"`, 1)
			h := newHarness(&callai.FakeTranscriber{}, &callai.FakeAnalyzer{})
			if w := h.do(t, "PUT", "/calls/call-1", "owner-a", body); w.Code != 400 {
				t.Fatalf("응답 전용 필드를 받아들였다: %d %s", w.Code, w.Body.String())
			}
		})
	}
	// ai.provider 도 마찬가지다.
	r := fixture()
	r.AI.Provider = "alibaba"
	if validate(&r, "call-1") == nil {
		t.Fatal("ai.provider 가 들어온 기기 요청을 받아들였다")
	}
}

func decodeUpload(t *testing.T, h *harness, uid, id string) uploadURLResponse {
	t.Helper()
	w := h.do(t, "POST", "/calls/"+id+"/audio/upload-url", uid, uploadBody)
	if w.Code != 200 {
		t.Fatalf("upload-url: %d %s", w.Code, w.Body.String())
	}
	var res uploadURLResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}
