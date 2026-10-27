package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/githooksenv"
	"github.com/steveyegge/beads/internal/remedy"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/doltutil"
)

// Ref names anchoring a git-backed Dolt remote's data plane, as published by
// dolt's git blobstore (store/blobstore/git_refs.go: DoltDataRef and
// DefaultInfoBranch). Pinned locally so the binary does not import that
// package; TestResetDataRefNamesMatchDolt keeps the pin honest.
const (
	gitDoltDataRef = "refs/dolt/data"
	gitDoltInfoRef = "refs/heads/__dolt_remote_info__"
)

// resetDataKind classifies how reset-data can replace the data plane behind
// a remote URL.
type resetDataKind int

const (
	// resetDataGitBacked: rebuild a fresh store holding only live chunks and
	// replace the Dolt data refs on the git remote with it: in embedded mode
	// by deleting the refs and force-pushing, through a Dolt sql-server by
	// pushing to a temporary ref and moving the data ref to it
	// (resetGitBackedThroughServer).
	resetDataGitBacked resetDataKind = iota
	// resetDataFileStore: clear the native Dolt file-store directory, then
	// force-push to rebuild it.
	resetDataFileStore
	// resetDataFileAbsent: the file target is missing or empty — nothing
	// anchors old data; force-push alone rebuilds it.
	resetDataFileAbsent
	// resetDataUnsupported: a cloud or hosted store whose contents bd cannot
	// safely clear — replace the remote with a fresh URL/prefix instead.
	resetDataUnsupported
)

// classifyResetDataRemote decides which reset-data mechanism applies to url.
// File URLs are classified by what is actually on disk, because a file://
// remote can be either a bare git repository (Dolt normalizes those to
// git+file:// at push time) or a native Dolt file store: a bare git repo
// takes the git-backed path, a directory with an nbs manifest is a file
// store, and a missing or empty target has nothing to reset. A non-empty
// target that is neither is an error — reset-data must not clear a
// directory it cannot identify.
func classifyResetDataRemote(url string) (resetDataKind, error) {
	if doltutil.IsGitProtocolURL(url) {
		return resetDataGitBacked, nil
	}
	path, isFile := resetDataFilePath(url)
	if !isFile {
		return resetDataUnsupported, nil
	}
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return resetDataFileAbsent, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspecting remote target %s: %w", path, err)
	}
	if len(entries) == 0 {
		return resetDataFileAbsent, nil
	}
	if isBareGitRepoDir(path) {
		return resetDataGitBacked, nil
	}
	if _, err := os.Stat(filepath.Join(path, "manifest")); err == nil {
		return resetDataFileStore, nil
	}
	return 0, fmt.Errorf("remote target %s is non-empty but is neither a bare git repository nor a Dolt file store (no manifest); refusing to clear it", path)
}

// resetDataFilePath extracts the local filesystem path from a file:// URL or
// a bare absolute path, reporting whether url is file-backed at all.
func resetDataFilePath(url string) (string, bool) {
	if strings.HasPrefix(url, "file://") {
		return strings.TrimPrefix(url, "file://"), true
	}
	if filepath.IsAbs(url) {
		return url, true
	}
	return "", false
}

// isBareGitRepoDir reports whether dir looks like a bare git repository
// (HEAD file plus an objects directory).
func isBareGitRepoDir(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && fi.IsDir()
}

// resetDataGitURL converts a Dolt git-protocol remote URL to the form the
// git CLI accepts (strip the git+ prefix; file paths gain file://).
func resetDataGitURL(url string) string {
	if trimmed := strings.TrimPrefix(url, "git+"); trimmed != url {
		return trimmed
	}
	if path, isFile := resetDataFilePath(url); isFile && !strings.HasPrefix(url, "file://") {
		return "file://" + path
	}
	return url
}

// lsRemoteDoltDataRefs returns which of the Dolt data-plane refs currently
// exist on the git remote at gitURL.
func lsRemoteDoltDataRefs(ctx context.Context, gitURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", gitURL, gitDoltDataRef, gitDoltInfoRef) // #nosec G204 -- URL from configured remote
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %s failed: %s: %w", gitURL, strings.TrimSpace(string(out)), err)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs = append(refs, fields[1])
		}
	}
	return refs, nil
}

// deleteGitDoltDataRefs deletes refs on the git remote at gitURL. Git
// client-side hooks are disabled for the push, same as bd's other internal
// git invocations (GH#3724 class: a user's templated pre-push hook must not
// break — or observe — bd's data-plane plumbing).
func deleteGitDoltDataRefs(ctx context.Context, gitURL string, refs []string) error {
	args := []string{"push", gitURL}
	for _, ref := range refs {
		args = append(args, ":"+ref)
	}
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- URL from configured remote, fixed refspecs
	cmd.Env = envWithNoGitHooks()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push (delete %s) failed: %s: %w", strings.Join(refs, ", "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// envWithNoGitHooks returns the current environment with git client-side
// hooks disabled via GIT_CONFIG_PARAMETERS, preserving any parameters the
// caller already set (mirrors applyNoGitHooksToCmd in internal/storage/dolt).
func envWithNoGitHooks() []string {
	return githooksenv.DisabledEnv(os.Environ())
}

// clearDoltFileStore removes the contents of a native Dolt file-store
// directory, keeping the directory itself so the follow-up push can rebuild
// the store in place. The caller has already verified a manifest is present.
func clearDoltFileStore(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading remote store %s: %w", dir, err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("clearing remote store %s: %w", dir, err)
		}
	}
	return nil
}

var doltRemoteResetDataYes bool

var doltRemoteResetDataCmd = &cobra.Command{
	Use:   "reset-data <name>",
	Short: "Replace a remote's data plane in place after a history squash",
	Long: `Replace a Dolt remote's stored data with a fresh copy of local HEAD.

After a history squash (see the History Bloat recovery runbook), a plain
'bd dolt push --force' re-points the remote's refs but deletes nothing:
Dolt remotes accumulate chunks monotonically, so the remote keeps the full
pre-squash store. This command rebuilds the remote's data plane so it holds
only live chunks:

  - Git-backed remotes (issue data riding a git remote under refs/dolt/data):
    rebuilds a fresh store from local HEAD and replaces the remote's Dolt
    data refs with it (through a Dolt sql-server it pushes the fresh store
    to a temporary ref first, so the remote keeps its data until the new
    store is complete, then restarts bd's own server so that it rereads
    the remote; any other server must be restarted before its next push or
    pull of the remote). Code branches are untouched.
  - Native file remotes (file:// paths): clears the store directory, then
    force-pushes to rebuild it.
  - Cloud/hosted remotes (aws://, gs://, dolthub://, ...): bd cannot clear
    the stored data safely — replace the remote with a fresh URL or prefix:
      bd dolt remote remove <name>
      bd dolt remote add <name> <fresh-url>
      bd dolt push --force

This rewrites the remote's data plane. Every other clone must re-clone from
the reset remote (that is already true after the squash itself). Refuses to
run with uncommitted working-set changes: the rebuilt remote holds exactly
HEAD, and anything uncommitted would not be part of it.

Examples:
  bd dolt remote reset-data origin          # prompts for confirmation
  bd dolt remote reset-data origin --yes    # no prompt (scripts, agents)`,
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("dolt remote reset-data")
		name := args[0]

		if usesProxiedServer() {
			return HandleErrorRespectJSON("bd dolt remote reset-data is not supported over a proxied bd server; run it from the workspace that hosts the Dolt database")
		}
		if isDoltLocalOnly() {
			fmt.Fprintln(os.Stderr, "Error: cannot reset remote data: remote sync is disabled (dolt.local-only=true).")
			fmt.Fprintln(os.Stderr, "To re-enable remote sync: bd config unset dolt.local-only")
			return SilentExit()
		}

		ctx := rootCtx
		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}

		remotes, err := st.ListRemotes(ctx)
		if err != nil {
			return HandleError("listing remotes: %v", err)
		}
		var url, ref string
		for _, r := range remotes {
			if r.Name == name {
				url, ref = r.URL, r.Ref
				break
			}
		}
		if url == "" {
			return HandleErrorWithHint(
				fmt.Sprintf("remote %q is not configured", name),
				"Use 'bd dolt remote list' to see configured remotes.")
		}

		// The rebuilt remote holds exactly HEAD; refuse while anything
		// uncommitted would be silently left out of it.
		if det, ok := storage.UnwrapStore(st).(storage.PendingChangeDetector); ok {
			dirty, derr := det.HasCommittablePending(ctx)
			if derr != nil {
				return HandleError("checking working set: %v", derr)
			}
			if dirty {
				return HandleErrorWithHint(
					"working set has uncommitted changes; refusing to reset remote data",
					"Run 'bd dolt commit' first — the rebuilt remote holds exactly HEAD.")
			}
		}

		kind, err := classifyResetDataRemote(url)
		if err != nil {
			return HandleError("%v", err)
		}
		if kind == resetDataUnsupported {
			// The URL is the remote's, which a pull can carry; the name goes
			// into the commands only as one shell word.
			remote, note := remedy.CommandArg(runtime.GOOS, name, "<remote>")
			return HandleErrorWithHint(
				fmt.Sprintf("cannot clear stored data on remote %q (%s)", name, remedy.PrintedValue(url)),
				fmt.Sprintf("Replace the remote with a fresh URL or prefix instead:\n"+
					"  bd dolt remote remove %s\n"+
					"  bd dolt remote add %s <fresh-url>\n"+
					"  bd dolt push --force%s", remote, remote, note))
		}

		if !doltRemoteResetDataYes {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return HandleErrorWithHint(
					fmt.Sprintf("reset-data replaces all Dolt data stored on remote %q (%s)", name, url),
					"Re-run with --yes to confirm.")
			}
			fmt.Printf("This replaces all Dolt data stored on remote %q:\n", name)
			fmt.Printf("  %s\n", url)
			fmt.Println("The remote is rebuilt from local HEAD, including any pending changes; other clones must re-clone.")
			fmt.Print("Proceed? (y/N): ")
			reader := bufio.NewReader(os.Stdin)
			response, rerr := reader.ReadString('\n')
			if rerr != nil {
				return HandleError("reading confirmation: %v", rerr)
			}
			response = strings.TrimSpace(strings.ToLower(response))
			if response != "y" && response != "yes" {
				fmt.Println("Canceled.")
				return nil
			}
		}

		var deletedRefs []string
		var clearedStore bool
		// Only a git+ URL is a git remote to Dolt (a file:// URL to a bare
		// git directory is a native file store to it, which takes no data
		// ref), so only that takes the temporary-ref route; the rest keep
		// the flow below.
		if kind == resetDataGitBacked && !isEmbeddedStore(st) && doltutil.IsGitProtocolURL(url) {
			replaced, rerr := resetGitBackedThroughServer(ctx, st, name, url, ref)
			if rerr != nil {
				return HandleError("%v", rerr)
			}
			return reportResetData(name, url, replaced, false, refreshServerAfterReset(st, name))
		}
		switch kind {
		case resetDataGitBacked:
			gitURL := resetDataGitURL(url)
			refs, lerr := lsRemoteDoltDataRefs(ctx, gitURL)
			if lerr != nil {
				return HandleError("%v", lerr)
			}
			if len(refs) == 0 {
				fmt.Println("No Dolt data refs found on the remote (already reset?); pushing fresh store.")
			} else {
				if derr := deleteGitDoltDataRefs(ctx, gitURL, refs); derr != nil {
					return HandleError("%v", derr)
				}
				deletedRefs = refs
				fmt.Printf("Deleted %s on %s\n", strings.Join(refs, ", "), gitURL)
			}
		case resetDataFileStore:
			path, _ := resetDataFilePath(url)
			if cerr := clearDoltFileStore(path); cerr != nil {
				return HandleError("%v", cerr)
			}
			clearedStore = true
			fmt.Printf("Cleared remote store %s\n", path)
		case resetDataFileAbsent:
			fmt.Println("Remote store is empty or absent; pushing fresh store.")
		}

		fmt.Printf("Force-pushing to remote %q...\n", name)
		if perr := st.PushRemote(ctx, name, true); perr != nil {
			remote, note := remedy.CommandArg(runtime.GOOS, name, "<remote>")
			return HandleError("push after reset failed (the remote's data refs were already removed — re-run `bd dolt push --force --remote %s`%s once the cause is fixed): %v", remote, note, perr)
		}

		return reportResetData(name, url, deletedRefs, clearedStore, nil)
	},
}

// resetDataServerRefresh is what a reset through a Dolt sql-server did about
// the server's view of the remote: restarted bd's own server, or names the
// restart the server needs.
type resetDataServerRefresh struct {
	restarted bool
	// required is the server-restart notice, set when the server still
	// holds its pre-reset view of the remote.
	required string
}

// refreshServerAfterReset makes the Dolt server drop its view of remote
// after resetGitBackedThroughServer moved the remote's data ref under it. A
// sql-server keeps one store per git remote URL and ref for its whole life,
// and that store's cache of the remote's table files only grows: after the
// move it still takes table files of the old store, which the fresh one
// lacks, as present on the remote, so a later push through it can write a
// manifest naming files the remote does not hold, or report a branch as
// up to date that the remote no longer has. Only a restart clears it. bd
// restarts the server when it is the one bd manages for this workspace
// (ManagedServerDir: the dialed port is the tracked server's), keeping its
// port; any other server is the operator's to restart, which the report
// says.
func refreshServerAfterReset(st storage.DoltStorage, remote string) *resetDataServerRefresh {
	needed := fmt.Sprintf("restart the Dolt server before the next push or pull of remote %s: it still holds its pre-reset view of the remote", remedy.PrintedValue(remote))
	m, ok := storage.UnwrapStore(st).(interface{ ManagedServerDir() (string, bool) })
	if !ok {
		return &resetDataServerRefresh{required: needed}
	}
	serverDir, managed := m.ManagedServerDir()
	if !managed {
		return &resetDataServerRefresh{required: needed}
	}
	if _, err := doltserver.Restart(serverDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: restarting the Dolt server after the reset failed: %v\n", err)
		return &resetDataServerRefresh{required: needed}
	}
	return &resetDataServerRefresh{restarted: true}
}

// reportResetData prints reset-data's success report. refresh is nil for a
// reset that ran no push through a Dolt sql-server's temporary remote.
func reportResetData(name, url string, deletedRefs []string, clearedStore bool, refresh *resetDataServerRefresh) error {
	if jsonOutput {
		out := map[string]interface{}{
			"remote":        name,
			"url":           url,
			"deleted_refs":  deletedRefs,
			"cleared_store": clearedStore,
			"pushed":        true,
		}
		if refresh != nil {
			out["server_restarted"] = refresh.restarted
			out["server_restart_required"] = refresh.required != ""
			if refresh.required != "" {
				out["warning"] = refresh.required
			}
		}
		return outputJSON(out)
	}
	fmt.Printf("✓ Remote %q data plane reset; store rebuilt from HEAD.\n", name)
	if refresh != nil && refresh.restarted {
		fmt.Println("  Restarted bd's Dolt server so that it rereads the remote.")
	}
	if refresh != nil && refresh.required != "" {
		fmt.Printf("  Warning: %s.\n", refresh.required)
	}
	fmt.Println("  Other clones of this database must re-clone from the remote.")
	return nil
}

// resetGitBackedThroughServer rebuilds a git-backed remote's Dolt data plane
// for a store whose pushes run in a Dolt sql-server, without ever leaving the
// remote without its data. A sql-server keeps the store of every git remote
// it has pushed to or pulled from for its whole life, and once the remote's
// data ref is deleted under it, that store still holds the old manifest: a
// force push to the remote then either reports success having written
// nothing or never finishes, until the server restarts. So this pushes a
// fresh store to a ref of its own (a remote the server has not opened yet),
// then moves the data ref to it with git, from the value it read first, and
// removes the temporary ref and remote. Until the move the remote keeps its
// old data; the move replaces it in one ref update. The server's store for
// the remote itself is still the pre-reset one afterwards, which the caller
// clears (refreshServerAfterReset).
func resetGitBackedThroughServer(ctx context.Context, st storage.DoltStorage, name, url, ref string) ([]string, error) {
	gitURL := resetDataGitURL(url)
	dataRef := storage.EffectiveGitDataRef(ref)
	before, err := lsRemoteRefOID(ctx, gitURL, dataRef)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("naming the temporary remote: %w", err)
	}
	tmpName := "bd-reset-data-" + hex.EncodeToString(nonce)
	tmpRef := "refs/dolt/" + tmpName
	remote, note := remedy.CommandArg(runtime.GOOS, name, "<remote>")
	rerun := "`bd dolt remote reset-data " + remote + " --yes`" + note
	if err := st.AddRemoteWithRef(ctx, tmpName, url, tmpRef); err != nil {
		return nil, fmt.Errorf("adding the temporary remote %s on the Dolt server failed; the remote's Dolt data is unchanged (re-run %s once the cause is fixed): %w", tmpName, rerun, err)
	}
	// Cleanup runs even after Ctrl-C canceled ctx, so an interrupted reset
	// does not leave the temporary remote or ref behind.
	cleanupCtx := context.WithoutCancel(ctx)
	defer func() {
		rctx, cancel := context.WithTimeout(cleanupCtx, 60*time.Second)
		defer cancel()
		if err := st.RemoveRemote(rctx, tmpName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not remove the temporary remote %s from the Dolt server (bd dolt remote remove %s): %v\n", tmpName, tmpName, err)
		}
	}()
	// leftover names the temporary ref a failed delete left on the remote,
	// with the command that deletes it.
	leftover := func(err error) string {
		u, unote := remedy.CommandArg(runtime.GOOS, gitURL, "<url>")
		return fmt.Sprintf("the temporary ref %s holding a copy of the fresh store remains on the remote; delete it with `git push %s :%s`%s: %v", tmpRef, u, tmpRef, unote, err)
	}
	// failWith deletes the temporary ref after a failed step and returns
	// failure, naming the ref when it could not be deleted. A push that
	// failed before writing it leaves no ref, which is no leftover.
	failWith := func(failure error) ([]string, error) {
		cerr := deleteTempGitRef(cleanupCtx, gitURL, tmpRef)
		if cerr == nil {
			return nil, failure
		}
		if oid, lerr := lsRemoteRefOID(cleanupCtx, gitURL, tmpRef); lerr == nil && oid == "" {
			return nil, failure
		}
		return nil, fmt.Errorf("%w; %s", failure, leftover(cerr))
	}
	fmt.Printf("Pushing a fresh store to %s on %s...\n", tmpRef, gitURL)
	if err := st.PushRemote(ctx, tmpName, true); err != nil {
		return failWith(fmt.Errorf("pushing a fresh store failed; the remote's Dolt data is unchanged (re-run %s once the cause is fixed): %w", rerun, err))
	}
	if err := moveGitRef(ctx, gitURL, tmpRef, dataRef, before); err != nil {
		return failWith(fmt.Errorf("replacing %s with the fresh store failed; the remote's Dolt data is unchanged (re-run %s once the cause is fixed): %w", dataRef, rerun, err))
	}
	if err := deleteTempGitRef(cleanupCtx, gitURL, tmpRef); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", leftover(err))
	}
	fmt.Printf("Replaced %s on %s with the fresh store\n", dataRef, gitURL)
	if before == "" {
		return nil, nil
	}
	return []string{dataRef}, nil
}

// deleteTempGitRef deletes ref on the git remote at gitURL from a scratch
// repository, so it does not depend on bd's working directory being inside a
// git repository (a BEADS_DIR workspace need not be). Hooks are disabled as
// for deleteGitDoltDataRefs, and the push is bounded so cleanup cannot hang.
// A ref the remote does not hold is nothing to delete.
func deleteTempGitRef(ctx context.Context, gitURL, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// The ref's object ID gives the remote's object format, which the
	// scratch repository must share for git to push to it. An absent ref
	// has nothing to delete.
	oid, err := lsRemoteRefOID(ctx, gitURL, ref)
	if err != nil {
		return err
	}
	if oid == "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "bd-reset-data-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", dir}, args...)...) // #nosec G204 -- URL from configured remote, fixed refspec
		cmd.Env = envWithNoGitHooks()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s (delete %s) failed: %s: %w", args[0], ref, strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	if err := git(scratchGitInitArgs(oid)...); err != nil {
		return err
	}
	return git("push", "-q", gitURL, ":"+ref)
}

// scratchGitInitArgs returns the git init arguments for a scratch
// repository that exchanges objects with a remote holding oid. git fetches
// and pushes only between repositories of one object format, so a SHA-256
// remote (64-hex object IDs) needs --object-format=sha256, as Dolt's own
// cache repository for the remote gets (dbfactory remoteInitFlags). A SHA-1
// remote gets no flag, so git older than 2.29, which lacks it, still works.
func scratchGitInitArgs(oid string) []string {
	args := []string{"init", "--bare", "-q"}
	if len(oid) == 64 {
		args = append(args, "--object-format=sha256")
	}
	return args
}

// lsRemoteRefOID returns the object ID ref names on the git remote at gitURL,
// or "" when the remote has no such ref.
func lsRemoteRefOID(ctx context.Context, gitURL, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "ls-remote", gitURL, ref).CombinedOutput() // #nosec G204 -- URL from configured remote
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s failed: %s: %w", gitURL, strings.TrimSpace(string(out)), err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", nil
}

// moveGitRef points ref on the git remote at gitURL at the commit src names
// there, provided ref still holds before ("" for absent). git pushes only
// from objects it has, so it fetches src's commit into a scratch repository
// of the remote's object format first: with a filter where the remote
// supports one, else the whole commit, which the push then does not send
// back.
func moveGitRef(ctx context.Context, gitURL, src, ref, before string) error {
	srcOID, err := lsRemoteRefOID(ctx, gitURL, src)
	if err != nil {
		return err
	}
	if srcOID == "" {
		return fmt.Errorf("%s is not on the remote", src)
	}
	dir, err := os.MkdirTemp("", "bd-reset-data-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", dir}, args...)...) // #nosec G204 -- URL from configured remote, fixed refspecs
		cmd.Env = envWithNoGitHooks()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s failed: %s: %w", args[0], strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	if err := git(scratchGitInitArgs(srcOID)...); err != nil {
		return err
	}
	if err := git("fetch", "-q", "--no-tags", "--depth=1", "--filter=tree:0", gitURL, "+"+src+":refs/bd-reset-data/new"); err != nil {
		return err
	}
	return git("push", "-q", "--force-with-lease="+ref+":"+before, gitURL, "refs/bd-reset-data/new:"+ref)
}
