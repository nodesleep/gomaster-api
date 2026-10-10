package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct{}

func (fakeRunner) Run(context.Context, request) result {
	return result{Status: "accepted", Stdout: "ok\n"}
}
func testServer() *server {
	return &server{secret: bytes.Repeat([]byte("s"), 32), apiKey: strings.Repeat("k", 32), origins: map[string]bool{"https://editor.example": true}, runner: fakeRunner{}, jobs: map[string]*submission{}, queue: make(chan task, 32)}
}
func call(s *server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestAuthentication(t *testing.T) {
	s := testServer()
	if w := call(s, "POST", "/v1/submissions", `{"source_code":"package main"}`, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	token := s.token("alice")
	if sub, err := s.verify(token); err != nil || sub != "alice" {
		t.Fatal(sub, err)
	}
	parts := strings.Split(token, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"bob","exp":9999999999}`))
	if _, err := s.verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("accepted modified JWT")
	}
	r := httptest.NewRequest("POST", "/v1/auth/token", strings.NewReader(`{"subject":"alice"}`))
	r.Header.Set("X-API-Key", s.apiKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call(s, "POST", "/v1/auth/token", `{"subject":"alice"}`, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestSubmissionLifecycleAndOwnership(t *testing.T) {
	s := testServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.worker(ctx)
	token := s.token("alice")
	w := call(s, "POST", "/v1/submissions", `{"source_code":"package main"}`, token)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var j submission
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	path := "/v1/submissions/" + j.ID
	if w := call(s, "GET", path, "", s.token("bob")); w.Code != 404 {
		t.Fatal("another user could read submission")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w = call(s, "GET", path, "", token)
		json.Unmarshal(w.Body.Bytes(), &j)
		if j.Status == "accepted" {
			if j.Stdout != "ok\n" {
				t.Fatal(j)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not complete")
}
func TestValidationCORSAndQueue(t *testing.T) {
	s := testServer()
	token := s.token("alice")
	for _, body := range []string{`{}`, `{"source_code":"x","unknown":true}`, `{"source_code":"x"} {}`, `{"source_code":"` + strings.Repeat("x", 65537) + `"}`} {
		if w := call(s, "POST", "/v1/submissions", body, token); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	for i := 0; i < 32; i++ {
		if w := call(s, "POST", "/v1/submissions", `{"source_code":"x"}`, token); w.Code != 202 {
			t.Fatal(w.Code)
		}
	}
	if w := call(s, "POST", "/v1/submissions", `{"source_code":"x"}`, token); w.Code != 503 {
		t.Fatal(w.Code)
	}
	for origin, want := range map[string]int{"https://editor.example": 204, "https://evil.example": 403} {
		r := httptest.NewRequest("OPTIONS", "/v1/submissions", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(w.Code)
		}
	}
}
func TestLocalExecution(t *testing.T) {
	binary := os.Getenv("TEST_GO_BINARY")
	if binary == "" {
		t.Skip("set TEST_GO_BINARY to run real compiler integration tests")
	}
	runner, err := newLocalRunner(binary)
	if err != nil {
		t.Fatal(err)
	}
	expected := "hello world\n"
	tests := []struct {
		name, source, stdin, status string
		expected                    *string
	}{
		{"stdin", `package main;import("fmt";"os";"bufio");func main(){s:=bufio.NewScanner(os.Stdin);s.Scan();fmt.Println("hello",s.Text())}`, "world\n", "accepted", &expected},
		{"compile", `package main;func main(){ invalid }`, "", "compile_error", nil},
		{"panic", `package main;func main(){panic("boom")}`, "", "runtime_error", nil},
		{"timeout", `package main;func main(){for{}}`, "", "time_limit_exceeded", nil},
		{"output", `package main;import "fmt";func main(){for{fmt.Println("output")}}`, "", "output_limit_exceeded", nil},
		{"wrong", `package main;func main(){}`, "", "wrong_answer", &expected},
		{"secrets", `package main;import("fmt";"os");func main(){fmt.Print(os.Getenv("JWT_SECRET"))}`, "", "accepted", nil},
	}
	t.Setenv("JWT_SECRET", "do-not-inherit")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runner.Run(context.Background(), request{tc.source, tc.stdin, tc.expected})
			if res.Status != tc.status {
				t.Fatalf("want %s: %+v", tc.status, res)
			}
			if tc.name == "stdin" && res.Stdout != expected {
				t.Fatal(res)
			}
			if tc.name == "secrets" && res.Stdout != "" {
				t.Fatal("inherited secret")
			}
			if len(res.Stdout)+len(res.Stderr) > outputLimit {
				t.Fatal("unbounded output")
			}
		})
	}
}
