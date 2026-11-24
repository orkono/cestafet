package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var nextPID = 1000

func register(t *testing.T, s *Store, id, project string) Session {
	t.Helper()
	nextPID++
	in := Session{ID: id, ClaudePID: nextPID, Profile: "p-" + id, Project: project, Cwd: "/w/" + id}
	if _, err := s.Register(in); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return got
}

func TestOpenCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bus.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows has no Unix modes; the file is protected by the profile's ACL there.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Fatalf("db mode = %o, want 600", perm)
	}
}

func TestRegisterKeepsSummaryAndRoleOnRefresh(t *testing.T) {
	s := newStore(t)
	register(t, s, "a", "proj")
	if err := s.SetSummary("a", "auth refactor"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("a"); err != nil {
		t.Fatal(err)
	}
	isNew, err := s.Register(Session{ID: "a", ClaudePID: 999, Profile: "p", Project: "proj", Cwd: "/w"})
	if err != nil || isNew {
		t.Fatalf("re-register: new=%v err=%v", isNew, err)
	}
	got, _ := s.Get("a")
	if got.Summary != "auth refactor" || got.Role != RoleCoordinator || got.ClaudePID != 999 {
		t.Fatalf("unexpected session after refresh: %+v", got)
	}
}

func TestSingleCoordinatorPerProject(t *testing.T) {
	s := newStore(t)
	register(t, s, "a", "proj")
	register(t, s, "b", "proj")
	if prev, err := s.MakeCoordinator("a"); err != nil || prev != "" {
		t.Fatalf("first coordinator: prev=%q err=%v", prev, err)
	}
	prev, err := s.MakeCoordinator("b")
	if err != nil || prev != "a" {
		t.Fatalf("takeover: prev=%q err=%v", prev, err)
	}
	c, err := s.Coordinator("proj")
	if err != nil || c.ID != "b" {
		t.Fatalf("coordinator = %+v, %v", c, err)
	}
	a, _ := s.Get("a")
	if a.Role != RoleMember {
		t.Fatalf("old coordinator role = %s", a.Role)
	}
}

func TestSendReachesOtherSessionsOfProjectOnly(t *testing.T) {
	s := newStore(t)
	from := register(t, s, "coord", "proj")
	register(t, s, "m1", "proj")
	register(t, s, "m2", "proj")
	register(t, s, "other", "elsewhere")

	id, to, err := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: "no deploys to staging"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(to) != 2 {
		t.Fatalf("recipients = %v, want m1 and m2", to)
	}
	for _, sid := range []string{"m1", "m2"} {
		got, err := s.TakePending(sid)
		if err != nil || len(got) != 1 || got[0].ID != id {
			t.Fatalf("pending for %s = %+v, %v", sid, got, err)
		}
		again, _ := s.TakePending(sid)
		if len(again) != 0 {
			t.Fatalf("notice delivered twice to %s", sid)
		}
	}
	for _, sid := range []string{"coord", "other"} {
		if got, _ := s.TakePending(sid); len(got) != 0 {
			t.Fatalf("%s must not receive the notice", sid)
		}
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	s := newStore(t)
	from := register(t, s, "a", "proj")
	register(t, s, "x", "elsewhere")
	cases := map[string]NewNotice{
		"empty":         {From: from, Origin: OriginAgent, Body: "   "},
		"too long":      {From: from, Origin: OriginAgent, Body: strings.Repeat("x", MaxBodyBytes+1)},
		"bad origin":    {From: from, Origin: "boss", Body: "hi"},
		"other project": {From: from, Origin: OriginAgent, Body: "hi", To: []string{"x"}},
	}
	for name, n := range cases {
		if _, _, err := s.Send(n); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestUserOriginCarriesPromptText(t *testing.T) {
	s := newStore(t)
	from := register(t, s, "coord", "proj")
	register(t, s, "m", "proj")
	pid, err := s.RecordPrompt("coord", "tell the others: testing started")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Send(NewNotice{From: from, Origin: OriginUser, PromptID: pid, Body: "Testing started."}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.TakePending("m")
	if len(got) != 1 || got[0].Origin != OriginUser || got[0].PromptText != "tell the others: testing started" {
		t.Fatalf("notice = %+v", got)
	}
}

func TestBackfillGivesLateSessionRecentNotices(t *testing.T) {
	s := newStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	from := register(t, s, "coord", "proj")
	if _, _, err := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: "old"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(BackfillWindow + time.Hour)
	if _, _, err := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: "recent"}); err != nil {
		t.Fatal(err)
	}
	register(t, s, "late", "proj")
	if err := s.Backfill("late", "proj"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.TakePending("late")
	if len(got) != 1 || got[0].Body != "recent" {
		t.Fatalf("backfilled = %+v", got)
	}
}

func TestAckAndStatus(t *testing.T) {
	s := newStore(t)
	from := register(t, s, "coord", "proj")
	register(t, s, "m", "proj")
	id, _, _ := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: "hi"})
	if err := s.Ack(id, "m", "ok, paused deploys"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(id, "coord", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack by sender: %v", err)
	}
	st, err := s.Status(id)
	if err != nil || len(st) != 1 || st[0].AckedAt.IsZero() || st[0].Reply != "ok, paused deploys" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if unacked, _ := s.Received("m", true, 10); len(unacked) != 0 {
		t.Fatalf("acked notice still listed as unacked")
	}
}

func TestIdleAndGone(t *testing.T) {
	s := newStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	register(t, s, "a", "proj")
	now = now.Add(IdleAfter + time.Minute)
	a, _ := s.Get("a")
	if a.Status != StatusIdle {
		t.Fatalf("status = %s, want idle", a.Status)
	}
	if err := s.End("a"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List("proj")
	if len(list) != 0 {
		t.Fatalf("gone session still listed: %+v", list)
	}
	if _, err := s.ByClaudePID(a.ClaudePID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("gone session found by pid: %v", err)
	}
}

func TestNewSessionInSameProcessSupersedesOld(t *testing.T) {
	s := newStore(t)
	if _, err := s.Register(Session{ID: "before", ClaudePID: 77, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("before"); err != nil {
		t.Fatal(err)
	}
	// /clear: same claude process, new session id
	if _, err := s.Register(Session{ID: "after", ClaudePID: 77, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	old, _ := s.Get("before")
	if old.Status != StatusGone || old.Role != RoleMember {
		t.Fatalf("old session = %+v", old)
	}
	c, err := s.Coordinator("proj")
	if err != nil || c.ID != "after" {
		t.Fatalf("coordinator after /clear = %+v, %v", c, err)
	}
	if got, _ := s.ByClaudePID(77); got.ID != "after" {
		t.Fatalf("ByClaudePID = %s", got.ID)
	}
}

func TestTakePendingWithinSplitsLargeBacklog(t *testing.T) {
	s := newStore(t)
	from := register(t, s, "coord", "proj")
	register(t, s, "m", "proj")
	var sent []int64
	for i := 0; i < 5; i++ {
		body := strings.Repeat(string(rune('a'+i)), MaxBodyBytes)
		id, _, err := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, id)
	}
	var got []int64
	for round := 0; round < 5 && len(got) < len(sent); round++ {
		batch, left, err := s.TakePendingWithin("m", 12*1024)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			t.Fatalf("round %d delivered nothing with %d left", round, left)
		}
		size := 0
		for _, n := range batch {
			size += len(n.Body)
			got = append(got, n.ID)
		}
		if len(batch) > 1 && size > 12*1024 {
			t.Fatalf("round %d injected %d bytes", round, size)
		}
		if left != len(sent)-len(got) {
			t.Fatalf("round %d: left = %d, want %d", round, left, len(sent)-len(got))
		}
	}
	if len(got) != len(sent) {
		t.Fatalf("delivered %v, want %v", got, sent)
	}
	for i := range sent {
		if got[i] != sent[i] {
			t.Fatalf("order %v, want %v", got, sent)
		}
	}
	if rest, left, _ := s.TakePendingWithin("m", 12*1024); len(rest) != 0 || left != 0 {
		t.Fatalf("leftover %d notices, left=%d", len(rest), left)
	}
}

func TestTakePendingWithinAlwaysDeliversOne(t *testing.T) {
	s := newStore(t)
	from := register(t, s, "coord", "proj")
	register(t, s, "m", "proj")
	if _, _, err := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: strings.Repeat("x", MaxBodyBytes)}); err != nil {
		t.Fatal(err)
	}
	batch, left, err := s.TakePendingWithin("m", 100)
	if err != nil || len(batch) != 1 || left != 0 {
		t.Fatalf("batch=%d left=%d err=%v", len(batch), left, err)
	}
}

func TestReusedClaudePidDoesNotInheritASession(t *testing.T) {
	s := newStore(t)
	old := Session{ID: "old", ClaudePID: 500, ClaudeStarted: 1000, Profile: "p", Project: "proj", Cwd: "/w"}
	if _, err := s.Register(old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("old"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ByOwner(500, 2000); err == nil {
		t.Fatal("a younger process with the same pid matched the old session")
	}
	if got, err := s.ByOwner(500, 1000); err != nil || got.ID != "old" {
		t.Fatalf("own process: %+v, %v", got, err)
	}
	// The new claude process with the reused pid registers: the dead process's
	// session is retired, but its coordinator role does not move over.
	fresh := Session{ID: "fresh", ClaudePID: 500, ClaudeStarted: 2000, Profile: "p", Project: "proj", Cwd: "/w"}
	if _, err := s.Register(fresh); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("fresh")
	if got.Role == RoleCoordinator {
		t.Fatal("coordinator role moved to a different process that reused the pid")
	}
	if o, _ := s.Get("old"); o.Status != StatusGone {
		t.Fatalf("old session status = %q, want gone", o.Status)
	}
}

func TestClearKeepsCoordinatorWithKnownStartTime(t *testing.T) {
	s := newStore(t)
	if _, err := s.Register(Session{ID: "a", ClaudePID: 600, ClaudeStarted: 1000, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(Session{ID: "b", ClaudePID: 600, ClaudeStarted: 1000, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("b"); got.Role != RoleCoordinator {
		t.Fatalf("/clear in the same process lost the coordinator role: %+v", got)
	}
	if got, err := s.ByOwner(600, 0); err != nil || got.ID != "b" {
		t.Fatalf("unknown start time (Unix) must still match by pid: %+v, %v", got, err)
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Osman Hömek #1 100%")
	path := filepath.Join(dir, "bus.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	from := register(t, s, "a", "proj")
	register(t, s, "b", "proj")
	if _, _, err := s.Send(NewNotice{From: from, Origin: OriginAgent, Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) == 0 || names[0] != "bus.db" {
		t.Fatalf("files in %q = %v, want bus.db (and WAL files)", dir, names)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("database at %q: %v", path, err)
	}
}

func TestConcurrentOpenMigratesCleanly(t *testing.T) {
	openConcurrently := func(t *testing.T, path string) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				st, err := Open(path)
				if err != nil {
					errs <- err
					return
				}
				_ = st.Close()
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent Open(%q): %v", path, err)
		}
	}

	// Brand-new database opened simultaneously (e.g. MCP server + SessionStart hook).
	for i := 0; i < 5; i++ {
		openConcurrently(t, filepath.Join(t.TempDir(), "bus.db"))
	}

	// Legacy v0.1.0 database upgraded simultaneously by several processes.
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	dsn, err := fileURI(legacy)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notices (
		id INTEGER PRIMARY KEY AUTOINCREMENT, project TEXT NOT NULL, from_session TEXT NOT NULL,
		from_profile TEXT NOT NULL, from_role TEXT NOT NULL, origin TEXT NOT NULL, prompt_id INTEGER,
		body TEXT NOT NULL, created_at INTEGER NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	openConcurrently(t, legacy)
}

func TestTakePendingWithinDeliversOnceConcurrently(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bus.db")
	s0, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s0.Close()
	from := register(t, s0, "coord", "proj")
	register(t, s0, "worker", "proj")

	handles := make([]*Store, 4)
	for i := range handles {
		h, err := Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		handles[i] = h
	}

	for round := 0; round < 20; round++ {
		if _, _, err := s0.Send(NewNotice{From: from, Origin: OriginAgent, Body: "hold deploys"}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var delivered atomic.Int32
		errs := make(chan error, len(handles))
		start := make(chan struct{})
		for _, h := range handles {
			wg.Add(1)
			go func(st *Store) {
				defer wg.Done()
				<-start
				got, _, err := st.TakePendingWithin("worker", 12*1024)
				if err != nil {
					errs <- err
					return
				}
				delivered.Add(int32(len(got)))
			}(h)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		if n := delivered.Load(); n != 1 {
			t.Fatalf("round %d: delivered %d times, want 1", round, n)
		}
	}
}

func TestRegisterDropsCoordinatorOnProjectChange(t *testing.T) {
	s := newStore(t)
	if _, err := s.Register(Session{ID: "coordA", ClaudePID: 10, Profile: "p1", Project: "projA", Cwd: "/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(Session{ID: "coordB", ClaudePID: 20, Profile: "p2", Project: "projB", Cwd: "/b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("coordA"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("coordB"); err != nil {
		t.Fatal(err)
	}
	// Refreshing coordA in another project (cd or projects.toml change) must drop its coordinator role.
	if isNew, err := s.Register(Session{ID: "coordA", ClaudePID: 10, Profile: "p1", Project: "projB", Cwd: "/b"}); err != nil || isNew {
		t.Fatalf("refresh: new=%v err=%v", isNew, err)
	}
	gotA, _ := s.Get("coordA")
	if gotA.Project != "projB" || gotA.Role != RoleMember {
		t.Fatalf("coordA after project switch = %+v, want projB member", gotA)
	}
	c, err := s.Coordinator("projB")
	if err != nil || c.ID != "coordB" {
		t.Fatalf("projB coordinator = %+v, %v; want coordB", c, err)
	}
}

func TestSupersedeDoesNotCarryCoordinatorAcrossProjects(t *testing.T) {
	s := newStore(t)
	if _, err := s.Register(Session{ID: "oldA", ClaudePID: 30, ClaudeStarted: 1000, Profile: "p1", Project: "projA", Cwd: "/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(Session{ID: "coordB", ClaudePID: 40, ClaudeStarted: 1000, Profile: "p2", Project: "projB", Cwd: "/b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("oldA"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("coordB"); err != nil {
		t.Fatal(err)
	}
	// Same process starts a new session in projB: oldA is retired, role does not move into projB.
	if _, err := s.Register(Session{ID: "newB", ClaudePID: 30, ClaudeStarted: 1000, Profile: "p1", Project: "projB", Cwd: "/b"}); err != nil {
		t.Fatal(err)
	}
	if old, _ := s.Get("oldA"); old.Status != StatusGone || old.Role != RoleMember {
		t.Fatalf("oldA = %+v, want gone member", old)
	}
	if got, _ := s.Get("newB"); got.Role != RoleMember {
		t.Fatalf("newB inherited coordinator across projects: %+v", got)
	}
	if c, err := s.Coordinator("projB"); err != nil || c.ID != "coordB" {
		t.Fatalf("projB coordinator = %+v, %v; want coordB", c, err)
	}
}

func TestResumeSupersedesPreviousSessionOnSamePID(t *testing.T) {
	s := newStore(t)
	if _, err := s.Register(Session{ID: "earlier", ClaudePID: 50, ClaudeStarted: 1000, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	if err := s.End("earlier"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(Session{ID: "current", ClaudePID: 50, ClaudeStarted: 1000, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MakeCoordinator("current"); err != nil {
		t.Fatal(err)
	}
	// Resuming the earlier session id in the same claude process must retire "current"
	// and keep the coordinator role in the same project.
	if isNew, err := s.Register(Session{ID: "earlier", ClaudePID: 50, ClaudeStarted: 1000, Profile: "p", Project: "proj", Cwd: "/w"}); err != nil || isNew {
		t.Fatalf("resume: new=%v err=%v", isNew, err)
	}
	if cur, _ := s.Get("current"); cur.Status != StatusGone || cur.Role != RoleMember {
		t.Fatalf("superseded session = %+v, want gone member", cur)
	}
	if res, _ := s.Get("earlier"); res.Status != StatusLive || res.Role != RoleCoordinator {
		t.Fatalf("resumed session = %+v, want live coordinator", res)
	}
	if got, err := s.ByOwner(50, 1000); err != nil || got.ID != "earlier" {
		t.Fatalf("ByOwner = %+v, %v; want earlier", got, err)
	}
}
