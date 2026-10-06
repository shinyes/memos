package fileserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/testutil/fakes3"
	apiv1 "github.com/usememos/memos/proto/gen/api/v1"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

// newHeicOnObjectStorage builds a file server whose default storage is an S3
// endpoint holding one HEIC attachment, and returns the object key so a test can
// decide what the provider answers with.
func newHeicOnObjectStorage(ctx context.Context, t *testing.T) (*FileServerService, *fakes3.Server, *apiv1.Attachment, string, func()) {
	t.Helper()

	fake := fakes3.New(t, "derivative-attachments")
	svc, fs, cleanup := newSharedProfileTestServices(ctx, t)

	configuredStorage := &storepb.Storage{
		Id:     "s3-derivatives",
		Name:   "Derivative S3",
		Type:   storepb.StorageType_STORAGE_TYPE_S3,
		Config: &storepb.Storage_S3Config{S3Config: fake.Config("derivative-attachments")},
	}
	_, err := svc.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
		Key: storepb.InstanceSettingKey_STORAGE,
		Value: &storepb.InstanceSetting_StorageSetting{StorageSetting: &storepb.InstanceStorageSetting{
			FilepathTemplate:  "files/{uuid}_{filename}",
			UploadSizeLimitMb: 30,
			Storages:          []*storepb.Storage{configuredStorage},
			DefaultStorageId:  configuredStorage.Id,
		}},
	})
	require.NoError(t, err)

	attachment := createPublicImageMemo(ctx, t, svc, "derivative-owner", "photo.heic", "image/heic", []byte("stored heic bytes"))
	uid := strings.TrimPrefix(attachment.Name, "attachments/")
	stored, err := svc.Store.GetAttachment(ctx, &store.FindAttachment{UID: &uid})
	require.NoError(t, err)
	key := stored.Payload.GetS3Object().GetKey()
	require.NotEmpty(t, key)

	return fs, fake, attachment, key, cleanup
}

func getAttachment(t *testing.T, fs *FileServerService, attachment *apiv1.Attachment, query string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	fs.RegisterRoutes(e)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/file/%s/%s%s", attachment.Name, attachment.Filename, query), nil))
	return recorder
}

// A provider that answers with the format it was asked for is served and cached
// as the in-page derivative.
func TestServeAttachmentFile_ProviderFormatIsServedAsTheDerivative(t *testing.T) {
	ctx := context.Background()
	fs, fake, attachment, key, cleanup := newHeicOnObjectStorage(ctx, t)
	defer cleanup()

	// Stand in for a provider that honoured fmt=avif.
	require.NoError(t, fake.PutObject("derivative-attachments", key, "image/avif", []byte("rendered avif")))

	display := getAttachment(t, fs, attachment, "")
	require.Equal(t, http.StatusOK, display.Code)
	require.Equal(t, displayContentType, display.Header().Get(echo.HeaderContentType))
	require.Equal(t, []byte("rendered avif"), display.Body.Bytes())

	uid := strings.TrimPrefix(attachment.Name, "attachments/")
	require.FileExists(t, filepath.Join(fs.Profile.Data, thumbnailCacheFolder, uid+displayCacheSuffix))
}

// A provider that answers with the format it was asked for serves the thumbnail
// derivative under the media type the list route has always used.
func TestServeAttachmentFile_ProviderThumbnailIsServedAsJPEG(t *testing.T) {
	ctx := context.Background()
	fs, fake, attachment, key, cleanup := newHeicOnObjectStorage(ctx, t)
	defer cleanup()

	// Stand in for a provider that honoured fmt=jpeg.
	require.NoError(t, fake.PutObject("derivative-attachments", key, "image/jpeg", []byte("rendered jpeg")))

	thumbnail := getAttachment(t, fs, attachment, "?thumbnail=true")
	require.Equal(t, http.StatusOK, thumbnail.Code)
	require.Equal(t, "image/jpeg", thumbnail.Header().Get(echo.HeaderContentType))
	require.Equal(t, []byte("rendered jpeg"), thumbnail.Body.Bytes())

	uid := strings.TrimPrefix(attachment.Name, "attachments/")
	require.FileExists(t, filepath.Join(fs.Profile.Data, thumbnailCacheFolder, uid+".v3.jpeg"))
}

// A backend with no processing pipeline ignores the expression and answers with
// the stored object. Those bytes must not be cached or served as a derivative:
// the stored object is served under its own media type, and no derivative cache
// file is written.
func TestServeAttachmentFile_UnprocessedAnswerIsNotTreatedAsADerivative(t *testing.T) {
	ctx := context.Background()
	fs, _, attachment, _, cleanup := newHeicOnObjectStorage(ctx, t)
	defer cleanup()

	// Nothing rewrites the object, so every read returns it with its own type,
	// which is what a plain S3 backend does.
	display := getAttachment(t, fs, attachment, "")
	require.Equal(t, http.StatusOK, display.Code)
	require.Equal(t, "image/heic", display.Header().Get(echo.HeaderContentType), "the stored object keeps its own media type")
	require.Equal(t, []byte("stored heic bytes"), display.Body.Bytes())

	thumbnail := getAttachment(t, fs, attachment, "?thumbnail=true")
	require.Equal(t, http.StatusOK, thumbnail.Code)
	require.Equal(t, "image/heic", thumbnail.Header().Get(echo.HeaderContentType))
	require.Equal(t, []byte("stored heic bytes"), thumbnail.Body.Bytes())

	uid := strings.TrimPrefix(attachment.Name, "attachments/")
	cacheFolder := filepath.Join(fs.Profile.Data, thumbnailCacheFolder)
	require.NoFileExists(t, filepath.Join(cacheFolder, uid+displayCacheSuffix), "an untransformed answer must not be cached as the in-page derivative")
	require.NoFileExists(t, filepath.Join(cacheFolder, uid+".v3.jpeg"), "an untransformed answer must not be cached as the thumbnail")

	// The escape hatch still returns the stored object, byte for byte, under the
	// media type the attachment declares.
	original := getAttachment(t, fs, attachment, "?original=true")
	require.Equal(t, http.StatusOK, original.Code)
	require.Equal(t, "image/heic", original.Header().Get(echo.HeaderContentType))
	require.Equal(t, []byte("stored heic bytes"), original.Body.Bytes())
}
