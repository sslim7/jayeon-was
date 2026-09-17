package sms

import (
	"cloud.google.com/go/firestore"
	"context"
	"errors"
	"fmt"
	"github.com/sslim7/nature-was/internal/recipients"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirestoreCampaignConcurrency(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires Firestore emulator")
	}
	ctx := context.Background()
	client, err := firestore.NewClient(ctx, "demo-jayeon")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	uid := fmt.Sprintf("sms-test-%d", time.Now().UnixNano())
	s := &FirestoreStore{Client: client}
	for _, id := range []string{"first", "second"} {
		_, err = recipients.DocumentRef(client, uid, id).Set(ctx, recipients.Recipient{ID: id, Name: id, Phone: "+82101234567" + map[string]string{"first": "8", "second": "9"}[id]})
		if err != nil {
			t.Fatal(err)
		}
	}
	req := CreateRequest{RequestID: "request_123", Title: "안내", Message: "hello", RecipientIDs: []string{"first", "second"}}
	c, created, err := s.Create(ctx, uid, req)
	if err != nil || !created {
		t.Fatal(c, created, err)
	}
	defer func() {
		docs, _ := s.collection(uid).Doc(c.ID).Collection("attempts").Documents(ctx).GetAll()
		for _, d := range docs {
			d.Ref.Delete(ctx)
		}
		s.collection(uid).Doc(c.ID).Delete(ctx)
		for _, id := range req.RecipientIDs {
			recipients.DocumentRef(client, uid, id).Delete(ctx)
		}
	}()
	recipients.DocumentRef(client, uid, "first").Delete(ctx)
	again, created, err := s.Create(ctx, uid, req)
	if err != nil || created || again.ID != c.ID {
		t.Fatal("idempotent after source deletion", err)
	}
	req.Title = "different"
	if _, _, err = s.Create(ctx, uid, req); !errors.Is(err, ErrIdempotency) {
		t.Fatal(err)
	}
	d, err := s.Get(ctx, uid, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	rid := d.Recipients[0].ID
	if _, err = s.Get(ctx, uid+"other", c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("owner isolation", err)
	}
	if _, err = s.Apply(ctx, uid, c.ID, "", "start", PatchRequest{}); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Apply(ctx, uid, c.ID, rid, "patch", PatchRequest{Status: Sending, AttemptID: "attempt_same"})
			if e != nil {
				t.Error(e)
				return
			}
			if r.DispatchAllowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 1 {
		t.Fatal("dispatch grants", allowed.Load())
	}
	if _, err = s.Apply(ctx, uid, c.ID, d.Recipients[1].ID, "patch", PatchRequest{Status: Sending, AttemptID: "attempt_other"}); !errors.Is(err, ErrConflict) {
		t.Fatal("parallel recipient", err)
	}
	if _, err = s.Apply(ctx, uid, c.ID, "", "cancel", PatchRequest{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Apply(ctx, uid, c.ID, rid, "patch", PatchRequest{Status: Sent, AttemptID: "attempt_same"})
	if err != nil || result.Campaign.Status != Cancelled || result.Campaign.SentCount != 1 {
		t.Fatal(result, err)
	}
	if _, err = s.Apply(ctx, uid, c.ID, rid, "patch", PatchRequest{Status: Failed, AttemptID: "attempt_same", ErrorCode: "NO_SERVICE"}); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal overwrite", err)
	}
	// 생성 시각과 문서 ID를 묶은 커서로 다음 페이지를 조회한다.
	second, _, err := s.Create(ctx, uid, CreateRequest{RequestID: "request_456", Title: "second", Message: "hi", RecipientIDs: []string{"second"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.collection(uid).Doc(second.ID).Delete(ctx)
	page, cursor, err := s.List(ctx, uid, 1, "")
	if err != nil || len(page) != 1 || cursor == nil || page[0].ID != second.ID {
		t.Fatal(page, cursor, err)
	}
	page, cursor2, err := s.List(ctx, uid, 1, *cursor)
	if err != nil || len(page) != 1 || cursor2 != nil || page[0].ID != c.ID {
		t.Fatal(page, cursor2, err)
	}
	if _, _, err = s.List(ctx, uid+"other", 1, *cursor); !errors.Is(err, ErrValidation) {
		t.Fatal("foreign cursor", err)
	}
	s.collection(uid).Doc(second.ID).Delete(ctx)
	items, next, err := s.List(ctx, uid, 1, "")
	if err != nil || len(items) != 1 || next != nil {
		t.Fatal(items, next, err)
	}
}

func TestFirestoreMaximumCampaignDocument(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires Firestore emulator")
	}
	ctx := context.Background()
	client, err := firestore.NewClient(ctx, "demo-jayeon")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	uid := fmt.Sprintf("sms-size-%d", time.Now().UnixNano())
	s := &FirestoreStore{Client: client}
	now := time.Now().UTC()
	d := document{Campaign: Campaign{ID: "maximum_document", Title: strings.Repeat("😀", 100), Message: strings.Repeat("😀", 2000), Status: PartialFailed, RecipientCount: 50, FailedCount: 50, CreatedAt: now, UpdatedAt: now}, Fingerprint: strings.Repeat("a", 64)}
	for i := range 50 {
		r := CampaignRecipient{ID: fmt.Sprintf("%020d", i), CampaignID: d.Campaign.ID, RecipientID: strings.Repeat("r", 128), Name: strings.Repeat("😀", 100), Phone: "+123456789012345", Message: d.Campaign.Message, Status: Failed, AttemptID: strings.Repeat("a", 128), AttemptCount: 20, FailedAt: &now, ErrorCode: strings.Repeat("😀", 100), ErrorMessage: strings.Repeat("😀", 300), CreatedAt: now, UpdatedAt: now}
		for range 20 {
			r.Attempts = append(r.Attempts, Attempt{ID: strings.Repeat("a", 128), Status: Failed, StartedAt: now, FinishedAt: &now})
		}
		d.Recipients = append(d.Recipients, r)
	}
	ref := s.collection(uid).Doc(d.Campaign.ID)
	defer ref.Delete(ctx)
	if _, err = ref.Set(ctx, d); err != nil {
		t.Fatal("maximum valid campaign exceeds Firestore limits", err)
	}
	got, err := s.Get(ctx, uid, d.Campaign.ID)
	if err != nil || len(got.Recipients) != 50 || len(got.Recipients[49].Attempts) != 20 {
		t.Fatal("max campaign roundtrip", err)
	}
	// 생성 시각이 같아도 ID 정렬로 누락이나 중복 없이 페이지를 조회해야 한다.
	for _, id := range []string{"tie_a", "tie_b"} {
		copy := d
		copy.Campaign.ID = id
		if _, err = s.collection(uid).Doc(id).Set(ctx, copy); err != nil {
			t.Fatal(err)
		}
		defer s.collection(uid).Doc(id).Delete(ctx)
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		items, next, err := s.List(ctx, uid, 1, cursor)
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
		if seen[items[0].ID] {
			t.Fatal("duplicate page")
		}
		seen[items[0].ID] = true
		if next == nil {
			break
		}
		cursor = *next
	}
	if len(seen) != 3 {
		t.Fatal("missing page", seen)
	}
}

// 예약 표시는 생성 시점에만 정해지고 응답·재조회·목록에 그대로 실린다.
// 필드가 없는 과거 문서는 false로 읽혀 예약함에 섞이지 않아야 한다.
func TestFirestoreReservedFlag(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires Firestore emulator")
	}
	ctx := context.Background()
	client, err := firestore.NewClient(ctx, "demo-jayeon")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	uid := fmt.Sprintf("sms-reserved-%d", time.Now().UnixNano())
	s := &FirestoreStore{Client: client}
	if _, err = recipients.DocumentRef(client, uid, "only").Set(ctx, recipients.Recipient{ID: "only", Name: "수신자", Phone: "01012345678"}); err != nil {
		t.Fatal(err)
	}
	defer recipients.DocumentRef(client, uid, "only").Delete(ctx)
	reserved, created, err := s.Create(ctx, uid, CreateRequest{RequestID: "reserved_001", Title: "예약", Message: "예약 문자", RecipientIDs: []string{"only"}, Reserved: true})
	if err != nil || !created || !reserved.Reserved {
		t.Fatal("reserved create", reserved, created, err)
	}
	defer s.collection(uid).Doc(reserved.ID).Delete(ctx)
	plain, created, err := s.Create(ctx, uid, CreateRequest{RequestID: "reserved_002", Title: "일반", Message: "보통 문자", RecipientIDs: []string{"only"}})
	if err != nil || !created || plain.Reserved {
		t.Fatal("default create", plain, created, err)
	}
	defer s.collection(uid).Doc(plain.ID).Delete(ctx)
	// 값이 없는 과거 캠페인 문서를 그대로 재현한다.
	now := time.Now().UTC()
	legacy := s.collection(uid).Doc("legacy_campaign")
	defer legacy.Delete(ctx)
	if _, err = legacy.Set(ctx, map[string]any{
		"revision":    int64(1),
		"fingerprint": strings.Repeat("b", 64),
		"recipients":  []any{},
		"campaign": map[string]any{
			"id": "legacy_campaign", "title": "과거", "message": "이전 데이터", "status": Ready,
			"recipientCount": 1, "readyCount": 1, "createdAt": now, "updatedAt": now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{reserved.ID: true, plain.ID: false, "legacy_campaign": false}
	for id, expected := range want {
		d, e := s.Get(ctx, uid, id)
		if e != nil || d.Campaign.Reserved != expected {
			t.Fatal("reread", id, d.Campaign.Reserved, e)
		}
	}
	items, next, err := s.List(ctx, uid, 50, "")
	if err != nil || next != nil || len(items) != len(want) {
		t.Fatal("list", items, next, err)
	}
	for _, item := range items {
		if item.Reserved != want[item.ID] {
			t.Fatal("list reserved", item.ID, item.Reserved)
		}
	}
}
