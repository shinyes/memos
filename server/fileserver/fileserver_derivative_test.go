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
	"github.com/usememos/memos/server/auth"
	"github.com/usememos/memos/store"
)

// An image no browser decodes is answered from the storage provider's
// processing pipeline: the attachment route returns the in-page derivative, the
// list route returns the thumbnail derivative, and ?original=true still returns
// the object exactly as uploaded. Each derivative is cached under its own file
// name, and deleting the attachment removes both.
//
// The fake provider has no processing pipeline, so it answers every read with
// the stored object. What this test proves is therefore which path the file
// server took, under which cache name, and with which media type — not what the
// provider renders.
func TestServeAttachmentFile_ImageDerivativesFromStorageProvider(t *testing.T) {
	ctx := context.Background()
	fake := fakes3.New(t, "derivative-attachments")
	svc, fs, cleanup := newSharedProfileTestServices(ctx, t)
	defer cleanup()

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

	stored := []byte("heic bytes as uploaded")
	attachment := createPublicImageMemo(ctx, t, svc, "derivative-owner", "photo.heic", "image/heic", stored)
	uid := strings.TrimPrefix(attachment.Name, "attachments/")
	cacheFolder := filepath.Join(fs.Profile.Data, thumbnailCacheFolder)
	displayPath := filepath.Join(cacheFolder, uid+displayCacheSuffix)
	thumbnailPath := filepath.Join(cacheFolder, uid+".v2.jpeg")

	e := echo.New()
	fs.RegisterRoutes(e)
	get := func(query string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/file/%s/%s%s", attachment.Name, attachment.Filename, query), nil))
		return recorder
	}

	display := get("")
	require.Equal(t, http.StatusOK, display.Code)
	require.Equal(t, displayContentType, display.Header().Get(echo.HeaderContentType))
	require.FileExists(t, displayPath)

	thumbnail := get("?thumbnail=true")
	require.Equal(t, http.StatusOK, thumbnail.Code)
	require.Equal(t, "image/jpeg", thumbnail.Header().Get(echo.HeaderContentType))
	require.FileExists(t, thumbnailPath, "this build cannot decode HEIC, so only the provider path can produce a thumbnail")

	original := get("?original=true")
	require.Equal(t, http.StatusOK, original.Code)
	require.Equal(t, "image/heic", original.Header().Get(echo.HeaderContentType))
	require.Equal(t, stored, original.Body.Bytes(), "the stored object stays reachable byte for byte")

	ownerUsername := "derivative-owner"
	owner, err := svc.Store.GetUser(ctx, &store.FindUser{Username: &ownerUsername})
	require.NoError(t, err)
	ownerCtx := context.WithValue(ctx, auth.UserIDContextKey, owner.ID)
	_, err = svc.DeleteAttachment(ownerCtx, &apiv1.DeleteAttachmentRequest{Name: attachment.Name})
	require.NoError(t, err)
	require.NoFileExists(t, displayPath, "the in-page derivative is removed with the attachment")
	require.NoFileExists(t, thumbnailPath)
}
