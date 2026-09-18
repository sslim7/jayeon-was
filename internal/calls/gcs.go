package calls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/storage"
)

// gcs.go 는 audioStore 의 Google Cloud Storage 구현이다. **이 파일이 GCS 를 아는 유일한 곳이다.**
//
// 🔴 **버킷 메타데이터를 읽지 않는다.** 런타임 서비스 계정에는 `roles/storage.objectAdmin`
// 만 있고 `storage.buckets.get` 이 없다(redhead-terraform `apps/nature/`). `bucket.Attrs` 를
// 부르는 코드를 새로 쓰는 순간 최소 권한을 넓혀야 하고, 그것을 알아차리는 시점은 보통
// 「운영에서만 403 이 난다」는 신고를 받은 뒤다. 여기서 쓰는 것은 세 개뿐이다:
// BucketHandle.SignedURL, ObjectHandle.Attrs(storage.objects.get), ObjectHandle.NewReader.

// errAudioMissing 은 오디오 객체가 GCS 에 없다는 뜻이다.
var errAudioMissing = errors.New("audio object not found")

// errSigning 은 서명 URL 발급 실패다.
//
// ⚠️ Cloud Run 에서는 개인키 없이 IAM signBlob 으로 서명된다(런타임 SA 에 자기 자신에 대한
// roles/iam.serviceAccountTokenCreator 가 붙어 있다). **로컬 개발에서는 사용자 ADC 가
// 다른 경로를 타서 이 실패가 재현되지 않는다.** 그래서 원인 문자열을 삼키지 않고 감싸
// 로그에 남긴다 — 운영에서 이 에러가 나면 십중팔구 iam.serviceAccounts.signBlob 권한이다.
var errSigning = errors.New("signed url")

type gcsAudio struct {
	bucket *storage.BucketHandle
	name   string
}

// newGCSAudio 는 버킷 핸들만 만든다. 🔴 **버킷 존재 확인을 하지 않는다**(위 주석 참고).
func newGCSAudio(client *storage.Client, bucket string) *gcsAudio {
	return &gcsAudio{bucket: client.Bucket(bucket), name: bucket}
}

func (g *gcsAudio) Bucket() string { return g.name }

func (g *gcsAudio) SignedPut(_ context.Context, object, contentType string, expires time.Time) (string, error) {
	url, err := g.bucket.SignedURL(object, &storage.SignedURLOptions{
		Method:  "PUT",
		Expires: expires,
		Scheme:  storage.SigningSchemeV4,
		// 🔴 ContentType 이 서명에 들어간다. 앱이 PUT 할 때 **글자 그대로 같은 값**을
		// Content-Type 헤더로 보내지 않으면 GCS 가 403 SignatureDoesNotMatch 를 돌려주는데,
		// 응답만 봐서는 원인이 전혀 드러나지 않는다.
		ContentType: contentType,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", errSigning, err)
	}
	return url, nil
}

// SignedGet 은 재생용 URL 이다.
//
// 🔴 **객체 존재 확인(object.Attrs)을 하지 않는다.** 통화 상세를 열 때마다 GCS 왕복이
// 붙는데, 서명 자체는 존재 여부와 무관한 로컬 계산이다. 보관 기간이 지나 객체가 사라진
// 경우 URL 은 발급되지만 재생하면 404 가 나고, 앱은 이미 그 경로를 「재생 불가」로 처리한다.
// 매 조회 GCS 왕복보다 이쪽이 낫다.
func (g *gcsAudio) SignedGet(_ context.Context, object string, expires time.Time) (string, error) {
	url, err := g.bucket.SignedURL(object, &storage.SignedURLOptions{Method: "GET", Expires: expires, Scheme: storage.SigningSchemeV4})
	if err != nil {
		return "", fmt.Errorf("%w: %v", errSigning, err)
	}
	return url, nil
}

func (g *gcsAudio) Size(ctx context.Context, object string) (int64, error) {
	attrs, err := g.bucket.Object(object).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return 0, errAudioMissing
	}
	if err != nil {
		return 0, err
	}
	return attrs.Size, nil
}

func (g *gcsAudio) Open(ctx context.Context, object string) (io.ReadCloser, error) {
	r, err := g.bucket.Object(object).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, errAudioMissing
	}
	return r, err
}
