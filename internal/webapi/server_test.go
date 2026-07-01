package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ekho/gobackup-docker/internal/pipeline"
)

func newTestServer(gobackupURL string) *httptest.Server {
	s := &Server{
		Status:      func() pipeline.Status { return pipeline.Status{HostID: "h", Models: []string{"pgtest"}} },
		GobackupURL: gobackupURL,
	}
	return httptest.NewServer(s.Handler())
}

func TestHealthz(t *testing.T) {
	ts := newTestServer("")
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStatus(t *testing.T) {
	ts := newTestServer("")
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got pipeline.Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HostID != "h" || len(got.Models) != 1 || got.Models[0] != "pgtest" {
		t.Errorf("status = %#v", got)
	}
}

func TestPerform_proxiesKnownModel(t *testing.T) {
	var gotModel string
	gobackup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/perform" {
			t.Errorf("gobackup got path %q", r.URL.Path)
		}
		_ = r.ParseForm()
		gotModel = r.FormValue("model")
		w.WriteHeader(200)
		io.WriteString(w, "Backup: performed")
	}))
	defer gobackup.Close()

	ts := newTestServer(gobackup.URL)
	defer ts.Close()

	resp, err := http.PostForm(ts.URL+"/api/perform", url.Values{"model": {"pgtest"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotModel != "pgtest" {
		t.Errorf("gobackup received model %q, want pgtest", gotModel)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "performed") {
		t.Errorf("body = %q", body)
	}
}

func TestPerform_errors(t *testing.T) {
	// Unknown model → 404 (even though gobackup would be reachable).
	ts := newTestServer("http://127.0.0.1:0")
	defer ts.Close()
	resp, _ := http.PostForm(ts.URL+"/api/perform", url.Values{"model": {"nope"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown model: status %d, want 404", resp.StatusCode)
	}

	// Not configured → 501.
	ts2 := newTestServer("")
	defer ts2.Close()
	resp2, _ := http.PostForm(ts2.URL+"/api/perform", url.Values{"model": {"pgtest"}})
	if resp2.StatusCode != http.StatusNotImplemented {
		t.Errorf("no gobackup url: status %d, want 501", resp2.StatusCode)
	}

	// Wrong method → 405.
	resp3, _ := http.Get(ts.URL + "/api/perform")
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET perform: status %d, want 405", resp3.StatusCode)
	}
}
