package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/testutil/fakes3"
	storepb "github.com/usememos/memos/proto/gen/store"
)

// newProcessRecorder starts an object endpoint that answers every read with
// statusCode and records the request line, so a test can see what the driver
// put on the wire.
func newProcessRecorder(t *testing.T, statusCode int) (*Driver, func() string) {
	t.Helper()

	var (
		mu       sync.Mutex
		rawQuery string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rawQuery = r.URL.RawQuery
		mu.Unlock()

		if statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			return
		}
		body := []byte("processed image")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	driver, err := NewDriver(context.Background(), &storepb.StorageS3Config{
		AccessKeyId:     "test-access-key",
		AccessKeySecret: "test-secret-key",
		Endpoint:        server.URL,
		Region:          "us-east-1",
		Bucket:          "attachments",
		UsePathStyle:    true,
	})
	require.NoError(t, err)

	return driver, func() string {
		mu.Lock()
		defer mu.Unlock()
		return rawQuery
	}
}

// A processing expression must reach the provider as query parameters, because
// the provider's read-time transform is driven entirely by them.
func TestGetProcessedObjectStreamSendsProcessQuery(t *testing.T) {
	ctx := context.Background()
	driver, lastQuery := newProcessRecorder(t, http.StatusOK)

	stream, err := driver.GetProcessedObjectStream(ctx, "assets/photo.heic", "", "w=600&fmt=avif&q=80")
	require.NoError(t, err)
	body, err := io.ReadAll(stream.Body)
	require.NoError(t, err)
	require.NoError(t, stream.Body.Close())
	require.Equal(t, []byte("processed image"), body)

	query, err := url.ParseQuery(lastQuery())
	require.NoError(t, err)
	require.Equal(t, "600", query.Get("w"))
	require.Equal(t, "avif", query.Get("fmt"))
	require.Equal(t, "80", query.Get("q"))
}

// A plain read must stay plain: the driver only adds the expression when one is
// asked for, so storages without a processing pipeline see their usual request.
func TestGetObjectStreamSendsNoProcessQuery(t *testing.T) {
	ctx := context.Background()
	driver, lastQuery := newProcessRecorder(t, http.StatusOK)

	stream, err := driver.GetObjectStream(ctx, "assets/photo.heic", "")
	require.NoError(t, err)
	require.NoError(t, stream.Body.Close())

	query, err := url.ParseQuery(lastQuery())
	require.NoError(t, err)
	require.Empty(t, query.Get("w"))
	require.Empty(t, query.Get("fmt"))
}

// A provider that answers a processed read with a client error has made a
// verdict on the object; callers stop asking rather than retrying forever.
func TestGetProcessedObjectStreamReportsRefusal(t *testing.T) {
	ctx := context.Background()
	driver, _ := newProcessRecorder(t, http.StatusBadRequest)

	_, err := driver.GetProcessedObjectStream(ctx, "assets/photo.heic", "", "w=600&fmt=avif")
	require.ErrorIs(t, err, ErrObjectProcessingRefused)
	require.Contains(t, err.Error(), "w=600&fmt=avif", "the refusal names the expression that was refused")
}

// Server errors are not verdicts: they must stay retryable so a provider
// outage does not permanently mark every image as unprocessable.
func TestGetProcessedObjectStreamKeepsServerErrorsTransient(t *testing.T) {
	ctx := context.Background()
	driver, _ := newProcessRecorder(t, http.StatusInternalServerError)

	_, err := driver.GetProcessedObjectStream(ctx, "assets/photo.heic", "", "w=600&fmt=avif")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrObjectProcessingRefused), "a server error is not a verdict on the object")
}

// Storages without a processing pipeline have to keep working when an
// expression is configured: they ignore the parameters they do not define, and
// the read returns the stored object.
func TestProcessedReadAgainstPlainS3Backend(t *testing.T) {
	ctx := context.Background()
	fake := fakes3.New(t, "attachments")
	driver, err := NewDriver(ctx, fake.Config("attachments"))
	require.NoError(t, err)

	content := []byte("stored object, untouched")
	_, err = driver.UploadObject(ctx, "assets/photo.heic", "image/heic", bytes.NewReader(content))
	require.NoError(t, err)

	stream, err := driver.GetProcessedObjectStream(ctx, "assets/photo.heic", "", "w=600&fmt=avif")
	require.NoError(t, err)
	read, err := io.ReadAll(stream.Body)
	require.NoError(t, err)
	require.NoError(t, stream.Body.Close())
	require.Equal(t, content, read)
}
