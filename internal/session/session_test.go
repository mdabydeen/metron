package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdabydeen/metron/internal/ollama"
)

func store(t *testing.T) Store {
	t.Helper()
	return Store{Root: t.TempDir()}
}

func meta(id string) Meta {
	return Meta{ID: id, Started: time.Unix(0, 0).UTC(), Model: "m"}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	s := store(t)
	msgs := []ollama.Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolName: "view_slice", Content: "    1 | x"},
	}

	if err := s.Save(meta("20260101-000001"), msgs); err != nil {
		t.Fatal(err)
	}
	gotMeta, gotMsgs, err := s.Load("20260101-000001")
	if err != nil {
		t.Fatal(err)
	}

	if gotMeta.ID != "20260101-000001" || gotMeta.Model != "m" {
		t.Fatalf("meta = %+v, want the saved metadata", gotMeta)
	}
	if len(gotMsgs) != len(msgs) {
		t.Fatalf("loaded %d messages, want %d", len(gotMsgs), len(msgs))
	}
	// ToolName pairs a result with the call it answers; losing it across a save
	// would leave a resumed model guessing.
	if gotMsgs[2].ToolName != "view_slice" {
		t.Fatalf("message = %+v, want the tool name preserved", gotMsgs[2])
	}
}

func TestSaveIgnoresItselfInGit(t *testing.T) {
	s := store(t)

	if err := s.Save(meta("20260101-000001"), nil); err != nil {
		t.Fatal(err)
	}

	// metron writes into a directory it does not own. A tool that leaves
	// untracked files in someone's repository on every run is one people stop
	// running, so the directory ignores itself without needing the project's
	// cooperation.
	b, err := os.ReadFile(filepath.Join(s.Root, Dir, ".gitignore"))
	if err != nil {
		t.Fatalf("no self-ignore written: %v", err)
	}
	if strings.TrimSpace(string(b)) != "*" {
		t.Fatalf(".gitignore = %q, want it to ignore everything", b)
	}
}

func TestSaveIsAtomicAndReplacesThePreviousCopy(t *testing.T) {
	s := store(t)
	if err := s.Save(meta("20260101-000001"), []ollama.Message{{Role: "user", Content: "first"}}); err != nil {
		t.Fatal(err)
	}

	// Compaction rewrites messages in place and trimming drops them, so the
	// transcript is replaced rather than appended to.
	if err := s.Save(meta("20260101-000001"), []ollama.Message{{Role: "user", Content: "second"}}); err != nil {
		t.Fatal(err)
	}

	_, msgs, err := s.Load("20260101-000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "second" {
		t.Fatalf("messages = %+v, want only the latest history", msgs)
	}
	// No temporary files left behind.
	entries, _ := os.ReadDir(s.dir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("left a temporary file behind: %s", e.Name())
		}
	}
}

func TestListIsNewestFirstAndLatestAgrees(t *testing.T) {
	s := store(t)
	for _, id := range []string{"20260101-000000", "20260301-000000", "20260201-000000"} {
		if err := s.Save(meta(id), nil); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"20260301-000000", "20260201-000000", "20260101-000000"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("List() = %v, want %v", ids, want)
		}
	}
	latest, err := s.Latest()
	if err != nil || latest != want[0] {
		t.Fatalf("Latest() = %q, %v, want %q", latest, err, want[0])
	}
}

func TestListAndLatestOnAnEmptyStore(t *testing.T) {
	s := store(t)

	ids, err := s.List()
	if err != nil || len(ids) != 0 {
		t.Fatalf("List() = %v, %v, want empty and no error before anything is saved", ids, err)
	}
	latest, err := s.Latest()
	if err != nil || latest != "" {
		t.Fatalf("Latest() = %q, %v, want empty", latest, err)
	}
}

func TestLoadRejectsAMissingOrMalformedSession(t *testing.T) {
	s := store(t)
	if _, _, err := s.Load("20260101-999999"); err == nil {
		t.Fatal("Load() = nil error for a session that does not exist")
	}

	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"20260101-000003": "{not json}\n",
		"20260101-000004": `{"id":"20260101-000004"}` + "\n{not json}\n",
		"20260101-000005": "\n",
	} {
		if err := os.WriteFile(s.Path(name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Load(name); err == nil {
			t.Errorf("Load(%q) = nil error, want a malformed transcript rejected", name)
		}
	}
}

func TestLoadSkipsBlankLines(t *testing.T) {
	s := store(t)
	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"20260101-000006","model":"m"}` + "\n\n" + `{"role":"user","content":"hi"}` + "\n\n"
	if err := os.WriteFile(s.Path("20260101-000006"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, msgs, err := s.Load("20260101-000006")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("loaded %d messages, want blank lines skipped", len(msgs))
	}
}

func TestNewIDIsSortableAndTimeBased(t *testing.T) {
	early := NewID(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	late := NewID(time.Date(2026, 9, 3, 21, 0, 0, 0, time.UTC))

	if early != "20260102-030405" {
		t.Fatalf("NewID() = %q, want a readable timestamp", early)
	}
	// List sorts lexically and calls it chronological; that only holds if the
	// ids are fixed-width timestamps.
	if early >= late {
		t.Fatalf("ids do not sort chronologically: %q !< %q", early, late)
	}
}

// gitRepo makes a scratch repository with one commit and returns its root.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"-c", "commit.gpgsign=false", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestHeadStampsTheCommitAndDirtiness(t *testing.T) {
	dir := gitRepo(t)

	rev, dirty := Head(dir)
	if rev == "" {
		t.Fatal("Head() returned no revision inside a repository")
	}
	if dirty {
		t.Fatal("Head() reported a clean tree as dirty")
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, dirty = Head(dir); !dirty {
		t.Fatal("Head() did not notice the modified tree")
	}
}

func TestHeadOutsideARepository(t *testing.T) {
	rev, dirty := Head(t.TempDir())
	if rev != "" || dirty {
		t.Fatalf("Head() = %q, %v outside a repository, want empty", rev, dirty)
	}
}

func TestDriftWarningExplainsWhyAResumeMayNotLineUp(t *testing.T) {
	dir := gitRepo(t)
	rev, _ := Head(dir)

	// A transcript full of line numbers and quoted code is only meaningful
	// against the tree it was recorded on.
	if got := DriftWarning(Meta{GitHead: rev}, dir); got != "" {
		t.Fatalf("DriftWarning() = %q, want silence when nothing moved", got)
	}
	if got := DriftWarning(Meta{}, dir); got != "" {
		t.Fatalf("DriftWarning() = %q, want silence with nothing stamped", got)
	}
	if got := DriftWarning(Meta{GitHead: "0123456789abcdef"}, dir); !strings.Contains(got, "has moved") {
		t.Fatalf("DriftWarning() = %q, want the moved head reported", got)
	}
	if got := DriftWarning(Meta{GitHead: rev}, t.TempDir()); !strings.Contains(got, "not one") {
		t.Fatalf("DriftWarning() = %q, want the missing repository reported", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DriftWarning(Meta{GitHead: rev}, dir); !strings.Contains(got, "uncommitted") {
		t.Fatalf("DriftWarning() = %q, want the newly dirty tree reported", got)
	}
	// Already dirty when saved: no new information, so no warning.
	if got := DriftWarning(Meta{GitHead: rev, Dirty: true}, dir); got != "" {
		t.Fatalf("DriftWarning() = %q, want silence when it was already dirty", got)
	}
}

func TestSaveReportsAnUnwritableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := (Store{Root: dir}).Save(meta("20260101-000001"), nil); err == nil {
		t.Fatal("Save() = nil error into an unwritable root")
	}
}

func TestShortLeavesShortRevisionsAlone(t *testing.T) {
	if got := short("abc"); got != "abc" {
		t.Fatalf("short() = %q, want a short revision untouched", got)
	}
}

// failingWriter fails after letting n writes through, so both encode paths in
// writeTranscript can be reached without filling a disk.
type failingWriter struct{ allow int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.allow == 0 {
		return 0, errors.New("disk full")
	}
	f.allow--
	return len(p), nil
}

func TestWriteTranscriptReportsWriteFailures(t *testing.T) {
	msgs := []ollama.Message{{Role: "user", Content: "hi"}}

	if err := writeTranscript(&failingWriter{allow: 0}, meta("20260101-000002"), msgs); err == nil ||
		!strings.Contains(err.Error(), "metadata") {
		t.Fatalf("writeTranscript() = %v, want the metadata failure named", err)
	}
	if err := writeTranscript(&failingWriter{allow: 1}, meta("20260101-000002"), msgs); err == nil ||
		!strings.Contains(err.Error(), "message") {
		t.Fatalf("writeTranscript() = %v, want the message failure named", err)
	}
}

func TestSaveReportsAnUnwritableSessionDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	s := store(t)
	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	// The directory exists but cannot be written to, so the temporary file
	// cannot be created -- a different failure from not being able to mkdir.
	if err := os.Chmod(s.dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.dir(), 0o700) })

	err := s.Save(meta("20260101-000001"), nil)
	if err == nil || !strings.Contains(err.Error(), "write session file") {
		t.Fatalf("Save() = %v, want the write failure reported", err)
	}
}

func TestListReportsAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	s := store(t)
	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.dir(), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.dir(), 0o700) })

	if _, err := s.List(); err == nil {
		t.Fatal("List() = nil error for an unreadable directory")
	}
	if _, err := s.Latest(); err == nil {
		t.Fatal("Latest() = nil error for an unreadable directory")
	}
}

func TestLoadReportsAnOverlongLine(t *testing.T) {
	s := store(t)
	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	// One line past the scanner's ceiling: the transcript is unreadable, and
	// saying so beats returning a silently truncated conversation.
	body := `{"id":"20260101-000007"}` + "\n" + strings.Repeat("x", maxLine+1) + "\n"
	if err := os.WriteFile(s.Path("20260101-000007"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Load("20260101-000007"); err == nil {
		t.Fatal("Load() = nil error for a transcript with an unreadable line")
	}
}

func TestLoadRejectsAnIdThatIsAPath(t *testing.T) {
	s := store(t)

	// Path joins the id onto a directory, so an id that is really a path would
	// read outside the store. Operator-supplied via --resume, so this is
	// hygiene rather than a model boundary -- but it costs nothing to hold.
	for _, id := range []string{"../../../etc/passwd", "..", "a/b", "", "not-a-timestamp"} {
		if _, _, err := s.Load(id); err == nil {
			t.Errorf("Load(%q) = nil error, want a malformed id refused", id)
		}
	}
	if !ValidID("20260903-142530") {
		t.Fatal("ValidID rejected a well-formed id")
	}
}

func TestSavedTranscriptsAreNotWorldReadable(t *testing.T) {
	s := store(t)
	if err := s.Save(meta("20260101-000001"), []ollama.Message{
		{Role: "tool", Content: "contents of whatever the model read"},
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(s.Path("20260101-000001"))
	if err != nil {
		t.Fatal(err)
	}
	// A transcript holds every tool result the model saw, which is every file
	// it read.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %v, want 0600 for a file holding read file contents", perm)
	}
}
