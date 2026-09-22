package recipients

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/xuri/excelize/v2"
)

func workbook(t *testing.T, rows [][]any) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	for i, row := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		if e := f.SetSheetRow("Sheet1", cell, &row); e != nil {
			t.Fatal(e)
		}
	}
	var b bytes.Buffer
	if e := f.Write(&b); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestImportPreviewConfirmRace(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	_, e := s.Save(ctx, uid, "", Input{Name: "기존", Phone: "01011112222"})
	if e != nil {
		t.Fatal(e)
	}
	data := workbook(t, [][]any{{"이름", "전화번호", "그룹"}, {"기존중복", "010-1111-2222", "A"}, {"신규", "01033334444", "B"}, {"파일중복", "010-3333-4444", "B"}, {"오류", "abc", ""}, {"경합", "01055556666", "A"}})
	p, e := s.Preview(ctx, uid, messaging.Upload{Name: "목록.xlsx", DataBase64: base64.StdEncoding.EncodeToString(data)})
	if e != nil || p.AddedCount != 2 || p.ExcludedCount != 3 {
		t.Fatal(p, e)
	}
	_, e = s.Save(ctx, uid, "", Input{Name: "중간등록", Phone: "01055556666"})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := s.Confirm(ctx, uid, p.ID)
			if e != nil || got.AddedCount != 1 || got.ExcludedCount != 4 || !got.Confirmed {
				t.Error(got, e)
			}
		}()
	}
	wg.Wait()
	page, e := s.List(ctx, uid, "", "", "", 100)
	if e != nil || len(page.Items) != 3 {
		t.Fatal(page, e)
	}
	if _, e = s.Confirm(ctx, uid+"other", p.ID); e == nil {
		t.Fatal("다른 사용자 import 접근")
	}
}
func TestImportInvalidFiles(t *testing.T) {
	for _, data := range [][]byte{[]byte("bad"), workbook(t, [][]any{{"이름"}, {"A"}})} {
		if _, e := parseXLSX(data); e == nil {
			t.Fatal("잘못된 파일 허용")
		}
	}
}

// 행 수를 늘린 시험용 워크북. 전화번호는 텍스트라 맨 앞 0이 보존된다.
func importRows(n int) [][]any {
	rows := [][]any{{"이름", "전화번호"}}
	for i := range n {
		rows = append(rows, []any{fmt.Sprintf("사람%d", i), fmt.Sprintf("010%08d", i)})
	}
	return rows
}

// 거절 사유가 사용자에게 그대로 가는지 본다. ErrInvalid 면 Fail() 이 「파일 또는 입력값을
// 확인해 주세요.」로 덮어써서 사용자는 무엇이 잘못됐는지 영영 알 수 없다.
func TestImportRejectionMessages(t *testing.T) {
	long := []any{"이름", "전화번호"}
	for i := range maxExtraColumns + 1 {
		long = append(long, fmt.Sprintf("부가%d", i))
	}
	cases := []struct {
		name string
		rows [][]any
		want string
	}{
		{"빈 열 제목", [][]any{{"이름", "", "전화번호"}, {"A", "", "01011112222"}}, "2번째 열"},
		{"열 제목 중복", [][]any{{"이름", "전화번호", "이름"}, {"A", "01011112222", "B"}}, "두 번 나옵니다"},
		{"같은 뜻의 열 둘", [][]any{{"이름", "전화번호", "연락처"}, {"A", "01011112222", "01022223333"}}, "함께 쓸 수 없어요"},
		{"이름 열 없음", [][]any{{"전화번호"}, {"01011112222"}}, "「이름」 열이 없어요"},
		{"전화번호 열 없음", [][]any{{"이름"}, {"A"}}, "「전화번호」(또는 「연락처」) 열이 없어요"},
		{"부가 열 초과", [][]any{long}, "열은 20개까지예요"},
		{"제목보다 많은 칸", [][]any{{"이름", "전화번호"}, {"A", "01011112222", "여분"}}, "많은 칸"},
		{"자료 행 없음", [][]any{{"이름", "전화번호"}}, "자료 행이 없어요"},
		{"엑셀이 아님", nil, "열 수 없어요"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		data := []byte("not an xlsx")
		if c.rows != nil {
			data = workbook(t, c.rows)
		}
		_, e := parseXLSX(data)
		var v messaging.ValidationError
		if !errors.As(e, &v) {
			t.Fatalf("%s: ValidationError 가 아니다: %v", c.name, e)
		}
		if errors.Is(e, messaging.ErrInvalid) {
			t.Fatalf("%s: 고정 문구로 덮인다: %v", c.name, e)
		}
		if !strings.Contains(v.Message, c.want) {
			t.Fatalf("%s: %q 가 %q 를 담고 있지 않다", c.name, v.Message, c.want)
		}
		if prev, dup := seen[v.Message]; dup {
			t.Fatalf("%s 와 %s 가 같은 문구로 뭉쳤다: %q", prev, c.name, v.Message)
		}
		seen[v.Message] = c.name
	}
}

// 안전선은 기능 한도가 아니다. 닿았을 때 실제 행 수를 말해 줘야 사용자가 나눌 수 있다.
func TestImportParseRowSafetyLimit(t *testing.T) {
	if _, e := parseXLSX(workbook(t, importRows(maxParseRows))); e != nil {
		t.Fatal("안전선 안의 행을 거절했다", e)
	}
	_, e := parseXLSX(workbook(t, importRows(maxParseRows+1)))
	var v messaging.ValidationError
	if !errors.As(e, &v) {
		t.Fatal("행 초과가 ValidationError 가 아니다", e)
	}
	if !strings.Contains(v.Message, fmt.Sprint(maxParseRows+1)) {
		t.Fatalf("실제 행 수가 문구에 없다: %q", v.Message)
	}
}

// 청크는 행 수와 크기 두 기준 모두로 끊긴다. 열이 넓으면 200행 전에 크기로 먼저 끊긴다.
func TestImportChunkBoundaries(t *testing.T) {
	narrow := []ImportRow{}
	for i := range 450 {
		narrow = append(narrow, ImportRow{Row: i + 2, Name: "가", Phone: "01000000000", Status: "ADD", CustomFields: []CustomField{}})
	}
	chunks, e := splitChunks(narrow)
	if e != nil || len(chunks) != 3 || len(chunks[0]) != maxRowsPerChunk || len(chunks[2]) != 50 {
		t.Fatal("행 수 기준으로 끊기지 않았다", len(chunks), e)
	}
	wide := []ImportRow{}
	for i := range 450 {
		fields := []CustomField{}
		for j := range 20 {
			fields = append(fields, CustomField{Name: fmt.Sprintf("부가%d", j), Value: strings.Repeat("가", 100)})
		}
		wide = append(wide, ImportRow{Row: i + 2, Name: "가", Phone: "01000000000", Status: "ADD", CustomFields: fields})
	}
	chunks, e = splitChunks(wide)
	if e != nil {
		t.Fatal(e)
	}
	if len(chunks[0]) >= maxRowsPerChunk {
		t.Fatal("넓은 파일이 크기 기준으로 먼저 끊기지 않았다", len(chunks[0]))
	}
	for i, c := range chunks {
		b, _ := json.Marshal(c)
		if len(b) > maxChunkBytes {
			t.Fatalf("청크 %d 가 %d바이트로 한도를 넘었다", i, len(b))
		}
	}
	// 🔴 앞에 정상 행이 있어도 큰 행은 지목해서 거절해야 한다. 새 청크로 밀어 넣으면
	// 한도를 넘은 문서가 그대로 저장된다.
	huge := []ImportRow{
		{Row: 2, Name: "가", Phone: "01000000000", Status: "ADD", CustomFields: []CustomField{}},
		{Row: 7, Name: "가", Phone: "01000000000", Status: "EXCLUDED", CustomFields: []CustomField{{Name: "덩어리", Value: strings.Repeat("가", maxChunkBytes)}}},
	}
	if _, e = splitChunks(huge); e == nil || !strings.Contains(e.Error(), "7행") {
		t.Fatal("너무 큰 행을 지목하지 않았다", e)
	}
	if _, e = splitChunks(huge[1:]); e == nil || !strings.Contains(e.Error(), "7행") {
		t.Fatal("첫 행이 너무 커도 지목해야 한다", e)
	}
}

func recipientCount(t *testing.T, s *Store, uid string) int {
	t.Helper()
	docs, e := collection(s.FS, uid).Documents(context.Background()).GetAll()
	if e != nil {
		t.Fatal(e)
	}
	return len(docs)
}

// 청크 경계를 넘어 확정된다. 예전 200행 한도가 막고 있던 크기다.
func TestImportConfirmAcrossChunks(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	data := workbook(t, importRows(450))
	p, e := s.Preview(ctx, uid, messaging.Upload{Name: "목록.xlsx", DataBase64: base64.StdEncoding.EncodeToString(data)})
	if e != nil || p.AddedCount != 450 || len(p.Items) != 450 {
		t.Fatal(p.AddedCount, len(p.Items), e)
	}
	if p.ChunkCount != 3 {
		t.Fatal("청크가 3개가 아니다", p.ChunkCount)
	}
	got, e := s.Confirm(ctx, uid, p.ID)
	if e != nil || got.AddedCount != 450 || got.ExcludedCount != 0 || !got.Confirmed || len(got.Items) != 450 {
		t.Fatal(got.AddedCount, got.ExcludedCount, got.Confirmed, len(got.Items), e)
	}
	if n := recipientCount(t, s, uid); n != 450 {
		t.Fatal("저장된 수신자 수", n)
	}
	// 같은 요청을 다시 불러도 중복 생성되지 않는다.
	if got, e = s.Confirm(ctx, uid, p.ID); e != nil || got.AddedCount != 450 {
		t.Fatal(got.AddedCount, e)
	}
	if n := recipientCount(t, s, uid); n != 450 {
		t.Fatal("재확정으로 중복 생성", n)
	}
}

// 첫 청크만 끝난 상태에서 끊겼다가 다시 불려도 이어서 끝나고 중복 생성되지 않는다.
func TestImportConfirmResumes(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	data := workbook(t, importRows(450))
	p, e := s.Preview(ctx, uid, messaging.Upload{Name: "목록.xlsx", DataBase64: base64.StdEncoding.EncodeToString(data)})
	if e != nil {
		t.Fatal(e)
	}
	ref := messaging.Collection(s.FS, uid, "recipientImports").Doc(p.ID)
	if e = s.confirmChunk(ctx, uid, ref, 0, false); e != nil {
		t.Fatal(e)
	}
	if n := recipientCount(t, s, uid); n != maxRowsPerChunk {
		t.Fatal("첫 청크만 저장되지 않았다", n)
	}
	got, e := s.Confirm(ctx, uid, p.ID)
	if e != nil || got.AddedCount != 450 || !got.Confirmed {
		t.Fatal(got.AddedCount, got.Confirmed, e)
	}
	if n := recipientCount(t, s, uid); n != 450 {
		t.Fatal("이어서 확정한 뒤 수신자 수", n)
	}
}

// 배포 전 형식(메타 문서 안에 items) 으로 남아 있는 미리보기도 확정된다.
func TestImportConfirmLegacyDocument(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	items := []ImportRow{
		{Row: 2, Name: "옛날1", Phone: "01011112222", Status: "ADD", CustomFields: []CustomField{}},
		{Row: 3, Name: "옛날2", Phone: "01033334444", Status: "ADD", CustomFields: []CustomField{}},
		{Row: 4, Name: "옛날3", Phone: "abc", Status: "EXCLUDED", Reason: "잘못된 번호", CustomFields: []CustomField{}},
	}
	ref := messaging.Collection(s.FS, uid, "recipientImports").NewDoc()
	if _, e := ref.Create(ctx, map[string]any{"id": ref.ID, "addedCount": 2, "excludedCount": 1, "items": items, "createdAt": time.Now().UTC(), "confirmed": false}); e != nil {
		t.Fatal(e)
	}
	got, e := s.Confirm(ctx, uid, ref.ID)
	if e != nil || got.AddedCount != 2 || got.ExcludedCount != 1 || !got.Confirmed || len(got.Items) != 3 {
		t.Fatal(got.AddedCount, got.ExcludedCount, got.Confirmed, len(got.Items), e)
	}
	if n := recipientCount(t, s, uid); n != 2 {
		t.Fatal("이전 형식 확정 후 수신자 수", n)
	}
	if _, e = s.Confirm(ctx, uid, ref.ID); e != nil {
		t.Fatal(e)
	}
	if n := recipientCount(t, s, uid); n != 2 {
		t.Fatal("이전 형식 재확정으로 중복 생성", n)
	}
}
