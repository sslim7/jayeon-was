package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/recipients"
	"github.com/sslim7/nature-was/internal/sms"
	"github.com/sslim7/nature-was/internal/userguard"
	"github.com/sslim7/nature-was/internal/users"
)

// 실제 인증·가드·Firestore를 연결해 50명 순차 처리와 중단 후 복구를 검증한다.
// Android 전송은 호출하지 않으며 단말이 전달하는 SENT/FAILED 결과만 재현한다.
func TestSMSHTTPFiftyRecipientLifecycle(t *testing.T) {
	host := os.Getenv("FIRESTORE_EMULATOR_HOST")
	if host == "" {
		t.Skip("Firestore emulator required")
	}
	project := fmt.Sprintf("demo-sms-http-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fs, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fs.Close()
		client := &http.Client{Timeout: 10 * time.Second}
		req, e := http.NewRequest(http.MethodDelete, "http://"+host+"/emulator/v1/projects/"+project+"/databases/(default)/documents", nil)
		if e != nil {
			t.Error(e)
			return
		}
		resp, e := client.Do(req)
		if e != nil {
			t.Error(e)
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != 200 {
			t.Errorf("emulator cleanup: %d", resp.StatusCode)
		}
	})
	accounts := users.NewStore(fs)
	tokens := auth.NewTokenIssuer("sms-http-integration-test-secret-only")
	seed := func(email string, temporary bool) string {
		t.Helper()
		id, e := accounts.Create(ctx, users.User{Email: email, UserName: "통합 테스트", IsActive: true, MustChangePassword: temporary, PasswordHash: "unused-test-hash", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		if e != nil {
			t.Fatal(e)
		}
		token, e := tokens.IssueAccess(id)
		if e != nil {
			t.Fatal(e)
		}
		return token
	}
	token := seed("owner@example.test", false)
	other := seed("other@example.test", false)
	temporary := seed("temporary@example.test", true)
	mux := http.NewServeMux()
	guard := userguard.New(accounts)
	recipients.Register(mux, fs, guard)
	sms.Register(mux, fs, guard)
	handler := tokens.Middleware(mux)
	request := func(bearer, method, path string, body any, want int, out any) {
		t.Helper()
		var payload []byte
		if body != nil {
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(payload)).WithContext(ctx)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d want %d body %s", method, path, w.Code, want, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s missing no-store", path)
		}
		if out != nil {
			if e := json.Unmarshal(w.Body.Bytes(), out); e != nil {
				t.Fatalf("decode %s: %v", path, e)
			}
		}
	}
	request("", "GET", "/recipients", nil, 401, nil)
	request(temporary, "GET", "/sms/campaigns", nil, 403, nil)
	originals := make([]recipients.Recipient, 50)
	ids := make([]string, 50)
	for i := range originals {
		request(token, "POST", "/recipients", map[string]any{"name": fmt.Sprintf("수신자 %02d", i), "phone": fmt.Sprintf("010-2000-%04d", i), "groupId": "테스트"}, 201, &originals[i])
		ids[i] = originals[i].ID
	}
	var campaign sms.Campaign
	create := sms.CreateRequest{RequestID: "http-campaign-01", Title: "50명 발송 검증", Message: "안녕하세요.\n통합 테스트 문자입니다. ", RecipientIDs: ids}
	request(token, "POST", "/sms/campaigns", create, 201, &campaign)
	base := "/sms/campaigns/" + campaign.ID
	if campaign.Status != sms.Ready || campaign.RecipientCount != 50 || campaign.ReadyCount != 50 {
		t.Fatalf("initial: %+v", campaign)
	}
	var replay sms.Campaign
	request(token, "POST", "/sms/campaigns", create, 200, &replay)
	if replay.ID != campaign.ID {
		t.Fatal("idempotency created another campaign")
	}
	request(other, "GET", base, nil, 404, nil)
	request(other, "GET", base+"/recipients", nil, 404, nil)
	request(other, "PUT", "/recipients/"+ids[0], map[string]any{"name": "외부 사용자", "phone": "01033334444"}, 404, nil)
	request(token, "PUT", "/recipients/"+ids[0], map[string]any{"name": "변경된 원본", "phone": "01099998888"}, 200, nil)
	request(token, "DELETE", "/recipients/"+ids[1], nil, 204, nil)
	var snapshot struct {
		Items []sms.CampaignRecipient `json:"items"`
	}
	request(token, "GET", base+"/recipients", nil, 200, &snapshot)
	if len(snapshot.Items) != 50 {
		t.Fatal("snapshot count", len(snapshot.Items))
	}
	for i, item := range snapshot.Items {
		if item.RecipientID != ids[i] || item.Name != originals[i].Name || item.Phone != originals[i].Phone || item.Message != create.Message {
			t.Fatalf("snapshot changed or order lost at %d", i)
		}
	}
	request(token, "POST", base+"/start", nil, 200, &campaign)
	var last sms.Result
	for i, item := range snapshot.Items {
		path := base + "/recipients/" + item.ID
		attempt := fmt.Sprintf("http-attempt-%02d", i)
		claim := sms.PatchRequest{Status: sms.Sending, AttemptID: attempt}
		request(token, "PATCH", path, claim, 200, &last)
		if !last.DispatchAllowed {
			t.Fatalf("claim %d not allowed", i)
		}
		if i == 0 {
			request(token, "PATCH", path, claim, 200, &last)
			if last.DispatchAllowed {
				t.Fatal("repeated claim allowed dispatch")
			}
			request(token, "PATCH", base+"/recipients/"+snapshot.Items[1].ID, sms.PatchRequest{Status: sms.Sending, AttemptID: "concurrent-attempt"}, 409, nil)
		}
		result := sms.PatchRequest{Status: sms.Sent, AttemptID: attempt}
		if i == 49 {
			result.Status = sms.Failed
			result.ErrorCode = "RADIO_OFF"
			result.ErrorMessage = "모의 단말 오류"
		}
		request(token, "PATCH", path, result, 200, &last)
		if last.DispatchAllowed {
			t.Fatal("result permits dispatch")
		}
		if i == 0 {
			request(token, "PATCH", path, result, 200, &last)
			if last.Campaign.SentCount != 1 {
				t.Fatal("result replay changed counter")
			}
			request(token, "POST", path+"/retry", nil, 409, nil)
		}
		if i == 3 {
			request(token, "POST", base+"/cancel", nil, 200, &campaign)
			if campaign.Status != sms.Cancelled || campaign.SentCount != 4 || campaign.ReadyCount != 46 {
				t.Fatalf("cancel counts: %+v", campaign)
			}
			nextPath := base + "/recipients/" + snapshot.Items[4].ID
			request(token, "PATCH", nextPath, sms.PatchRequest{Status: sms.Sending, AttemptID: "blocked-attempt"}, 409, nil)
			request(token, "GET", base, nil, 200, &campaign)
			if campaign.Status != sms.Cancelled {
				t.Fatal("read resumed campaign")
			}
			request(token, "POST", base+"/start", nil, 200, &campaign)
			if campaign.Status != sms.Sending || campaign.SentCount != 4 {
				t.Fatalf("resume: %+v", campaign)
			}
		}
	}
	if last.Campaign.Status != sms.PartialFailed || last.Campaign.SentCount != 49 || last.Campaign.FailedCount != 1 || last.Campaign.ReadyCount != 0 || last.Campaign.SendingCount != 0 || last.Campaign.CompletedAt == nil {
		t.Fatalf("partial finish: %+v", last.Campaign)
	}
	failedPath := base + "/recipients/" + snapshot.Items[49].ID
	request(token, "POST", failedPath+"/retry", nil, 200, &last)
	if last.Recipient.Status != sms.Ready || last.Campaign.Status != sms.Ready || last.DispatchAllowed {
		t.Fatalf("retry: %+v", last)
	}
	request(token, "POST", base+"/start", nil, 200, &campaign)
	request(token, "PATCH", failedPath, sms.PatchRequest{Status: sms.Sending, AttemptID: "http-attempt-49"}, 409, nil)
	request(token, "PATCH", failedPath, sms.PatchRequest{Status: sms.Sending, AttemptID: "http-retry-49"}, 200, &last)
	if !last.DispatchAllowed || last.Recipient.AttemptCount != 2 {
		t.Fatalf("retry claim: %+v", last)
	}
	request(token, "PATCH", failedPath, sms.PatchRequest{Status: sms.Sent, AttemptID: "http-retry-49"}, 200, &last)
	request(token, "GET", base, nil, 200, &campaign)
	if campaign.Status != sms.Completed || campaign.SentCount != 50 || campaign.FailedCount != 0 || campaign.ReadyCount != 0 || campaign.SendingCount != 0 || campaign.CompletedAt == nil {
		t.Fatalf("finish: %+v", campaign)
	}
	request(token, "GET", base+"/recipients", nil, 200, &snapshot)
	for _, item := range snapshot.Items {
		if item.Status != sms.Sent || item.SentAt == nil {
			t.Fatal("missing terminal result")
		}
	}
}
