package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
)

type fakeInboxLister struct {
	calls    int
	messages []*mail.Message
	err      error
}

func (f *fakeInboxLister) List() ([]*mail.Message, error) {
	f.calls++
	return f.messages, f.err
}

func TestLoadInboxSnapshotListsOnceAndCounts(t *testing.T) {
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "msg-1", Read: false},
			{ID: "msg-2", Read: true},
			{ID: "msg-3", Read: false},
		},
	}

	messages, total, unread, err := loadInboxSnapshot(box, false)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
	if total != 3 || unread != 2 {
		t.Fatalf("counts = (%d total, %d unread), want (3, 2)", total, unread)
	}
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
}

func TestLoadInboxSnapshotUnreadOnlyFiltersAfterSingleList(t *testing.T) {
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "msg-1", Read: false},
			{ID: "msg-2", Read: true},
			{ID: "msg-3", Read: false},
		},
	}

	messages, total, unread, err := loadInboxSnapshot(box, true)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
	if total != 3 || unread != 2 {
		t.Fatalf("counts = (%d total, %d unread), want (3, 2)", total, unread)
	}
	if len(messages) != 2 {
		t.Fatalf("filtered messages len = %d, want 2", len(messages))
	}
	if messages[0].ID != "msg-1" || messages[1].ID != "msg-3" {
		t.Fatalf("filtered messages = [%s %s], want [msg-1 msg-3]", messages[0].ID, messages[1].ID)
	}
}

func TestLoadInboxSnapshotPropagatesListError(t *testing.T) {
	wantErr := errors.New("list failed")
	box := &fakeInboxLister{err: wantErr}

	_, _, _, err := loadInboxSnapshot(box, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
}

func TestMailCommandContextDeadlineIsUnderPatrolThreshold(t *testing.T) {
	ctx, cancel := newMailCommandContext()
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("mail command context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("mail command deadline remaining = %s, want >0 and <=5s", remaining)
	}
}

func TestRunMailReadUsesOneSharedCommandContext(t *testing.T) {
	oldContext := newMailCommandContext
	ctx, cancel := context.WithCancel(context.Background())
	newMailCommandContext = func() (context.Context, context.CancelFunc) {
		return ctx, cancel
	}
	t.Cleanup(func() {
		newMailCommandContext = oldContext
		cancel()
	})

	oldGetMailbox := getMailboxForRead
	box := &fakeMailReadMailbox{
		msg: &mail.Message{
			ID:        "msg-shared",
			Subject:   "Shared context",
			From:      "mayor/",
			To:        "gastown/synth",
			Timestamp: time.Date(2026, 9, 14, 18, 2, 0, 0, time.UTC),
		},
	}
	getMailboxForRead = func(address string) (mailReadMailbox, error) {
		return box, nil
	}
	t.Cleanup(func() { getMailboxForRead = oldGetMailbox })

	oldJSON := mailReadJSON
	mailReadJSON = true
	t.Cleanup(func() { mailReadJSON = oldJSON })

	_ = captureStdout(t, func() {
		if err := runMailRead(nil, []string{"msg-shared"}); err != nil {
			t.Fatalf("runMailRead: %v", err)
		}
	})
	if box.lastGetContext != ctx {
		t.Fatal("GetContext did not receive command context")
	}
	if box.lastMarkContext != ctx {
		t.Fatal("MarkReadMessageContext did not receive the same command context")
	}
}

type fakeInboxContextLister struct {
	listCalls int
	blockList bool
}

func (f *fakeInboxContextLister) ListContext(ctx context.Context) ([]*mail.Message, error) {
	f.listCalls++
	if f.blockList {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []*mail.Message{{ID: "msg-1", Subject: "Hello"}}, nil
}

func TestRunMailInboxReturnsPhaseSpecificListTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	oldJSON := mailInboxJSON
	mailInboxJSON = true
	t.Cleanup(func() { mailInboxJSON = oldJSON })

	box := &fakeInboxContextLister{blockList: true}
	stdout := captureStdout(t, func() {
		err := runMailInboxWithMailbox(ctx, box, "gastown/synth")
		if err == nil {
			t.Fatal("runMailInboxWithMailbox returned nil, want timeout error")
		}
		if !strings.Contains(err.Error(), "listing messages timed out after") {
			t.Fatalf("error = %v, want listing phase timeout", err)
		}
		if box.listCalls != 1 {
			t.Fatalf("ListContext calls = %d, want 1", box.listCalls)
		}
	})
	if stdout != "" {
		t.Fatalf("stdout = %q, want no partial inbox output before list succeeds", stdout)
	}
}

func TestRunMailInboxDoesNotPrintDurablePartialWhenWispQueryCancels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake bd is POSIX-only")
	}

	beadsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatalf("write types sentinel: %v", err)
	}

	tmpDir := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "bd.log")
	wispStartedPath := filepath.Join(tmpDir, "wisp-started")
	fakeBD := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
if [ "$1" = "sql" ]; then
  case "$3" in
    *"FROM issues i"*)
      printf '%s\n' '[{"id":"issue-visible","title":"Durable visible","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:05Z","updated_at":"2026-06-12T12:00:05Z","pinned":0,"labels_csv":"gt:message,from:mayor/","assignee_match":1,"cc_match":0}]'
      exit 0
      ;;
    *"FROM wisps w"*)
      : > "$WISP_STARTED_FILE"
      sleep 60
      printf '%s\n' '[{"id":"wisp-late","title":"Wisp late","description":"","status":"open","priority":2,"assignee":"gastown/synth","created_at":"2026-06-12T12:00:00Z","updated_at":"2026-06-12T12:00:00Z","labels_csv":"gt:message,from:mayor/","assignee_match":1,"cc_match":0}]'
      exit 0
      ;;
  esac
fi
printf 'unexpected bd args: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(fakeBD, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_LOG", logPath)
	t.Setenv("WISP_STARTED_FILE", wispStartedPath)

	oldJSON := mailInboxJSON
	mailInboxJSON = true
	t.Cleanup(func() { mailInboxJSON = oldJSON })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := mail.NewMailboxWithBeadsDir("gastown/synth", t.TempDir(), beadsDir)
	var runErr error

	stdoutFile, err := os.CreateTemp(t.TempDir(), "mail-inbox-stdout-*")
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = stdoutFile
	t.Cleanup(func() { os.Stdout = oldStdout })

	done := make(chan struct{})
	stdoutCh := make(chan string, 1)
	go func() {
		runErr = runMailInboxWithMailbox(ctx, box, "gastown/synth")
		if _, err := stdoutFile.Seek(0, 0); err != nil {
			stdoutCh <- "seek failed: " + err.Error()
			close(done)
			return
		}
		out, err := os.ReadFile(stdoutFile.Name())
		if err != nil {
			stdoutCh <- "read failed: " + err.Error()
			close(done)
			return
		}
		stdoutCh <- string(out)
		close(done)
	}()

	waitForPath(t, wispStartedPath, time.Second)
	cancel()
	<-done
	_ = stdoutFile.Close()
	stdout := <-stdoutCh
	os.Stdout = oldStdout
	if runErr == nil {
		t.Fatal("runMailInboxWithMailbox returned nil, want timeout/cancel error")
	}
	if !strings.Contains(runErr.Error(), "listing messages canceled") {
		t.Fatalf("error = %v, want listing canceled phase", runErr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no durable-only partial JSON output", stdout)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := string(logBytes)
	if got := strings.Count(log, "FROM issues i"); got != 1 {
		t.Fatalf("durable issue SQL calls = %d, want 1; log:\n%s", got, log)
	}
	if got := strings.Count(log, "FROM wisps w"); got != 1 {
		t.Fatalf("wisp SQL calls = %d, want 1; log:\n%s", got, log)
	}
}

type fakeMailReadMailbox struct {
	msg              *mail.Message
	listMessages     []*mail.Message
	getCalls         int
	listCalls        int
	markCalls        int
	lastGetContext   context.Context
	lastListContext  context.Context
	lastMarkContext  context.Context
	getErr           error
	listErr          error
	markErr          error
	blockGet         bool
	blockList        bool
	blockUntilCancel bool
	stdoutPath       string
	outputAtMark     string
	cancelOnMark     context.CancelFunc
	markStarted      chan struct{}
	markDone         chan struct{}
}

func (f *fakeMailReadMailbox) GetContext(ctx context.Context, id string) (*mail.Message, error) {
	f.getCalls++
	f.lastGetContext = ctx
	if f.blockGet {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.msg == nil || f.msg.ID != id {
		return nil, mail.ErrMessageNotFound
	}
	return f.msg, nil
}

func (f *fakeMailReadMailbox) ListContext(ctx context.Context) ([]*mail.Message, error) {
	f.listCalls++
	f.lastListContext = ctx
	if f.blockList {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listMessages, nil
}

func (f *fakeMailReadMailbox) MarkReadMessageContext(ctx context.Context, msg *mail.Message) error {
	f.markCalls++
	f.lastMarkContext = ctx
	if f.stdoutPath != "" {
		if out, err := os.ReadFile(f.stdoutPath); err == nil {
			f.outputAtMark = string(out)
		}
	}
	if f.markStarted != nil {
		close(f.markStarted)
	}
	if f.cancelOnMark != nil {
		f.cancelOnMark()
	}
	defer func() {
		if f.markDone != nil {
			close(f.markDone)
		}
	}()
	if f.blockUntilCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.markErr
}

func TestRunMailReadPrintsMessageBeforeBoundedBookkeeping(t *testing.T) {
	oldJSON := mailReadJSON
	mailReadJSON = false
	t.Cleanup(func() { mailReadJSON = oldJSON })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdoutFile, err := os.CreateTemp(t.TempDir(), "mail-read-stdout-*")
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = stdoutFile
	t.Cleanup(func() { os.Stdout = oldStdout })

	box := &fakeMailReadMailbox{
		msg: &mail.Message{
			ID:        "msg-block",
			Subject:   "Slow bookkeeping",
			From:      "mayor/",
			To:        "gastown/synth",
			Body:      "content must be visible first",
			Timestamp: time.Date(2026, 9, 14, 18, 0, 0, 0, time.UTC),
		},
		blockUntilCancel: true,
		stdoutPath:       stdoutFile.Name(),
		cancelOnMark:     cancel,
		markStarted:      make(chan struct{}),
		markDone:         make(chan struct{}),
	}

	stderr := captureStderr(t, func() {
		if err := runMailReadWithMailbox(ctx, box, "gastown/synth", []string{"msg-block"}); err != nil {
			t.Fatalf("runMailReadWithMailbox: %v", err)
		}
	})

	if box.markCalls != 1 {
		t.Fatalf("MarkReadMessageContext calls = %d, want 1", box.markCalls)
	}
	select {
	case <-box.markStarted:
	default:
		t.Fatal("mark bookkeeping was not attempted")
	}
	select {
	case <-box.markDone:
	default:
		t.Fatal("mark bookkeeping did not observe context cancellation before command returned")
	}
	for _, want := range []string{"Subject:", "Slow bookkeeping", "content must be visible first"} {
		if !strings.Contains(box.outputAtMark, want) {
			t.Fatalf("stdout at mark start missing %q:\n%s", want, box.outputAtMark)
		}
	}
	if !strings.Contains(stderr, "could not mark message as read: context canceled") {
		t.Fatalf("stderr missing bounded warning, got:\n%s", stderr)
	}
}

func TestRunMailReadJSONRemainsValidWhenBookkeepingFails(t *testing.T) {
	oldJSON := mailReadJSON
	mailReadJSON = true
	t.Cleanup(func() { mailReadJSON = oldJSON })

	box := &fakeMailReadMailbox{
		msg: &mail.Message{
			ID:        "msg-json",
			Subject:   "JSON survives warning",
			From:      "mayor/",
			To:        "gastown/synth",
			Body:      "payload",
			Timestamp: time.Date(2026, 9, 14, 18, 1, 0, 0, time.UTC),
		},
		markErr: errors.New("label write failed"),
	}

	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := runMailReadWithMailbox(context.Background(), box, "gastown/synth", []string{"msg-json"}); err != nil {
				t.Fatalf("runMailReadWithMailbox: %v", err)
			}
		})
	})

	var got mail.Message
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid message JSON: %v\n%s", err, stdout)
	}
	if got.ID != "msg-json" || got.Subject != "JSON survives warning" {
		t.Fatalf("unexpected JSON payload: %+v", got)
	}
	if strings.Contains(stdout, "could not mark message") {
		t.Fatalf("warning contaminated JSON stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "could not mark message as read: label write failed") {
		t.Fatalf("stderr missing mark-read warning, got:\n%s", stderr)
	}
}

func TestRunMailReadReturnsPhaseSpecificGetTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	box := &fakeMailReadMailbox{blockGet: true}
	stdout := captureStdout(t, func() {
		err := runMailReadWithMailbox(ctx, box, "gastown/synth", []string{"msg-timeout"})
		if err == nil {
			t.Fatal("runMailReadWithMailbox returned nil, want timeout error")
		}
		if !strings.Contains(err.Error(), "getting message timed out after") {
			t.Fatalf("error = %v, want getting phase timeout", err)
		}
		if box.markCalls != 0 {
			t.Fatalf("MarkReadMessageContext calls = %d, want 0 before output", box.markCalls)
		}
	})
	if stdout != "" {
		t.Fatalf("stdout = %q, want no output before get succeeds", stdout)
	}
}

func TestRunMailReadReturnsPhaseSpecificListTimeoutForIndex(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	box := &fakeMailReadMailbox{blockList: true}
	stdout := captureStdout(t, func() {
		err := runMailReadWithMailbox(ctx, box, "gastown/synth", []string{"1"})
		if err == nil {
			t.Fatal("runMailReadWithMailbox returned nil, want timeout error")
		}
		if !strings.Contains(err.Error(), "listing messages timed out after") {
			t.Fatalf("error = %v, want listing phase timeout", err)
		}
		if box.getCalls != 0 {
			t.Fatalf("GetContext calls = %d, want 0 after list cancellation", box.getCalls)
		}
	})
	if stdout != "" {
		t.Fatalf("stdout = %q, want no output before list succeeds", stdout)
	}
}

type fakeMailDeleteMailbox struct {
	deleteCalls    int
	lastContext    context.Context
	blockDelete    bool
	cancelOnDelete context.CancelFunc
}

func (f *fakeMailDeleteMailbox) ListContext(ctx context.Context) ([]*mail.Message, error) {
	return nil, nil
}

func (f *fakeMailDeleteMailbox) DeleteContext(ctx context.Context, id string) error {
	f.deleteCalls++
	f.lastContext = ctx
	if f.cancelOnDelete != nil {
		f.cancelOnDelete()
	}
	if f.blockDelete {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestRunMailDeleteUsesSharedCommandContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := &fakeMailDeleteMailbox{blockDelete: true, cancelOnDelete: cancel}
	start := time.Now()
	stdout := captureStdout(t, func() {
		err := runMailDeleteWithMailbox(ctx, box, []string{"msg-slow"}, os.Stdout)
		if err == nil {
			t.Fatal("runMailDeleteWithMailbox returned nil, want cancellation error")
		}
		if !strings.Contains(err.Error(), "failed to delete 1 messages") {
			t.Fatalf("error = %v, want delete failure", err)
		}
	})
	elapsed := time.Since(start)
	if !strings.Contains(stdout, "deleting message canceled") {
		t.Fatalf("stdout = %q, want deleting phase cancellation", stdout)
	}
	if box.lastContext != ctx {
		t.Fatal("DeleteContext did not receive command context")
	}
	if box.deleteCalls != 1 {
		t.Fatalf("DeleteContext calls = %d, want 1", box.deleteCalls)
	}
	if elapsed > time.Second {
		t.Fatalf("runMailDeleteWithMailbox took %s after canceled context", elapsed)
	}
}

func waitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
