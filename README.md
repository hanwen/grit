'grit' = 'git' but new and unpolished.


TL;DR

* A writable FUSE filesystem that stores data in Git

* A command 'grit' that lets you do version control in a Grit tree

* Grit supports submodules natively

* Grit does not expose the .git directory, ensuring that users cannot break abstraction boundaries

* Grit needs go-fuse; it probably doesn't work on OSX


VISION

By storing all data in Git (and doing away with the worktree and
index), we can provide a filesystem-in-the-cloud that only needs a git
storage service.

By bypassing the Git command-line, we can control the entire user-experience. This opens possibilities such as:

* implementing "hg evolve" workflow
* Implementing the workflow as a web UI
* Specialized Gerrit support

By implementing submodule support natively, we can provide a first-class support for Gerrit topics.


EXAMPLE

To start the daemon,

```
git init --bare /tmp/g
mkdir /tmp/x
go run ./cmd/fuse/main.go -url https://gerrit.googlesource.com/gerrit -id 209c4dee0ecc2effea8259879b4b882eaf7c41bb -repo /tmp/gerrit/.git  /tmp/x
```

To interact with the checkout

```

# List top commit
go run ./cmd/grit/main.go -dir /tmp/x log

# List 3 commits, with their patches
go run ./cmd/grit/main.go -dir /tmp/x log -p -n 3

# List 3 commits of a submodule
go run ./cmd/grit/main.go -dir /tmp/x/modules/jgit log -n 3

# List files in tree
go run ./cmd/grit/main.go -dir /tmp/x ls-tree -r

# Checkout is writable
echo hoi > /tmp/x/modules/jgit/foobar
ls -l /tmp/x/modules/jgit/foobar

# Writes are automatically checkpointed
go run ./cmd/grit/main.go -dir /tmp/x/modules/jgit log -p

# A change in a submodule immediately affects the superproject
go run ./cmd/grit/main.go -dir /tmp/x log

# A second write amends the checkpointed commit
echo hoi > /tmp/x/modules/jgit/foobar2
go run ./cmd/grit/main.go -dir /tmp/x/modules/jgit log -p -n 2

# Amend the commit message
go run ./cmd/grit/main.go -dir /tmp/x/modules/jgit amend
```



