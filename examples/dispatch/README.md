# Unattended review drain

`crq watch --dispatch` turns "this PR needs fixing" into a session that fixes it.

```bash
crq drain install                 # prompt + wrapper + service, started
crq drain install --dry-run       # print what it would write first
```

That writes the fix prompt, a wrapper, and a systemd user unit (or a launchd
agent on macOS), enables lingering so it survives a logout, and starts it. There
are no files in this directory to copy: the prompt crq installs is embedded in
the binary, so it cannot drift from the one documented here.

## Two rules the prompt earned the hard way

**Sessions stay on a detached HEAD and push by ref.** crq's worktrees are backed
by one mirror shared by every PR in the repository. A session that ran
`git checkout -B <branch>` put that branch in the mirror, and git then refused to
fetch it — for *every* PR. One session's branch stopped every dispatch for hours.

```bash
git push "https://github.com/$head_repo.git" "HEAD:refs/heads/$branch"
```

The push names the repository the PR's branch lives in rather than `origin`.
crq's mirror is a clone of the *base* repository, so for a fork PR `origin` is
the wrong place: the commit would land on a same-named branch there and the PR
would never see it.

**Threads are resolved after pushing, and the session survives to do it.** A
push moves the head, which supersedes the round and drops its dispatch claim.
crq used to read that as "another watcher took this round" and kill the session
between the push and the resolve — every time it succeeded. Only a claim
somebody else actively holds stops a session now.

## Where to look when it misbehaves

- `$CRQ_WORKSPACE/logs/<owner>/<name>/<pr>-<head>-<time>.log` — each session's own
  output, the last five per PR. A failed session also keeps its worktree.
- `~/.local/state/crq/drain.err` — one line per dispatch, naming the session log.
- The dashboard issue and `crq status --line` — three passes in a row that start
  nothing and both say `dispatch failing`, rather than leaving it to be noticed.
