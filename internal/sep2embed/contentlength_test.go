package sep2embed

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// TestGETMirrorUsagePointListOver2048BytesCarriesContentLength pins the
// wire behavior server-go v0.4.0 fixes (#613, upstream #615): a protocol
// response over 2048 bytes must be buffered and served with
// Content-Length, not Transfer-Encoding: chunked. The EPRI reference
// client, and any 2030.5 client that cannot decode chunked responses,
// cannot parse GET /mup once it crosses that size at server-go v0.3.0.
func TestGETMirrorUsagePointListOver2048BytesCarriesContentLength(t *testing.T) {
	t.Parallel()

	baseURL, devices := newMUPTestServer(t, "mup-cl-001")
	dev := devices[0]

	// One device posting enough distinct MirrorUsagePoints is sufficient:
	// GET /mup is a server-wide, unscoped list (sep.xsd 6.2.3.1), so every
	// created resource is served back regardless of which device created it.
	const n = 20
	for i := 0; i < n; i++ {
		mrid := fmt.Sprintf("5EB2C2C5C2E1E1E1E1E1E1E1%08d", i)
		status, _, body := postMirrorUsagePoint(t, dev, baseURL, mrid, dev.lfdi)
		if status != http.StatusCreated {
			t.Fatalf("POST /mup[%d] status = %d, want 201; body=%s", i, status, body)
		}
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/mup", nil)
	if err != nil {
		t.Fatalf("NewRequest GET /mup: %v", err)
	}
	req.Header.Set("Accept", "application/sep+xml")
	resp, err := dev.client.Do(req)
	if err != nil {
		t.Fatalf("GET /mup: %v", err)
	}
	defer resp.Body.Close()

	// Read by the consumer's path: assert on resp.Header and
	// resp.TransferEncoding, exactly what a real client's http.Transport
	// parses off the wire, not on whether io.ReadAll succeeded (net/http
	// transparently decodes chunking either way).
	contentLength := resp.Header.Get("Content-Length")
	transferEncoding := resp.TransferEncoding

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET /mup body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /mup status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if len(body) <= 2048 {
		t.Fatalf("test setup: served MirrorUsagePointList is %d bytes, want over 2048 to exercise the constrained-client path", len(body))
	}

	if contentLength == "" {
		t.Errorf("GET /mup (%d bytes) response carries no Content-Length header; want the buffered length", len(body))
	} else if want := strconv.Itoa(len(body)); contentLength != want {
		t.Errorf("GET /mup Content-Length = %q, want %q (the actual served length)", contentLength, want)
	}
	if len(transferEncoding) != 0 {
		t.Errorf("GET /mup response used Transfer-Encoding %v, want none (buffered with Content-Length)", transferEncoding)
	}
}
