# Unattended review drain

`crq watch --dispatch` turns "this PR needs fixing" into a session that fixes it.
These are the files to run it as a service, and the prompt that survived contact
with it.

| file | what it is |
|---|---|
| `crq-drain` | wrapper that runs `crq watch --dispatch` with an agent and prompt |
| `crq-drain.service` | systemd --user unit; `Restart=always` + linger keeps it alive |
| `fix-prompt.txt` | what the fix session is told |

## The prompt earned two of its rules the hard way

**Stay detached.** crq's worktrees are backed by one mirror shared by every PR in
the repository. A session that ran `git checkout -B <branch>` put that branch in
the mirror, and git then refused to fetch it — for *every* PR. One session's
branch stopped every dispatch in the fleet for hours. Sessions commit on the
detached HEAD and push by ref instead:

```bash
git push origin "HEAD:refs/heads/$branch"
```

**Resolve after pushing, and expect to still be running.** A session's push moves
the head, which supersedes the round and drops its dispatch claim. crq used to
read that as "another watcher took this round" and kill the session — between
the push and the resolve, every time it succeeded. Only a claim somebody else
actively holds stops a session now.

## Where to look when it misbehaves

- `$CRQ_WORKSPACE/logs/<owner>/<name>/<pr>-<head>-<time>.log` — each session's own
  output, the last five per PR.
- `drain.err` — one line per dispatch, naming the session log.
- The dashboard issue and `crq status --line` — three failed passes in a row and
  both say `dispatch failing` rather than leaving it to be noticed.
