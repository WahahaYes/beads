package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/blobstore"
	"github.com/steveyegge/beads/internal/execenv"
	"github.com/steveyegge/beads/internal/githooksenv"
	"github.com/steveyegge/beads/internal/storage"
)

// TestResetDataRefNamesMatchDolt keeps the locally pinned data-plane ref
// names in sync with the constants dolt's git blobstore actually publishes
// (store/blobstore/git_refs.go). If a dolt bump renames either ref, this
// fails instead of reset-data silently deleting the wrong refs.
func TestResetDataRefNamesMatchDolt(t *testing.T) {
	if gitDoltDataRef != blobstore.DoltDataRef {
		t.Errorf("gitDoltDataRef = %q, dolt publishes %q", gitDoltDataRef, blobstore.DoltDataRef)
	}
	if want := "refs/heads/" + blobstore.DefaultInfoBranch; gitDoltInfoRef != want {
		t.Errorf("gitDoltInfoRef = %q, dolt publishes %q", gitDoltInfoRef, want)
	}
}

func TestEnvWithNoGitHooksPreservesExistingParameters(t *testing.T) {
	const existing = "'user.email=ci@example.com'"
	t.Setenv(githooksenv.ParametersEnv, existing)
	t.Setenv("BEADS_TEST_NO_HOOKS_KEEP", "GIT_CONFIG_PARAMETERS=unrelated-value")

	// Build the expected slice independently of Without/DisabledEnv so a
	// stale entry cannot hide behind Extract's last-wins lookup.
	var want []string
	for _, entry := range os.Environ() {
		if !execenv.KeyEqual(execenv.EntryKey(entry), githooksenv.ParametersEnv) {
			want = append(want, entry)
		}
	}
	want = append(want, githooksenv.ParametersEnv+"="+existing+" "+githooksenv.NoHooksParam)
	if got := envWithNoGitHooks(); !slices.Equal(got, want) {
		t.Fatalf("envWithNoGitHooks() differs from the expected environment: got %d entries, want %d", len(got), len(want))
	}
}

func TestClassifyResetDataRemote(t *testing.T) {
	makeBareGitRepo := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	makeDoltFileStore := func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	bareRepo := makeBareGitRepo(t)
	fileStore := makeDoltFileStore(t)
	emptyDir := t.TempDir()
	strangeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(strangeDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		url     string
		want    resetDataKind
		wantErr bool
	}{
		{"git+https", "git+https://github.com/org/repo.git", resetDataGitBacked, false},
		{"git+ssh", "git+ssh://git@github.com/org/repo.git", resetDataGitBacked, false},
		{"scp-style", "git@github.com:org/repo.git", resetDataGitBacked, false},
		{"git+file", "git+file://" + bareRepo, resetDataGitBacked, false},
		{"file url to bare git repo", "file://" + bareRepo, resetDataGitBacked, false},
		{"bare path to bare git repo", bareRepo, resetDataGitBacked, false},
		{"file url to dolt store", "file://" + fileStore, resetDataFileStore, false},
		{"bare path to dolt store", fileStore, resetDataFileStore, false},
		{"file url to missing dir", "file://" + filepath.Join(emptyDir, "nope"), resetDataFileAbsent, false},
		{"file url to empty dir", "file://" + emptyDir, resetDataFileAbsent, false},
		{"file url to unrecognized dir", "file://" + strangeDir, 0, true},
		{"aws", "aws://[table:bucket]/db", resetDataUnsupported, false},
		{"gs", "gs://bucket/db", resetDataUnsupported, false},
		{"dolthub", "dolthub://org/db", resetDataUnsupported, false},
		{"https hosted", "https://doltremoteapi.dolthub.com/org/db", resetDataUnsupported, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifyResetDataRemote(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("classifyResetDataRemote(%q) = %v, want error", tt.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyResetDataRemote(%q): %v", tt.url, err)
			}
			if got != tt.want {
				t.Errorf("classifyResetDataRemote(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestResetDataGitURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git+https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"git+ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git"},
		{"git+file:///tmp/remote.git", "file:///tmp/remote.git"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"file:///tmp/remote.git", "file:///tmp/remote.git"},
		{"/tmp/remote.git", "file:///tmp/remote.git"},
	}
	for _, tt := range tests {
		if got := resetDataGitURL(tt.url); got != tt.want {
			t.Errorf("resetDataGitURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestMoveGitRefReplacesOnlyTheValueReadFirst pins the swap the server-mode
// reset-data ends with: the data ref moves to the fresh store's commit in one
// update, and only from the value reset-data read before it pushed.
func TestMoveGitRefReplacesOnlyTheValueReadFirst(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	forEachGitObjectFormat(t, func(t *testing.T, initFlags []string) {
		testMoveGitRef(t, run, initFlags)
	})
}

func testMoveGitRef(t *testing.T, run func(t *testing.T, dir string, args ...string) string, initFlags []string) {
	setup := func(t *testing.T) (remote, url, oldOID, newOID string) {
		base := t.TempDir()
		remote = filepath.Join(base, "remote.git")
		run(t, base, append([]string{"init", "-q", "--bare"}, append(initFlags, remote)...)...)
		work := filepath.Join(base, "work")
		run(t, base, append([]string{"init", "-q"}, append(initFlags, work)...)...)
		for i, ref := range []string{"refs/dolt/data", "refs/dolt/bd-reset-data-test"} {
			run(t, work, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", strconv.Itoa(i))
			run(t, work, "push", "-q", remote, "HEAD:"+ref)
		}
		oid := func(ref string) string { return run(t, remote, "rev-parse", ref) }
		return remote, "file://" + remote, oid("refs/dolt/data"), oid("refs/dolt/bd-reset-data-test")
	}
	ctx := context.Background()
	t.Run("moves from the value read first", func(t *testing.T) {
		remote, url, oldOID, newOID := setup(t)
		if err := moveGitRef(ctx, url, "refs/dolt/bd-reset-data-test", "refs/dolt/data", oldOID); err != nil {
			t.Fatal(err)
		}
		if got := run(t, remote, "rev-parse", "refs/dolt/data"); got != newOID {
			t.Errorf("refs/dolt/data = %s, want the fresh store %s", got, newOID)
		}
	})
	t.Run("refuses when the data ref moved since", func(t *testing.T) {
		remote, url, oldOID, _ := setup(t)
		if err := moveGitRef(ctx, url, "refs/dolt/bd-reset-data-test", "refs/dolt/data", strings.Repeat("1", len(oldOID))); err == nil {
			t.Fatal("moveGitRef replaced a data ref another client had moved")
		}
		if got := run(t, remote, "rev-parse", "refs/dolt/data"); got != oldOID {
			t.Errorf("refs/dolt/data = %s, want it untouched at %s", got, oldOID)
		}
	})
	t.Run("creates an absent data ref, and only an absent one", func(t *testing.T) {
		remote, url, oldOID, newOID := setup(t)
		run(t, remote, "update-ref", "-d", "refs/dolt/data")
		if err := moveGitRef(ctx, url, "refs/dolt/bd-reset-data-test", "refs/dolt/data", ""); err != nil {
			t.Fatal(err)
		}
		if got := run(t, remote, "rev-parse", "refs/dolt/data"); got != newOID {
			t.Errorf("refs/dolt/data = %s, want %s", got, newOID)
		}
		run(t, remote, "update-ref", "refs/dolt/data", oldOID)
		if err := moveGitRef(ctx, url, "refs/dolt/bd-reset-data-test", "refs/dolt/data", ""); err == nil {
			t.Error("moveGitRef replaced a data ref it expected to be absent")
		}
	})
}

// TestDeleteTempGitRefWorksOutsideAGitRepository pins the temporary-ref
// cleanup of the server-mode reset-data: it runs from a scratch repository,
// so a BEADS_DIR workspace outside git still removes the ref, and it still
// runs after the command's context was canceled (Ctrl-C).
func TestDeleteTempGitRefWorksOutsideAGitRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	forEachGitObjectFormat(t, func(t *testing.T, initFlags []string) {
		testDeleteTempGitRefOutsideAGitRepository(t, run, initFlags)
	})
}

func testDeleteTempGitRefOutsideAGitRepository(t *testing.T, run func(t *testing.T, dir string, args ...string) string, initFlags []string) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	run(t, base, append([]string{"init", "-q", "--bare"}, append(initFlags, remote)...)...)
	work := filepath.Join(base, "work")
	run(t, base, append([]string{"init", "-q"}, append(initFlags, work)...)...)
	run(t, work, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "0")
	const tmpRef = "refs/dolt/bd-reset-data-test"
	run(t, work, "push", "-q", remote, "HEAD:refs/dolt/data", "HEAD:"+tmpRef)

	outside := filepath.Join(base, "not-a-repo")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(outside)
	t.Setenv("GIT_CEILING_DIRECTORIES", base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := deleteTempGitRef(context.WithoutCancel(ctx), "file://"+remote, tmpRef); err != nil {
		t.Fatal(err)
	}
	refs := run(t, remote, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, tmpRef) {
		t.Errorf("temporary ref still on the remote: %q", refs)
	}
	if !strings.Contains(refs, "refs/dolt/data") {
		t.Errorf("data ref removed with the temporary ref: %q", refs)
	}
}

// fakeResetDataServerStore is the part of a server-mode store the server
// route of reset-data drives: a temporary remote and a push to it.
type fakeResetDataServerStore struct {
	storage.DoltStorage
	push func(remote string) error
}

func (f *fakeResetDataServerStore) AddRemoteWithRef(context.Context, string, string, string) error {
	return nil
}
func (f *fakeResetDataServerStore) RemoveRemote(context.Context, string) error { return nil }
func (f *fakeResetDataServerStore) PushRemote(_ context.Context, remote string, _ bool) error {
	return f.push(remote)
}

// TestResetThroughServerNamesATemporaryRefItCouldNotDelete pins the failure
// report of the server route: when the fresh-store push fails after writing
// the temporary ref and the ref cannot be deleted, the error names the ref
// and the command that deletes it, so a copy of the store is not left on the
// remote unreported. A push that failed before writing anything leaves no
// ref, and the error says nothing about one.
func TestResetThroughServerNamesATemporaryRefItCouldNotDelete(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only ref directory this test relies on")
	}
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	forEachGitObjectFormat(t, func(t *testing.T, initFlags []string) {
		testResetThroughServerLeftoverRef(t, run, initFlags)
	})
}

// resetDataTestRemote makes a bare git remote holding refs/dolt/data and a
// work repository to push to it from, both in the object format initFlags
// select.
func resetDataTestRemote(t *testing.T, run func(t *testing.T, dir string, args ...string) string, initFlags []string) (remote, work string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	run(t, base, append([]string{"init", "-q", "--bare"}, append(initFlags, remote)...)...)
	work = filepath.Join(base, "work")
	run(t, base, append([]string{"init", "-q"}, append(initFlags, work)...)...)
	run(t, work, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "0")
	run(t, work, "push", "-q", remote, "HEAD:refs/dolt/data")
	return remote, work
}

func testResetThroughServerLeftoverRef(t *testing.T, run func(t *testing.T, dir string, args ...string) string, initFlags []string) {
	setup := func(t *testing.T) (remote, work string) {
		return resetDataTestRemote(t, run, initFlags)
	}
	ctx := context.Background()
	pushErr := errors.New("push failed")

	t.Run("a written ref that cannot be deleted is named", func(t *testing.T) {
		remote, work := setup(t)
		refsDir := filepath.Join(remote, "refs", "dolt")
		st := &fakeResetDataServerStore{push: func(name string) error {
			run(t, work, "push", "-q", remote, "HEAD:refs/dolt/"+name)
			// The ref stays: its directory takes no deletion.
			if err := os.Chmod(refsDir, 0o555); err != nil {
				t.Fatal(err)
			}
			return pushErr
		}}
		t.Cleanup(func() { _ = os.Chmod(refsDir, 0o755) })
		_, err := resetGitBackedThroughServer(ctx, st, "origin", "git+file://"+remote, "")
		if !errors.Is(err, pushErr) {
			t.Fatalf("err = %v, want the push failure", err)
		}
		tmpRef := ""
		for _, ref := range strings.Fields(run(t, remote, "for-each-ref", "--format=%(refname)", "refs/dolt/")) {
			if strings.HasPrefix(ref, "refs/dolt/bd-reset-data-") {
				tmpRef = ref
			}
		}
		if tmpRef == "" {
			t.Fatal("precondition: the temporary ref was deleted")
		}
		for _, want := range []string{"the remote's Dolt data is unchanged", "the temporary ref " + tmpRef, "git push file://" + remote + " :" + tmpRef} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q:\n%v", want, err)
			}
		}
	})

	t.Run("a push that wrote nothing reports no leftover", func(t *testing.T) {
		remote, _ := setup(t)
		st := &fakeResetDataServerStore{push: func(string) error { return pushErr }}
		_, err := resetGitBackedThroughServer(ctx, st, "origin", "git+file://"+remote, "")
		if !errors.Is(err, pushErr) {
			t.Fatalf("err = %v, want the push failure", err)
		}
		if strings.Contains(err.Error(), "temporary ref") {
			t.Errorf("error names a temporary ref the push never wrote:\n%v", err)
		}
	})
}

// TestResetThroughServerReplacesTheDataRef pins the whole git side of the
// server route of reset-data on SHA-1 and SHA-256 remotes alike (Dolt
// supports both): the data ref ends at the commit the fresh-store push wrote
// to the temporary ref, and the temporary ref is gone. Scratch repositories
// of the wrong object format can neither fetch from the remote nor push to
// it.
func TestResetThroughServerReplacesTheDataRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	forEachGitObjectFormat(t, func(t *testing.T, initFlags []string) {
		remote, work := resetDataTestRemote(t, run, initFlags)
		fresh := ""
		st := &fakeResetDataServerStore{push: func(name string) error {
			run(t, work, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "fresh")
			run(t, work, "push", "-q", remote, "HEAD:refs/dolt/"+name)
			fresh = run(t, work, "rev-parse", "HEAD")
			return nil
		}}
		deleted, err := resetGitBackedThroughServer(context.Background(), st, "origin", "git+file://"+remote, "")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(deleted, []string{"refs/dolt/data"}) {
			t.Errorf("replaced refs = %v, want [refs/dolt/data]", deleted)
		}
		if got := run(t, remote, "rev-parse", "refs/dolt/data"); got != fresh {
			t.Errorf("refs/dolt/data = %s, want the fresh store %s", got, fresh)
		}
		if refs := run(t, remote, "for-each-ref", "--format=%(refname)", "refs/dolt/"); strings.Contains(refs, "bd-reset-data-") {
			t.Errorf("temporary ref left on the remote: %q", refs)
		}
	})
}

// forEachGitObjectFormat runs fn once for SHA-1 git repositories and once for
// SHA-256 ones, passing the git init flags that select the format. The
// SHA-256 leg is skipped where git lacks --object-format (before 2.29).
func forEachGitObjectFormat(t *testing.T, fn func(t *testing.T, initFlags []string)) {
	t.Helper()
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			var initFlags []string
			if format == "sha256" {
				initFlags = []string{"--object-format=sha256"}
				probe := filepath.Join(t.TempDir(), "probe.git")
				if out, err := exec.Command("git", "init", "-q", "--bare", "--object-format=sha256", probe).CombinedOutput(); err != nil {
					t.Skipf("git cannot create SHA-256 repositories (needs git 2.29 or later): %v: %s", err, strings.TrimSpace(string(out)))
				}
			}
			fn(t, initFlags)
		})
	}
}
