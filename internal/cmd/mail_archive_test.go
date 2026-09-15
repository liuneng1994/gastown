package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
)

func TestStaleMessagesForSession(t *testing.T) {
	sessionStart := time.Date(2026, 1, 24, 2, 0, 0, 0, time.UTC)
	messages := []*mail.Message{
		{ID: "msg-1", Subject: "Older", Timestamp: sessionStart.Add(-2 * time.Minute)},
		{ID: "msg-2", Subject: "Newer", Timestamp: sessionStart.Add(2 * time.Minute)},
		{ID: "msg-3", Subject: "Equal", Timestamp: sessionStart},
	}

	stale := staleMessagesForSession(messages, sessionStart)
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale message, got %d", len(stale))
	}
	if stale[0].Message.ID != "msg-1" {
		t.Fatalf("expected msg-1 stale, got %s", stale[0].Message.ID)
	}
}

type fakeArchiveMailbox struct {
	messages      []*mail.Message
	attempted     []string
	deleted       []string
	deleteErrs    map[string]error
	blockOnDelete bool
	deleteStarted chan struct{}
	listCalls     int
}

func (f *fakeArchiveMailbox) ListContext(ctx context.Context) ([]*mail.Message, error) {
	f.listCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.messages, nil
}

func (f *fakeArchiveMailbox) DeleteContext(ctx context.Context, id string) error {
	f.attempted = append(f.attempted, id)
	if f.deleteStarted != nil {
		select {
		case f.deleteStarted <- struct{}{}:
		default:
		}
	}
	if f.blockOnDelete {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.deleteErrs[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func TestRunMailArchiveWithMailboxReportsDeadlineWithoutArchiving(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	box := &fakeArchiveMailbox{}
	var stdout bytes.Buffer
	err := runMailArchiveWithMailbox(ctx, box, []string{"hq-first", "hq-second"}, &stdout)

	if err == nil {
		t.Fatal("runMailArchiveWithMailbox returned nil; want deadline error")
	}
	if !strings.Contains(err.Error(), "failed to archive 1 messages") {
		t.Fatalf("error = %v, want aggregate archive failure", err)
	}
	if len(box.attempted) != 0 {
		t.Fatalf("attempted deletes = %v, want none after expired deadline", box.attempted)
	}
	out := stdout.String()
	if !strings.Contains(out, "Archived 0/2 messages") {
		t.Fatalf("stdout = %q, want partial archive count", out)
	}
	if !strings.Contains(out, "archiving message timed out after 4s") {
		t.Fatalf("stdout = %q, want phase-specific timeout", out)
	}
}

func TestRunMailArchiveWithMailboxStopsAfterInjectedBlockingDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := &fakeArchiveMailbox{
		blockOnDelete: true,
		deleteStarted: make(chan struct{}, 1),
	}
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runMailArchiveWithMailbox(ctx, box, []string{"hq-first", "hq-second"}, &stdout)
	}()

	select {
	case <-box.deleteStarted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("DeleteContext was not called")
	}

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("runMailArchiveWithMailbox did not return after cancellation")
	}

	if err == nil {
		t.Fatal("runMailArchiveWithMailbox returned nil; want canceled error")
	}
	if len(box.attempted) != 1 || box.attempted[0] != "hq-first" {
		t.Fatalf("attempted deletes = %v, want only first ID", box.attempted)
	}
	if !strings.Contains(stdout.String(), "Archived 0/2 messages") {
		t.Fatalf("stdout = %q, want failed full count", stdout.String())
	}
}

func TestRunMailArchiveWithMailboxTreatsGCNotFoundAsSuccess(t *testing.T) {
	ctx := context.Background()
	box := &fakeArchiveMailbox{
		deleteErrs: map[string]error{
			"hq-gone": mail.ErrMessageNotFound,
		},
	}
	var stdout bytes.Buffer
	err := runMailArchiveWithMailbox(ctx, box, []string{"hq-gone", "hq-ok"}, &stdout)

	if err != nil {
		t.Fatalf("runMailArchiveWithMailbox returned error: %v", err)
	}
	if got, want := len(box.deleted), 1; got != want {
		t.Fatalf("deleted count = %d, want %d", got, want)
	}
	out := stdout.String()
	if !strings.Contains(out, "underlying bead already gone") {
		t.Fatalf("stdout = %q, want GC note", out)
	}
	if !strings.Contains(out, "Archived 2 messages") {
		t.Fatalf("stdout = %q, want aggregate success", out)
	}
}

func TestRunMailArchiveStaleWithMailboxListCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	box := &fakeArchiveMailbox{}
	err := runMailArchiveStaleWithMailbox(ctx, box, time.Now(), &bytes.Buffer{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if !strings.Contains(err.Error(), "listing stale messages canceled") {
		t.Fatalf("error = %v, want phase-specific list context", err)
	}
}

func TestRunMailArchiveStaleWithMailboxStopsAfterInjectedBlockingDelete(t *testing.T) {
	sessionStart := time.Date(2026, 1, 24, 2, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := &fakeArchiveMailbox{
		messages: []*mail.Message{
			{ID: "msg-old", Subject: "Older", Timestamp: sessionStart.Add(-time.Minute)},
			{ID: "msg-older", Subject: "Older still", Timestamp: sessionStart.Add(-2 * time.Minute)},
		},
		blockOnDelete: true,
		deleteStarted: make(chan struct{}, 1),
	}
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runMailArchiveStaleWithMailbox(ctx, box, sessionStart, &stdout)
	}()

	select {
	case <-box.deleteStarted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("DeleteContext was not called")
	}

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("runMailArchiveStaleWithMailbox did not return after cancellation")
	}

	if err == nil {
		t.Fatal("runMailArchiveStaleWithMailbox returned nil; want canceled error")
	}
	if len(box.attempted) != 1 || box.attempted[0] != "msg-old" {
		t.Fatalf("attempted deletes = %v, want only first stale message", box.attempted)
	}
	if !strings.Contains(stdout.String(), "Archived 0/2 stale messages") {
		t.Fatalf("stdout = %q, want failed stale count", stdout.String())
	}
}

func TestRunMailArchiveStaleSessionLookupUsesContext(t *testing.T) {
	oldLookup := mailArchiveSessionCreatedAt
	defer func() { mailArchiveSessionCreatedAt = oldLookup }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var gotCtxErr error
	mailArchiveSessionCreatedAt = func(ctx context.Context, _ string) (time.Time, error) {
		gotCtxErr = ctx.Err()
		return time.Time{}, ctx.Err()
	}

	box := &fakeArchiveMailbox{}
	err := runMailArchiveStale(ctx, box, "gastown/polecats/toast")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if !errors.Is(gotCtxErr, context.Canceled) {
		t.Fatalf("session lookup ctx err = %v, want context.Canceled", gotCtxErr)
	}
	if box.listCalls != 0 {
		t.Fatalf("list calls = %d, want session lookup failure before list", box.listCalls)
	}
	if !strings.Contains(err.Error(), "getting session start time for gt-toast canceled") {
		t.Fatalf("error = %v, want phase-specific session lookup context", err)
	}
}
