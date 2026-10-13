package adminui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTicketAdmittedPageWriteIsRefused: a one-time ticket travels in a URL,
// so it may open a page but must not drive a page action. The control is the
// same POST with the admin key, which still flips the switch.
func TestTicketAdmittedPageWriteIsRefused(t *testing.T) {
	t.Parallel()

	snd := newTestSender(t, &recordingBus{}, false)
	s := senderServer(t, snd)

	mint := doRequest(t, s.Handler(), http.MethodPost, "/auth/ticket", "Bearer "+testKey, "localhost")
	var minted struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil || mint.Code != http.StatusOK || minted.Ticket == "" {
		t.Fatalf("POST /auth/ticket = %d %q, want 200 with a ticket", mint.Code, mint.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/apps/"+switchPanelID+"/publishing?ticket="+minted.Ticket, strings.NewReader(`{"on":true}`))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket POST publishing = %d %s, want 401", rec.Code, rec.Body.String())
	}
	if snd.Publishing().On {
		t.Error("a ticket-admitted POST switched publishing on")
	}

	code, _ := postPanelAction(t, s.Handler(), switchPanelID, "publishing", `{"on":true}`)
	if code != http.StatusOK {
		t.Errorf("key POST publishing = %d, want 200", code)
	}
	if !snd.Publishing().On {
		t.Error("the key POST did not switch publishing on, so the control cannot show the refusal mattered")
	}
}
