package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type request struct {
	Source   string  `json:"source_code"`
	Stdin    string  `json:"stdin"`
	Expected *string `json:"expected_output,omitempty"`
}
type result struct {
	Status        string `json:"status"`
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	CompileOutput string `json:"compile_output"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	TimeMS        int64  `json:"time_ms"`
}
type submission struct {
	ID string `json:"id"`
	result
	Created time.Time `json:"created_at"`
	owner   string
	input   request
}
type task struct {
	id    string
	input request
}
type executor interface {
	Run(context.Context, request) result
}
type server struct {
	secret  []byte
	apiKey  string
	origins map[string]bool
	runner  executor
	mu      sync.Mutex
	jobs    map[string]*submission
	queue   chan task
}
type claims struct {
	Subject  string `json:"sub"`
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	Issued   int64  `json:"iat"`
}

func (s *server) token(subject string) string {
	enc := base64.RawURLEncoding.EncodeToString
	body, _ := json.Marshal(claims{subject, "gomaster-api", "gomaster-editor", time.Now().Add(time.Hour).Unix(), time.Now().Unix()})
	raw := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(body)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(raw))
	return raw + "." + enc(mac.Sum(nil))
}
func (s *server) verify(token string) (string, error) {
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return "", errors.New("invalid token")
	}
	dec := base64.RawURLEncoding.DecodeString
	header, e := dec(p[0])
	if e != nil {
		return "", e
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "HS256" || h.Typ != "JWT" {
		return "", errors.New("invalid header")
	}
	sig, e := dec(p[2])
	if e != nil {
		return "", e
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(p[0] + "." + p[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", errors.New("invalid signature")
	}
	body, e := dec(p[1])
	if e != nil {
		return "", e
	}
	var c claims
	if json.Unmarshal(body, &c) != nil || c.Subject == "" || c.Issuer != "gomaster-api" || c.Audience != "gomaster-editor" || c.Expires <= time.Now().Unix() || c.Issued > time.Now().Unix()+30 {
		return "", errors.New("invalid claims")
	}
	return c.Subject, nil
}
func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	respond(w, code, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	origin := r.Header.Get("Origin")
	if origin != "" {
		w.Header().Add("Vary", "Origin")
		if !s.origins[origin] {
			fail(w, 403, "origin not allowed")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
	}
	if r.URL.Path == "/health" && r.Method == "GET" {
		respond(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/v1/auth/token" && r.Method == "POST" {
		key := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			fail(w, 401, "invalid API key")
			return
		}
		var in struct {
			Subject string `json:"subject"`
		}
		if decode(w, r, &in) != nil || strings.TrimSpace(in.Subject) == "" || len(in.Subject) > 128 {
			fail(w, 400, "subject must contain 1–128 characters")
			return
		}
		respond(w, 200, map[string]any{"access_token": s.token(in.Subject), "token_type": "Bearer", "expires_in": 3600})
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(auth) > 4096 {
		fail(w, 401, "Bearer token required")
		return
	}
	owner, err := s.verify(strings.TrimPrefix(auth, "Bearer "))
	if err != nil {
		fail(w, 401, "invalid or expired token")
		return
	}
	if r.URL.Path == "/v1/submissions" && r.Method == "POST" {
		var in request
		if decode(w, r, &in) != nil || strings.TrimSpace(in.Source) == "" || len(in.Source) > 64*1024 || len(in.Stdin) > 64*1024 || (in.Expected != nil && len(*in.Expected) > 64*1024) {
			fail(w, 400, "invalid JSON or input: source_code is required; each field is limited to 64 KiB")
			return
		}
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			fail(w, 500, "cannot create submission")
			return
		}
		id := hex.EncodeToString(b[:])
		s.mu.Lock()
		// Expire terminal submissions after one hour; queued/running work is retained.
		for k, j := range s.jobs {
			if time.Since(j.Created) > time.Hour && j.Status != "queued" && j.Status != "processing" {
				delete(s.jobs, k)
			}
		}
		if len(s.jobs) >= 1000 {
			s.mu.Unlock()
			fail(w, 503, "submission capacity reached; retry later")
			return
		}
		s.jobs[id] = &submission{ID: id, result: result{Status: "queued"}, Created: time.Now().UTC(), owner: owner, input: in}
		select {
		case s.queue <- task{id, in}:
		default:
			delete(s.jobs, id)
			s.mu.Unlock()
			fail(w, 503, "execution queue full; retry later")
			return
		}
		out := *s.jobs[id]
		s.mu.Unlock()
		w.Header().Set("Location", "/v1/submissions/"+id)
		respond(w, 202, out)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/submissions/") && r.Method == "GET" {
		id := strings.TrimPrefix(r.URL.Path, "/v1/submissions/")
		s.mu.Lock()
		j, ok := s.jobs[id]
		if !ok || j.owner != owner || (time.Since(j.Created) > time.Hour && j.Status != "queued" && j.Status != "processing") {
			s.mu.Unlock()
			fail(w, 404, "submission not found")
			return
		}
		out := *j
		s.mu.Unlock()
		respond(w, 200, out)
		return
	}
	fail(w, 404, "endpoint not found")
}
func (s *server) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-s.queue:
			s.mu.Lock()
			s.jobs[t.id].Status = "processing"
			s.mu.Unlock()
			res := s.runner.Run(ctx, t.input)
			s.mu.Lock()
			s.jobs[t.id].result = res
			s.jobs[t.id].input = request{}
			s.mu.Unlock()
		}
	}
}
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func main() {
	secret, key := os.Getenv("JWT_SECRET"), os.Getenv("API_KEY")
	if len(secret) < 32 || len(key) < 32 {
		log.Fatal("JWT_SECRET and API_KEY must each be at least 32 bytes")
	}
	n, err := strconv.Atoi(env("WORKERS", "2"))
	if err != nil || n < 1 || n > 8 {
		log.Fatal("WORKERS must be 1–8")
	}
	runner, err := newLocalRunner(env("GO_BINARY", "go"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{secret: []byte(secret), apiKey: key, origins: map[string]bool{}, runner: runner, jobs: map[string]*submission{}, queue: make(chan task, 32)}
	for _, o := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			s.origins[o] = true
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	for i := 0; i < n; i++ {
		go s.worker(ctx)
	}
	srv := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("listening on %s; local runner enabled for trusted code", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(fmt.Errorf("HTTP server: %w", err))
	}
}
