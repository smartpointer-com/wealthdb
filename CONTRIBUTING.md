# Contributing to wealthdb

Pull requests are welcome: a fix, a new report, a collector for another
bank. This page says what a pull request needs before it can be merged.

## Sign off every commit

wealthdb uses the [Developer Certificate of Origin](https://developercertificate.org)
(DCO), version 1.1. With it you certify that you wrote the change, or
otherwise have the right to submit it under the project's
[MIT license](LICENSE). Your contribution stays yours, under that license.

You certify it with one line at the end of each commit message, with
the same e-mail as the commit's author:

```
Signed-off-by: Ann Example <ann@example.com>
```

`git commit -s` adds the line for you. To add it to the commits a branch
already has, rebase the branch on wealthdb's `main`, then push it again:

```sh
git fetch https://github.com/smartpointer-com/wealthdb.git main
git rebase --signoff FETCH_HEAD
git push --force-with-lease
```

CI checks every commit a pull request adds. A commit without the line
fails the `dco` job, and the pull request cannot be merged until the job
passes. A merge commit is not checked.

## Before you open a pull request

- `make test` and `make lint` pass. `make hooks` installs the git hooks,
  which run the lint and the privacy check on every commit.
- The change holds no real data: no account numbers, balances, names or
  anything else a bank returned. [AGENTS.md](AGENTS.md) §4 lists what
  counts. Before a push, the privacy review in
  [.claude/skills/wealthdb-pii-review/SKILL.md](.claude/skills/wealthdb-pii-review/SKILL.md)
  reads the branch for what the check cannot see.
- A collector only reads ([AGENTS.md](AGENTS.md) §1). A new collector
  follows [NEW-COLLECTOR-PROMPT.md](NEW-COLLECTOR-PROMPT.md), and the
  contract in [collectors/README.md](collectors/README.md).

CI runs on a pull request from a fork once a maintainer approves the
run.

## Security problems

Report a security problem in private, as [SECURITY.md](SECURITY.md)
says, and not in a pull request or an issue.
