package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
)

// fakeHookRepo is an in-memory hook store for tests.
type fakeHookRepo struct {
	mu     sync.Mutex
	hooks  map[domain.HookID]*domain.Hook
	listFn func(ctx context.Context) ([]*domain.Hook, error)
}

func newFakeHookRepo() *fakeHookRepo {
	return &fakeHookRepo{hooks: make(map[domain.HookID]*domain.Hook)}
}

func (r *fakeHookRepo) Save(_ context.Context, h *domain.Hook) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[h.ID] = h
	return nil
}

func (r *fakeHookRepo) FindByID(_ context.Context, id domain.HookID) (*domain.Hook, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hooks[id]; ok {
		return h, nil
	}
	return nil, errors.New("not found")
}

func (r *fakeHookRepo) List(ctx context.Context) ([]*domain.Hook, error) {
	if r.listFn != nil {
		return r.listFn(ctx)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Hook, 0, len(r.hooks))
	for _, h := range r.hooks {
		out = append(out, h)
	}
	return out, nil
}

func (r *fakeHookRepo) Delete(_ context.Context, id domain.HookID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hooks, id)
	return nil
}

func newSvc(repo *fakeHookRepo) *Service {
	return &Service{
		cfg:          config.HooksConfig{},
		hookRepo:     repo,
		bus:          events.New(1, 16),
		client:       &http.Client{Timeout: 5 * time.Second},
		kafkaWriters: make(map[string]*kafka.Writer),
	}
}

func TestDeliverHTTPSuccess(t *testing.T) {
	var got struct {
		Body []byte
		Sig  string
		CT   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Body, _ = io.ReadAll(r.Body)
		got.Sig = r.Header.Get("X-OpenStreamer-Signature")
		got.CT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	repo := newFakeHookRepo()
	svc := newSvc(repo)
	hook := &domain.Hook{ID: "h1", Type: domain.HookTypeHTTP, Target: srv.URL, Secret: "topsecret"}

	ev := domain.Event{ID: "e1", Type: domain.EventStreamCreated, StreamCode: "s1"}
	if err := svc.deliver(context.Background(), hook, ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got.CT != "application/json" {
		t.Errorf("content-type=%s", got.CT)
	}
	if len(got.Body) == 0 {
		t.Fatal("server received empty body")
	}

	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(got.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got.Sig != want {
		t.Errorf("sig=%s want %s", got.Sig, want)
	}

	var parsed domain.Event
	if err := json.Unmarshal(got.Body, &parsed); err != nil {
		t.Fatalf("server received invalid JSON: %v", err)
	}
	if parsed.ID != "e1" || parsed.Type != domain.EventStreamCreated {
		t.Errorf("event mangled: %+v", parsed)
	}
}

func TestDeliverHTTPNoSecretSkipsSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-OpenStreamer-Signature") != "" {
			t.Errorf("unexpected signature header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: srv.URL}
	if err := svc.deliver(context.Background(), hook, domain.Event{Type: domain.EventStreamStarted}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

func TestDeliverHTTPNon2xxFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: srv.URL}
	err := svc.deliver(context.Background(), hook, domain.Event{Type: domain.EventStreamStarted})
	if err == nil {
		t.Fatal("expected error for 5xx response")
	}
}

func TestDeliverHTTPBadURLFails(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: "://bad"}
	err := svc.deliver(context.Background(), hook, domain.Event{Type: domain.EventStreamStarted})
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestDeliverHTTPHonoursTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: srv.URL, TimeoutSec: 1}

	start := time.Now()
	err := svc.deliver(context.Background(), hook, domain.Event{Type: domain.EventStreamStarted})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("timeout not honoured: elapsed=%v", elapsed)
	}
}

func TestDeliverUnknownTypeFails(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: "carrier-pigeon", Target: "x"}
	if err := svc.deliver(context.Background(), hook, domain.Event{}); err == nil {
		t.Fatal("expected error for unknown hook type")
	}
}

func TestDeliverKafkaWithoutBrokers(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeKafka, Target: "topic"}
	err := svc.deliver(context.Background(), hook, domain.Event{Type: domain.EventStreamCreated})
	if err == nil {
		t.Fatal("expected error when no brokers configured")
	}
}

func TestMatchesEventTypeFilter(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	h := &domain.Hook{EventTypes: []domain.EventType{domain.EventStreamStarted}}

	if !svc.matches(h, domain.Event{Type: domain.EventStreamStarted}) {
		t.Error("matching event type should pass")
	}
	if svc.matches(h, domain.Event{Type: domain.EventStreamStopped}) {
		t.Error("non-matching event type should fail")
	}
}

func TestMatchesEmptyEventTypesAllowsAll(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	h := &domain.Hook{}
	if !svc.matches(h, domain.Event{Type: domain.EventStreamStarted}) {
		t.Error("empty EventTypes should match everything")
	}
}

func TestMatchesStreamCodeFilter(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	h := &domain.Hook{StreamCodes: &domain.StreamCodeFilter{Only: []domain.StreamCode{"a"}}}

	if !svc.matches(h, domain.Event{StreamCode: "a"}) {
		t.Error("Only=[a] should match a")
	}
	if svc.matches(h, domain.Event{StreamCode: "b"}) {
		t.Error("Only=[a] should not match b")
	}
}

func TestWithMetadataMergesIntoPayload(t *testing.T) {
	ev := domain.Event{Payload: map[string]any{"foo": 1}}
	out := withMetadata(ev, map[string]string{"env": "prod"})
	if out.Payload["foo"] != 1 {
		t.Errorf("original payload field missing")
	}
	if out.Payload["meta.env"] != "prod" {
		t.Errorf("metadata not merged: %+v", out.Payload)
	}
}

func TestWithMetadataNoMetadataIsNoOp(t *testing.T) {
	ev := domain.Event{Payload: map[string]any{"foo": 1}}
	out := withMetadata(ev, nil)
	if &out.Payload == &ev.Payload && len(out.Payload) != 1 {
		t.Errorf("payload changed unexpectedly")
	}
}

func TestDispatchSkipsDisabledHooks(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := newFakeHookRepo()
	repo.hooks["h1"] = &domain.Hook{ID: "h1", Type: domain.HookTypeHTTP, Target: srv.URL, Enabled: false}
	repo.hooks["h2"] = &domain.Hook{ID: "h2", Type: domain.HookTypeHTTP, Target: srv.URL, Enabled: true}

	svc := newSvc(repo)
	if err := svc.dispatch(context.Background(), domain.Event{Type: domain.EventStreamStarted}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 delivery, got %d", calls.Load())
	}
}

func TestDispatchListErrorPropagates(t *testing.T) {
	repo := newFakeHookRepo()
	repo.listFn = func(_ context.Context) ([]*domain.Hook, error) {
		return nil, errors.New("db dead")
	}
	svc := newSvc(repo)
	if err := svc.dispatch(context.Background(), domain.Event{}); err == nil {
		t.Fatal("expected list error to propagate")
	}
}

func TestDeliverWithRetryEventuallySucceeds(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := newSvc(newFakeHookRepo())
	// Use original deliverWithRetry but with small backoffs by manipulating MaxRetries=1.
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: srv.URL, MaxRetries: 1}
	if err := svc.deliverWithRetry(context.Background(), hook, domain.Event{}); err != nil {
		t.Fatalf("retry should eventually succeed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestDeliverWithRetryRespectsCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := newSvc(newFakeHookRepo())
	hook := &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: srv.URL, MaxRetries: 3}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := svc.deliverWithRetry(ctx, hook, domain.Event{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	// Should give up well before the longest backoff.
	if elapsed > 2*time.Second {
		t.Errorf("did not cancel promptly: %v", elapsed)
	}
}

func TestDeliverTestEventNotFound(t *testing.T) {
	svc := newSvc(newFakeHookRepo())
	if err := svc.DeliverTestEvent(context.Background(), "nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestDeliverTestEventHTTP(t *testing.T) {
	var got domain.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	repo := newFakeHookRepo()
	repo.hooks["h"] = &domain.Hook{ID: "h", Type: domain.HookTypeHTTP, Target: srv.URL}
	svc := newSvc(repo)

	if err := svc.DeliverTestEvent(context.Background(), "h"); err != nil {
		t.Fatalf("DeliverTestEvent: %v", err)
	}
	if got.Payload["test"] != true {
		t.Errorf("payload not delivered: %+v", got.Payload)
	}
	if got.Type != domain.EventStreamCreated {
		t.Errorf("type=%s", got.Type)
	}
}

func TestDeliverTestEventUnsupportedType(t *testing.T) {
	repo := newFakeHookRepo()
	repo.hooks["h"] = &domain.Hook{ID: "h", Type: "carrier-pigeon"}
	svc := newSvc(repo)
	err := svc.DeliverTestEvent(context.Background(), "h")
	if !errors.Is(err, ErrHookTestUnsupported) {
		t.Fatalf("want ErrHookTestUnsupported, got %v", err)
	}
}
